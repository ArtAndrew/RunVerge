// SPDX-License-Identifier: Elastic-2.0
package httpapi

// Explicit opt-in consumer orchestration: disposable PostgreSQL schema, actual
// API/worker and synthetic local peers. Private files are not product endpoints.
import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/worker"
	"github.com/jackc/pgx/v5"
)

type configurationConsumerMetadata struct {
	BaseURL            string `json:"base_url"`
	OrganizationID     string `json:"organization_id"`
	PrincipalID        string `json:"principal_id"`
	Token              string `json:"token"`
	ServiceToken       string `json:"service_token"`
	ServicePrincipalID string `json:"service_principal_id"`
	ModelDestinationID string `json:"model_destination_id"`
	MCPDestinationID   string `json:"mcp_destination_id"`
	DoneFile           string `json:"done_file"`
	ObservationsFile   string `json:"observations_file"`
}

func configurationConsumerStore(t *testing.T, ctx context.Context) *storage.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated configuration consumer fixture")
	}
	base, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal("isolated PostgreSQL unavailable")
	}
	schema := domain.NewID("configuration_consumer")
	if _, err = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal("isolated schema creation failed")
	}
	var isolated *storage.Store
	t.Cleanup(func() {
		if isolated != nil {
			isolated.Close()
		}
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, err := base.Pool.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if err != nil {
			t.Error("isolated configuration schema cleanup failed")
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	isolated, err = storage.Open(ctx, u.String())
	if err != nil {
		t.Fatal("isolated store open failed")
	}
	if isolated.Migrate(ctx) != nil {
		t.Fatal("isolated configuration migrations failed")
	}
	if isolated.ConfigureLoginEncryption(make([]byte, 32)) != nil {
		t.Fatal("isolated encryption setup failed")
	}
	return isolated
}

func TestConfigurationConsumerHandoffFixture(t *testing.T) {
	metadataPath := os.Getenv("PLATFORM_CONFIGURATION_SMOKE_METADATA")
	if metadataPath == "" {
		t.Skip("explicit private configuration consumer fixture opt-in required")
	}
	if !filepath.IsAbs(metadataPath) {
		t.Fatal("fixture metadata requires an absolute new private path")
	}
	info, err := os.Lstat(filepath.Dir(metadataPath))
	if err != nil || !eventHandoffPrivate(info, true) {
		t.Fatal("fixture requires an owned private 0700 directory")
	}
	for _, p := range []string{metadataPath, metadataPath + ".done", metadataPath + ".observations.json"} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatal("fixture output paths must initially be absent")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	s := configurationConsumerStore(t, ctx)
	b, err := s.Bootstrap(ctx, "Isolated configuration consumers", 10000000)
	if err != nil {
		t.Fatal("isolated bootstrap failed")
	}
	owner, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal("isolated owner authentication failed")
	}
	raw, err := s.CreateServiceAccount(ctx, owner, domain.NewID("key"), domain.CreateServiceAccount{DisplayName: "Isolated configuration agent", Role: "member", Reason: "Synthetic consumer fixture provisioning"})
	if err != nil {
		t.Fatal("isolated service provisioning failed")
	}
	var service domain.ServiceAccount
	if json.Unmarshal(raw, &service) != nil || service.PrincipalID == "" {
		t.Fatal("isolated service response invalid")
	}
	serviceToken, _, err := s.IssueServiceToken(ctx, b.OrganizationID, service.PrincipalID, "member", 10*time.Minute, "Synthetic private delivery")
	if err != nil {
		t.Fatal("isolated service credential failed")
	}
	modelSecret, mcpSecret := domain.NewID("synthetic_model_secret"), domain.NewID("synthetic_mcp_secret")
	var modelCalls, discoverCalls, listCalls, toolCalls, authFailures atomic.Int64
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+modelSecret {
			authFailures.Add(1)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		var request map[string]json.RawMessage
		if json.NewDecoder(io.LimitReader(r.Body, 131073)).Decode(&request) != nil {
			http.Error(w, "invalid fixture JSON", 400)
			return
		}
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Deliberately hostile credential echo. The configuration probe must
		// discard both raw content and provider request ID before persistence.
		_ = json.NewEncoder(w).Encode(map[string]any{"id": modelSecret, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Untrusted fixture echo: " + modelSecret}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	}))
	t.Cleanup(model.Close)
	mcp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+mcpSecret {
			authFailures.Add(1)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1048577)).Decode(&request) != nil {
			http.Error(w, "invalid fixture JSON", 400)
			return
		}
		result := map[string]any{"resultType": "complete", "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "isolated-configuration-peer", "version": "1"}}}
		switch request.Method {
		case "server/discover":
			discoverCalls.Add(1)
			result["supportedVersions"] = []string{mcpbridge.ProtocolVersion}
			result["capabilities"] = map[string]any{"tools": map[string]any{}}
		case "tools/list":
			listCalls.Add(1)
			result["tools"] = []any{map[string]any{"name": "not_authorized_for_execution", "description": "Untrusted credential echo: " + mcpSecret, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}
		case "tools/call":
			toolCalls.Add(1)
			http.Error(w, "tools/call forbidden in discovery fixture", 403)
			return
		default:
			http.Error(w, "unsupported fixture method", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(mcp.Close)
	modelEndpoint := model.URL + "/v1/chat/completions"
	modelPurpose, err := s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: domain.NewID("purpose"), OrganizationID: b.OrganizationID, Version: 1, Kind: "model", EndpointDigest: storage.Hash([]byte(modelEndpoint)), CredentialEnvironment: "PLATFORM_MODEL_KEY_CONFIGURATION_CONSUMER", Active: true})
	if err != nil {
		t.Fatal("isolated model purpose install failed")
	}
	mcpPurpose, err := s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: domain.NewID("purpose"), OrganizationID: b.OrganizationID, Version: 1, Kind: "mcp", EndpointDigest: storage.Hash([]byte(mcp.URL)), CredentialEnvironment: "PLATFORM_MCP_KEY_CONFIGURATION_CONSUMER", Active: true})
	if err != nil {
		t.Fatal("isolated MCP purpose install failed")
	}
	modelID := domain.NewID("consumer_model")
	modelDestination, err := s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: domain.NewID("destination"), OrganizationID: b.OrganizationID, Version: 1, Kind: "model", Name: "Synthetic model connection", TargetID: modelID, CredentialPurposeID: modelPurpose.ID, CredentialPurposeVersion: 1, RequiresApproval: true, MaxVerifyChargeMicroUSD: 10000, Model: "configuration-consumer-fixture", Active: true}, Model: &domain.ModelConnection{ID: modelID, OrganizationID: b.OrganizationID, Version: 1, Model: "configuration-consumer-fixture", ProviderAccount: "isolated-synthetic-model", PriceVersion: "synthetic-v1", Endpoint: modelEndpoint, MaxChargeMicroUSD: 10000, MaxInputTokens: 1000, MaxOutputTokens: 64, InputMicroUSDPerMillion: 1000000, OutputMicroUSDPerMillion: 1000000, MaxInputBytes: 65536, MaxOutputBytes: 4096, TimeoutMilliseconds: 2000, BoundAssumptions: "Synthetic connectivity only, provider usage is not a bill", TokenLimitField: "max_tokens", AllowInsecureLoopback: true}})
	if err != nil {
		t.Fatal("isolated model destination install failed")
	}
	mcpDestination, err := s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: domain.NewID("destination"), OrganizationID: b.OrganizationID, Version: 1, Kind: "mcp", Name: "Synthetic TLS MCP registry", TargetID: domain.NewID("consumer_mcp"), CredentialPurposeID: mcpPurpose.ID, CredentialPurposeVersion: 1, RequiresApproval: true, DiscoveryUnmeteredApproved: true, Active: true}, MCP: &domain.MCPConnectionProfile{Endpoint: mcp.URL, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mcp.Certificate().Raw})), AllowPrivateNetwork: true, TimeoutSeconds: 3}})
	if err != nil {
		t.Fatal("isolated MCP destination install failed")
	}
	staticDir, err := filepath.Abs(filepath.Join("..", "..", "web", "dist"))
	if err != nil {
		t.Fatal("compiled UI path unavailable")
	}
	if _, err = os.Stat(filepath.Join(staticDir, "index.html")); err != nil {
		t.Fatal("build current UI before live fixture")
	}
	api := httptest.NewServer((&Server{Store: s, StaticDir: staticDir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	t.Cleanup(func() { api.CloseClientConnections(); api.Close() })
	workerDone := make(chan error, 1)
	workerStopped := false
	workerContext, stopWorker := context.WithCancel(ctx)
	w := &worker.Worker{Store: s, ID: domain.NewID("consumer_worker"), Concurrency: 2, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ResolveModelCredential: func(name string) (string, error) {
		if name != "PLATFORM_MODEL_KEY_CONFIGURATION_CONSUMER" {
			return "", domain.Forbidden
		}
		return modelSecret, nil
	}, ResolveMCPCredential: func(name string) (string, error) {
		if name != "PLATFORM_MCP_KEY_CONFIGURATION_CONSUMER" {
			return "", domain.Forbidden
		}
		return mcpSecret, nil
	}, ResolveToolCredential: func(string) (string, error) { return "", domain.Forbidden }}
	go func() { workerDone <- w.Run(workerContext) }()
	t.Cleanup(func() {
		stopWorker()
		if workerStopped {
			return
		}
		select {
		case err := <-workerDone:
			if err != nil {
				t.Error("isolated worker failed")
			}
		case <-time.After(8 * time.Second):
			t.Error("isolated worker cleanup exceeded bound")
		}
	})
	metadata := configurationConsumerMetadata{BaseURL: api.URL, OrganizationID: b.OrganizationID, PrincipalID: b.PrincipalID, Token: b.Token, ServiceToken: serviceToken, ServicePrincipalID: service.PrincipalID, ModelDestinationID: modelDestination.Destination.ID, MCPDestinationID: mcpDestination.Destination.ID, DoneFile: metadataPath + ".done", ObservationsFile: metadataPath + ".observations.json"}
	body, _ := json.Marshal(metadata)
	if eventHandoffPublish(metadataPath, body, false) != nil {
		t.Fatal("private configuration metadata publication failed")
	}
	t.Log("isolated configuration API and worker ready; synthetic credentials remain private")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		observation := map[string]any{"model_calls": modelCalls.Load(), "mcp_discover_calls": discoverCalls.Load(), "mcp_list_calls": listCalls.Load(), "tools_call_calls": toolCalls.Load(), "authentication_failures": authFailures.Load()}
		body, _ := json.Marshal(observation)
		if string(body) != last {
			if eventHandoffPublish(metadata.ObservationsFile, body, true) != nil {
				t.Fatal("private observations publication failed")
			}
			last = string(body)
		}
		if _, err := eventHandoffReadPrivate(metadata.DoneFile); err == nil {
			// Search all disposable relation rows for the deliberately echoed
			// synthetic peer credentials. Report a boolean, never raw records.
			stopWorker()
			select {
			case err := <-workerDone:
				workerStopped = true
				if err != nil {
					t.Fatal("isolated worker failed")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("isolated worker did not stop")
			}
			names, err := s.Pool.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname=current_schema()")
			if err != nil {
				t.Fatal("credential echo audit unavailable")
			}
			tables := []string{}
			for names.Next() {
				var name string
				if names.Scan(&name) != nil {
					t.Fatal("credential echo audit failed")
				}
				tables = append(tables, name)
			}
			names.Close()
			for _, name := range tables {
				var leaked bool
				query := "SELECT EXISTS(SELECT 1 FROM " + pgx.Identifier{name}.Sanitize() + " r WHERE position($1 in to_jsonb(r)::text)>0 OR position($2 in to_jsonb(r)::text)>0)"
				if s.Pool.QueryRow(ctx, query, modelSecret, mcpSecret).Scan(&leaked) != nil || leaked {
					t.Fatal("credential echo reached disposable persistence or audit failed")
				}
			}
			if toolCalls.Load() != 0 || authFailures.Load() != 0 {
				t.Fatal("discovery invoked a tool or sent the wrong credential")
			}
			observation["credential_echo_persistence_checked"] = true
			observation["credential_echo_persisted"] = false
			observation["inspected_tables"] = len(tables)
			body, _ = json.Marshal(observation)
			if eventHandoffPublish(metadata.ObservationsFile, body, true) != nil {
				t.Fatal("private final observations publication failed")
			}
			return
		} else if !os.IsNotExist(err) {
			t.Fatal("unsafe fixture completion file")
		}
		select {
		case <-ctx.Done():
			t.Fatal("configuration consumer fixture exceeded ten-minute bound")
		case <-ticker.C:
		}
	}
}
