// SPDX-License-Identifier: Elastic-2.0
package configurationruntime_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/configurationruntime"
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
)

type probeStore struct {
	mu              sync.Mutex
	marker          bool
	lostPrepareAck  bool
	failObservation bool
	observation     domain.ConfigurationProbeObservation
}

func (s *probeStore) PrepareConfigurationVerificationDispatch(ctx context.Context, l *domain.ConfigurationVerificationLease) (domain.ConfigurationDispatchPermit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.marker {
		return domain.ConfigurationDispatchPermit{}, errors.New("already sent")
	}
	s.marker = true
	if s.lostPrepareAck {
		return domain.ConfigurationDispatchPermit{}, errors.New("ambiguous commit acknowledgement")
	}
	now := time.Now()
	return domain.ConfigurationDispatchPermit{OrganizationID: l.OrganizationID, VerificationID: l.ID, WorkerID: l.WorkerID, Scope: l.Scope, ResourceVersion: l.ResourceVersion, ProfileDigest: l.ProfileDigest, FencingToken: l.Token, IssuedAt: now, NotAfter: now.Add(2 * time.Second), ValidFor: 2 * time.Second}, nil
}
func (s *probeStore) RecordConfigurationVerificationObservation(ctx context.Context, in domain.ConfigurationProbeObservation) (domain.ConfigurationVerification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.marker || s.failObservation {
		return domain.ConfigurationVerification{}, errors.New("safe receipt unavailable")
	}
	s.observation = in
	return domain.ConfigurationVerification{ID: in.VerificationID, OrganizationID: in.OrganizationID, Status: "response_received"}, nil
}
func (*probeStore) PauseConfigurationVerification(context.Context, *domain.ConfigurationVerificationLease, string) error {
	return nil
}

func probeConfig(t *testing.T, peer *httptest.Server) configurationruntime.Config {
	t.Helper()
	digest := sha256.Sum256([]byte(peer.URL))
	return configurationruntime.Config{Credential: "synthetic-mcp-key-echo", Probe: domain.ConfigurationProbeConfig{
		Destination: domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: "approved", OrganizationID: "org_probe", Version: 1, Kind: "mcp", TargetID: "registered", CredentialPurposeID: "purpose", CredentialPurposeVersion: 1, Active: true, DiscoveryUnmeteredApproved: true}, MCP: &domain.MCPConnectionProfile{Endpoint: peer.URL, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw})), AllowPrivateNetwork: true, TimeoutSeconds: 3}},
		Purpose:     domain.CredentialPurposeProfile{ID: "purpose", OrganizationID: "org_probe", Version: 1, Kind: "mcp", EndpointDigest: hex.EncodeToString(digest[:]), CredentialEnvironment: "PLATFORM_MCP_KEY_TEST", Active: true},
	}}
}
func probeLease(t *testing.T, peer *httptest.Server) *domain.ConfigurationVerificationLease {
	return &domain.ConfigurationVerificationLease{OrganizationID: "org_probe", ID: "cfg_probe", PlanID: "cfg_plan", OperationID: "mcp", WorkerID: "worker_probe", Scope: domain.ConfigurationScope{DestinationID: "approved", DestinationVersion: 1, Kind: "mcp", TargetID: "registered", CredentialPurposeID: "purpose", CredentialPurposeVersion: 1}, ResourceVersion: 1, ProfileDigest: domain.ConfigurationProbeDigest(probeConfig(t, peer).Probe, 1), Token: 1, LeaseUntil: time.Now().Add(time.Minute)}
}

func TestMCPConfigurationProbeNeverCallsToolsAndDiscardsPeerBodies(t *testing.T) {
	store := &probeStore{}
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		store.mu.Lock()
		marked := store.marker
		store.mu.Unlock()
		if !marked {
			t.Error("network before durable marker")
		}
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-mcp-key-echo" {
			t.Error("wrong bounded request boundary")
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("bad request")
			return
		}
		result := map[string]any{"resultType": "complete", "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "synthetic-peer", "version": "1"}}}
		switch req.Method {
		case "server/discover":
			result["supportedVersions"] = []string{mcpbridge.ProtocolVersion}
			result["capabilities"] = map[string]any{"tools": map[string]any{}}
		case "tools/list":
			result["tools"] = []any{map[string]any{"name": "synthetic-mcp-key-echo", "description": "synthetic-mcp-key-echo", "inputSchema": map[string]any{"type": "object"}}}
		default:
			t.Error("unexpected peer tool execution or fallback", req.Method)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer peer.Close()
	out, err := configurationruntime.Execute(context.Background(), store, probeLease(t, peer), probeConfig(t, peer))
	if err != nil || out.Status != "response_received" || calls.Load() != 2 {
		t.Fatal("discovery did not produce one structural receipt", err, calls.Load())
	}
	if store.observation.ToolCount != 1 || store.observation.ProtocolVersion != mcpbridge.ProtocolVersion || len(store.observation.CatalogDigest) != 64 {
		t.Fatal("missing bounded discovery receipt")
	}
	raw, _ := json.Marshal(store.observation)
	if strings.Contains(string(raw), "synthetic-mcp-key-echo") {
		t.Fatal("credential/tool description persisted")
	}
	_, err = configurationruntime.Execute(context.Background(), store, probeLease(t, peer), probeConfig(t, peer))
	if err == nil || calls.Load() != 2 {
		t.Fatal("sent occurrence retried")
	}
}

func TestMCPConfigurationProbeLostDispatchAckAndWrongPurposePerformNoNetwork(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "peer body must not escape")
	}))
	defer peer.Close()
	store := &probeStore{lostPrepareAck: true}
	out, err := configurationruntime.Execute(context.Background(), store, probeLease(t, peer), probeConfig(t, peer))
	if err == nil || out.Status != "unknown" || !store.marker || calls.Load() != 0 {
		t.Fatal("ambiguous dispatch admission was retried or treated proven unsent")
	}
	_, err = configurationruntime.Execute(context.Background(), store, probeLease(t, peer), probeConfig(t, peer))
	if err == nil || calls.Load() != 0 {
		t.Fatal("lost acknowledgement created replacement network attempt")
	}
	wrong := probeConfig(t, peer)
	wrong.Probe.Purpose.EndpointDigest = strings.Repeat("0", 64)
	fresh := &probeStore{}
	out, err = configurationruntime.Execute(context.Background(), fresh, probeLease(t, peer), wrong)
	if err == nil || out.Status != "not_dispatched" || fresh.marker || calls.Load() != 0 {
		t.Fatal("mismatched purpose admitted")
	}
}
