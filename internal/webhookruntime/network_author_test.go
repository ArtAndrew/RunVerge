// SPDX-License-Identifier: Elastic-2.0
package webhookruntime

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

// This backend deliberately proves only transport boundaries. Actual durable
// marker/receipt/COMMIT/crash evidence is in the separate storage tests.
type webhookNetworkStore struct{}

func (webhookNetworkStore) PrepareWebhookDispatch(_ context.Context, l *domain.WebhookLease) (domain.WebhookDispatchPermit, error) {
	now := time.Now()
	return domain.WebhookDispatchPermit{OrganizationID: l.OrganizationID, TaskID: l.TaskID, SubscriptionID: l.SubscriptionID, DeliveryID: l.DeliveryID, AttemptID: l.AttemptID, ProfileDigest: l.ProfileDigest, EnvelopeDigest: l.EnvelopeDigest, WorkerID: l.WorkerID, Token: l.Token, IssuedAt: now, NotAfter: now.Add(2 * time.Second), ValidFor: 2 * time.Second, SignatureTimestamp: now.Unix()}, nil
}
func (webhookNetworkStore) RecordWebhookObservation(_ context.Context, in domain.WebhookObservation) (domain.WebhookDelivery, error) {
	status := "unknown"
	if in.HTTPStatus >= 200 && in.HTTPStatus <= 299 {
		status = "acknowledged"
	} else if in.HTTPStatus != 0 {
		status = "not_acknowledged"
	}
	return domain.WebhookDelivery{ID: in.DeliveryID, Status: status}, nil
}

func webhookAuthorDNS(t *testing.T, ips []netip.Addr) *atomic.Int64 {
	t.Helper()
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, peer, err := socket.ReadFrom(buf)
			if err != nil {
				return
			}
			query := append([]byte{}, buf[:n]...)
			if len(query) < 17 {
				continue
			}
			end := 12
			for end < len(query) && query[end] != 0 {
				end += int(query[end]) + 1
			}
			if end+5 > len(query) {
				continue
			}
			end++
			kind := binary.BigEndian.Uint16(query[end : end+2])
			questionEnd := end + 4
			answer := append([]byte{}, query[:questionEnd]...)
			binary.BigEndian.PutUint16(answer[2:4], 0x8180)
			binary.BigEndian.PutUint16(answer[4:6], 1)
			binary.BigEndian.PutUint16(answer[6:8], 0)
			binary.BigEndian.PutUint16(answer[8:10], 0)
			binary.BigEndian.PutUint16(answer[10:12], 0)
			count := uint16(0)
			for _, ip := range ips {
				if kind != 1 && kind != 28 || kind == 1 && !ip.Is4() || kind == 28 && ip.Is4() {
					continue
				}
				raw := ip.AsSlice()
				record := []byte{0xc0, 0x0c, 0, 0, 0, 1, 0, 0, 0, 30, 0, 0}
				binary.BigEndian.PutUint16(record[2:4], kind)
				binary.BigEndian.PutUint16(record[10:12], uint16(len(raw)))
				answer = append(answer, record...)
				answer = append(answer, raw...)
				count++
			}
			binary.BigEndian.PutUint16(answer[6:8], count)
			calls.Add(1)
			_, _ = socket.WriteTo(answer, peer)
		}
	}()
	old := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", socket.LocalAddr().String())
	}}
	t.Cleanup(func() { net.DefaultResolver = old; _ = socket.Close(); <-done })
	return calls
}

func webhookAuthorNamedTLS(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "webhook.receiver.test"}, DNSNames: []string{"webhook.receiver.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	s := httptest.NewUnstartedServer(handler)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestWebhookNetworkAuthorAllDNSAnswersAndProxyBoundary(t *testing.T) {
	for _, mode := range []string{"allowed", "mixed_ipv4_metadata", "mixed_ipv6_metadata"} {
		t.Run(mode, func(t *testing.T) {
			var requests, proxyRequests atomic.Int64
			peer, ca := webhookAuthorNamedTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.TLS.ServerName != "webhook.receiver.test" || r.ProtoMajor != 1 {
					t.Error("SNI/HTTP1 identity mismatch")
				}
				w.WriteHeader(204)
			}))
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxyRequests.Add(1); w.WriteHeader(502) }))
			t.Cleanup(proxy.Close)
			t.Setenv("HTTPS_PROXY", proxy.URL)
			t.Setenv("HTTP_PROXY", proxy.URL)
			t.Setenv("NO_PROXY", "")
			t.Setenv("https_proxy", proxy.URL)
			t.Setenv("no_proxy", "")
			u, _ := url.Parse(peer.URL)
			endpoint := "https://webhook.receiver.test:" + u.Port() + "/events"
			ips := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
			if mode == "mixed_ipv4_metadata" {
				ips = append(ips, netip.MustParseAddr("169.254.169.254"))
			}
			if mode == "mixed_ipv6_metadata" {
				ips = append(ips, netip.MustParseAddr("fd00:ec2::254"))
			}
			dnsCalls := webhookAuthorDNS(t, ips)
			profile := domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "receiver", OrganizationID: "org_test", Version: 1, Name: "Named TLS receiver", SigningKeyID: "network_key", UnmeteredApproved: true, Active: true}, Endpoint: endpoint, SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_NETWORK", TrustedCAPEM: ca, AllowPrivateNetwork: true, TimeoutSeconds: 2}
			r, err := New(webhookNetworkStore{}, Config{Profile: profile, EncodedSigningKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), PersistenceTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"delivery_id":"delivery_test"}`)
			l := &domain.WebhookLease{OrganizationID: "org_test", TaskID: "task_test", SubscriptionID: "sub_test", DeliveryID: "delivery_test", AttemptID: "attempt_test", DestinationID: "receiver", DestinationVersion: 1, ProfileDigest: ProfileDigest(profile), EnvelopeDigest: digest(body), WorkerID: "worker_test", Token: 1, LeaseUntil: time.Now().Add(time.Minute), Envelope: body}
			d, err := r.Execute(context.Background(), l)
			if dnsCalls.Load() == 0 {
				t.Fatal("controlled DNS was not consulted")
			}
			if mode == "allowed" {
				if err != nil || d.Status != "acknowledged" || requests.Load() != 1 {
					t.Fatal("named TLS/pinned DNS request failed", err, d.Status, requests.Load())
				}
			} else {
				if err == nil || d.Status != "unknown" || requests.Load() != 0 {
					t.Fatal("a forbidden secondary DNS answer was ignored", err, requests.Load())
				}
			}
			if proxyRequests.Load() != 0 {
				t.Fatal("ambient proxy received webhook request/signature")
			}
		})
	}
}
