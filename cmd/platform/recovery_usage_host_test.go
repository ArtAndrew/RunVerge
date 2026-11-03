// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/storage"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const recoveryHostUsagePeerKey = "synthetic-owned-usage-peer-key"
const recoveryHostUsagePeerBody = `{"data":{"id":"gen-usage-selected","model":"example/usage-model","total_cost":0.0000001,"is_byok":true}}`

type recoveryHostUsageFixture struct {
	selection storage.RecoveryUsageSelection
}

func TestRecoveryHostAuthorUsageConsentBeforeFiles(t *testing.T) {
	for _, args := range [][]string{
		{"usage-acquire", "--intent-file", "/not-read", "--authority-dir", "/not-read"},
		{"usage-access-init", "--authority-dir", "/not-read", "--intent-file", "/not-read", "--key-file", "/not-read"},
		{"usage-access-revoke", "--authority-dir", "/not-read", "--expected-epoch", "1", "--operator-reference", "test"},
	} {
		if err := runRecoveryHost(context.Background(), args, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "usage:") {
			t.Fatal("missing consent reached files", args[0], err)
		}
	}
}

func TestRecoveryHostAuthorUsageRoundTrip(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_USAGE_HTTPS") != "1" {
		t.Skip("owned HTTPS environment required")
	}
	recoveryHostArchiveRoundTrip(t, true, false, true)
}

func prepareRecoveryHostUsageFixture(t *testing.T, ctx context.Context, s *storage.Store) *recoveryHostUsageFixture {
	t.Helper()
	b, err := s.Bootstrap(ctx, "owned usage CLI fixture", 1000000)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	c := domain.ModelConnection{ID: "usage-router", OrganizationID: p.OrganizationID, Version: 1, WireProfile: domain.ModelWireOpenRouterTextV1, Model: "example/usage-model", ProviderAccount: "synthetic-provider-account", PriceVersion: "synthetic-v1", Endpoint: modelgateway.OpenRouterEndpoint, CredentialEnvironment: "PLATFORM_MODEL_KEY_USAGE_FIXTURE", MaxChargeMicroUSD: 37, MaxInputTokens: 32, InputMicroUSDPerMillion: 1000000, MaxOutputTokens: 16, MaxInputBytes: 4096, MaxOutputBytes: 4096, TimeoutMilliseconds: 2000, TokenLimitField: "max_tokens", BoundAssumptions: "Synthetic history only; no model request", Active: true}
	c, err = s.InstallModelConnection(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Synthetic usage source", Instructions: "Return synthetic draft", Model: c.Model, BudgetMicroUSD: &budget})
	var a domain.Agent
	if err != nil || json.Unmarshal(raw, &a) != nil {
		t.Fatal("usage agent", err)
	}
	raw, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version)
	if err != nil || json.Unmarshal(raw, &a) != nil {
		t.Fatal("usage publish", err)
	}
	if _, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{Goal: "Owned usage history", AgentID: a.ID, BudgetMicroUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	l, _, err := s.ClaimModel(ctx, "usage-host-worker", 2*time.Minute)
	if err != nil || l == nil {
		t.Fatal("usage claim", err)
	}
	r, err := modelruntime.New(s, storage.RuntimeModelConfig(c, "synthetic-unused-key"))
	if err != nil {
		t.Fatal(err)
	}
	charge, intent, err := r.DescribeIntent(*l)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := s.ReserveCost(ctx, l, domain.ReserveCostRequest{ChargeKey: charge, IntentHash: intent, Category: "model", Provider: c.ProviderAccount, PriceVersion: c.PriceVersion, AmountMicroUSD: c.MaxChargeMicroUSD})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareModelDispatch(ctx, l, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkCostUnknown(ctx, p.OrganizationID, reservation.ID, domain.NewID("unknown"), "synthetic request, no external call"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteModelCandidate(ctx, l, reservation.ID, domain.ModelCandidate{Text: "Synthetic candidate", ProviderRequestID: "gen-usage-selected", Model: c.Model, PriceVersion: c.PriceVersion, IntentDigest: intent, InputTokens: 3, OutputTokens: 2, EstimatedMicroUSD: 3}); err != nil {
		t.Fatal(err)
	}
	return &recoveryHostUsageFixture{storage.RecoveryUsageSelection{OrganizationID: p.OrganizationID, ReservationID: reservation.ID}}
}

func recoveryHostUsagePlan(t *testing.T, ctx context.Context, f *recoveryHostUsageFixture, source, target, path string) string {
	t.Helper()
	args := []string{"usage-plan", "--source-intent-file", source, "--target-config", target, "--organization-id", f.selection.OrganizationID, "--reservation-id", f.selection.ReservationID, "--out", path}
	if err := runRecoveryHost(ctx, args, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("durable usage plan after stdout loss", err)
	}
	var saved recoveryUsageFile
	if readRecoveryFenceJSON(path, &saved) != nil || !recoveryUsageSavedValid(saved) || saved.Plan.State != "request_identity_available" {
		t.Fatal("invalid durable usage plan")
	}
	return path
}

func recoveryHostUsageRoundTrip(t *testing.T, ctx context.Context, f *recoveryHostUsageFixture, source, target, parent, stale string) {
	t.Helper()
	calls := recoveryHostUsageOwnedHTTPS(t)
	path := recoveryHostUsagePlan(t, ctx, f, source, target, filepath.Join(parent, "usage-plan.json"))
	authority := filepath.Join(parent, "usage-authority")
	key := filepath.Join(parent, "usage-key")
	if os.WriteFile(key, []byte(recoveryHostUsagePeerKey+"\n"), 0600) != nil {
		t.Fatal("private key")
	}
	configure := []string{"usage-access-init", "--authority-dir", authority, "--intent-file", path, "--key-file", key, "--credential-reference", "owned-key", "--operator-reference", "owned:account", "--expires-at", time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "--confirm-current-account"}
	if err := runRecoveryHost(ctx, configure, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("new authority and stdout loss", err)
	}
	checkAuthority := func(epoch uint64, enabled bool) {
		raw, err := recoveryHostCommand(t, ctx, "usage-access-inspect", "--authority-dir", authority)
		var i recoveryaccess.HostInspection
		if err != nil || json.Unmarshal(raw, &i) != nil || i.Current.Epoch != epoch || i.Current.Usage == nil || i.Current.Usage.Enabled != enabled || strings.Contains(string(raw), recoveryHostUsagePeerKey) {
			t.Fatal("authority inspection", err)
		}
	}
	checkAuthority(1, true)
	acquire := func(p string) []string {
		return []string{"usage-acquire", "--intent-file", p, "--authority-dir", authority, "--confirm-read-provider-usage"}
	}
	if err := runRecoveryHost(ctx, acquire(stale), io.Discard); !errors.Is(err, storage.ErrRecoveryUsageBinding) || calls.Load() != 0 {
		t.Fatal("changed accounting head reached HTTP", err)
	}
	if _, err := recoveryHostCommand(t, ctx, acquire(stale)...); err == nil || calls.Load() != 0 {
		t.Fatal("stale attempt repeated")
	}
	// Block receipt persistence after the provider response. Admission must not run.
	blocked := recoveryHostUsagePlan(t, ctx, f, source, target, filepath.Join(parent, "blocked-usage.json"))
	recoveryHostPrivateJSON(t, blocked+".acquisition", map[string]string{"sentinel": "preserve"})
	if _, err := recoveryHostCommand(t, ctx, acquire(blocked)...); err == nil || calls.Load() != 1 {
		t.Fatal("receipt write failure", err)
	}
	var blockedSaved recoveryUsageFile
	if readRecoveryFenceJSON(blocked, &blockedSaved) != nil {
		t.Fatal("blocked plan")
	}
	var targetConfig recoveryHostTarget
	if readPrivateJSON(target, &targetConfig) != nil {
		t.Fatal("target")
	}
	db, err := storage.Open(ctx, targetConfig.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM recovery_usage_observations WHERE id=$1", blockedSaved.OperationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed receipt entered database", err)
	}
	if _, err = recoveryHostCommand(t, ctx, acquire(blocked)...); err == nil || calls.Load() != 1 {
		t.Fatal("failed retention retried HTTP")
	}
	if err = runRecoveryHost(ctx, acquire(path), recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) || calls.Load() != 2 {
		t.Fatal("usage committed before stdout loss", err)
	}
	var acquired recoveryUsageAcquisitionFile
	if readRecoveryFenceJSON(path+".acquisition", &acquired) != nil || string(acquired.Response) != recoveryHostUsagePeerBody || acquired.Receipt.Observation.MicroUSDStatus != "fractional" {
		t.Fatal("durable exact usage response")
	}
	if _, err = recoveryHostCommand(t, ctx, acquire(path)...); err == nil || calls.Load() != 2 {
		t.Fatal("successful attempt repeated HTTP")
	}
	if _, err = recoveryHostCommand(t, ctx, "usage-access-revoke", "--authority-dir", authority, "--expected-epoch", "1", "--operator-reference", "owned:revoke", "--confirm-revoke"); err != nil {
		t.Fatal("revoke", err)
	}
	checkAuthority(2, false)
	if err = os.Remove(key); err != nil {
		t.Fatal(err)
	}
	raw, err := recoveryHostCommand(t, ctx, "usage-inspect", "--intent-file", path)
	var inspected storage.RecoveryUsageRecording
	if err != nil || json.Unmarshal(raw, &inspected) != nil || inspected.State != "recorded" || inspected.AcquisitionSHA256 != acquired.Receipt.ReceiptSHA256 || !inspected.ExecutionHeld || calls.Load() != 2 {
		t.Fatal("historical inspection without credential", err)
	}
	// A different target selector cannot turn an existing observation into absence.
	var saved recoveryUsageFile
	if readRecoveryFenceJSON(path, &saved) != nil {
		t.Fatal("saved plan")
	}
	targetConfig.RestoreID = domain.NewID("restore")
	saved.TargetConfig = recoveryHostPrivateJSON(t, filepath.Join(parent, "wrong-target.json"), targetConfig)
	wrong := recoveryHostPrivateJSON(t, filepath.Join(parent, "wrong-usage.json"), saved)
	if err = runRecoveryHost(ctx, []string{"usage-inspect", "--intent-file", wrong}, io.Discard); err == nil || err.Error() != "saved usage plan does not select this target and capture" || calls.Load() != 2 {
		t.Fatal("wrong target reported absent")
	}
}

// Runs only inside the disposable CI container whose hosts mapping sends the
// fixed production hostname to loopback. The test does not edit the user's host,
// supply an endpoint override, expose a credential callback, or contact a model.
func recoveryHostUsageOwnedHTTPS(t *testing.T) *atomic.Int32 {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_USAGE_HTTPS") != "1" || os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("disposable owned HTTPS and PostgreSQL environment required")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("owned trust fixture requires disposable Linux container")
	}
	ips, err := net.LookupIP("openrouter.ai")
	if err != nil || len(ips) == 0 {
		t.Fatal("owned hostname unavailable")
	}
	for _, ip := range ips {
		if !ip.Equal(net.ParseIP("127.0.0.1")) {
			t.Fatal("refuse non-owned provider address")
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(17), DNSNames: []string{"openrouter.ai"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDir := t.TempDir()
	caPath := filepath.Join(privateDir, "owned-ca.pem")
	if err = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", caPath)
	t.Setenv("SSL_CERT_DIR", privateDir)
	listener, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		t.Fatal("owned TLS listener", err)
	}
	var calls atomic.Int32
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/generation" || r.URL.RawQuery != "id=gen-usage-selected" || r.Header.Get("Authorization") != "Bearer "+recoveryHostUsagePeerKey {
			http.Error(w, "unexpected synthetic request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recoveryHostUsagePeerBody))
	})}
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	done := make(chan error, 1)
	go func() { done <- server.Serve(tlsListener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return &calls
}
