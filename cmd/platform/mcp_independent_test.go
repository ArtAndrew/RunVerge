// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

// The child calls the real CLI entrypoint, with stdout separate from diagnostics.
// This test reviews root-authored wiring; it does not reimplement MCP routing.
func TestIndependentMCPCLIChildProcess(t *testing.T) {
	if os.Getenv("MCP_CLI_INDEPENDENT_CHILD") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"platform"}, os.Args[i+1:]...)
			if e := run(); e != nil {
				fmt.Fprintln(os.Stderr, e.Error())
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}
func independentCLIDB(t *testing.T) (*storage.Store, string, storage.Bootstrap) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	base, e := storage.Open(ctx, dsn)
	if e != nil {
		t.Fatal("independent CLI database unavailable")
	}
	schema := domain.NewID("cli_mcp_test")
	if _, e = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		base.Close()
		t.Fatal(e)
	}
	parsed, e := url.Parse(dsn)
	if e != nil {
		t.Fatal("invalid independent database configuration")
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	isolated := parsed.String()
	s, e := storage.Open(ctx, isolated)
	if e != nil {
		t.Fatal("isolated CLI database unavailable")
	}
	t.Cleanup(func() {
		s.Close()
		_, e := base.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if e != nil {
			t.Error(e)
		}
	})
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	b, e := s.Bootstrap(ctx, "Independent MCP CLI fixture", 100000)
	if e != nil {
		t.Fatal(e)
	}
	return s, isolated, b
}
func independentCLICommand(t *testing.T, ctx context.Context, dsn string, args ...string) *exec.Cmd {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestIndependentMCPCLIChildProcess$", "--"}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DATABASE_URL=") && !strings.HasPrefix(entry, "MCP_CLI_INDEPENDENT_CHILD=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "DATABASE_URL="+dsn, "MCP_CLI_INDEPENDENT_CHILD=1")
	return cmd
}
func independentCredentialFile(t *testing.T, b storage.Bootstrap) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	raw, _ := json.Marshal(b)
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	return path
}
func independentCLIRejected(t *testing.T, dsn, secret, want string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := independentCLICommand(t, ctx, dsn, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	e := cmd.Run()
	if e == nil || ctx.Err() != nil || !strings.Contains(stderr.String(), want) {
		t.Fatalf("expected bounded CLI rejection %q, error=%v", want, e)
	}
	if stdout.Len() != 0 {
		t.Fatal("CLI configuration error contaminated protocol stdout")
	}
	if strings.Contains(stderr.String(), secret) || strings.Contains(stderr.String(), dsn) {
		t.Fatal("CLI error exposed credentials")
	}
}
func TestIndependentMCPCLIHTTPDisabledAndExplicitMutationConfiguration(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ordinary-API", "yes")
		w.WriteHeader(404)
	})
	handler, e := mcpHTTPHandler(api, false, "", "")
	if e != nil {
		t.Fatal(e)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest("POST", "/mcp", nil))
	if out.Code != 404 || out.Header().Get("X-Ordinary-API") != "yes" {
		t.Fatal("MCP unexpectedly enabled by default")
	}
	for _, tc := range []struct {
		name               string
		enabled            bool
		mutations, origins string
	}{
		{"mutations_require_enable", false, "platform_tasks_create", ""},
		{"origins_require_enable", false, "", "https://workspace.example"},
		{"unknown_mutation", true, "platform_unregistered_write", ""},
		{"duplicate_mutation", true, "platform_tasks_create,platform_tasks_create", ""},
		{"empty_mutation_entry", true, "platform_tasks_create,", ""},
		{"invalid_origin", true, "", "https://workspace.example/path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e = mcpHTTPHandler(api, tc.enabled, tc.mutations, tc.origins); e == nil {
				t.Fatal("unsafe operator profile accepted")
			}
		})
	}
	handler, e = mcpHTTPHandler(api, true, "", "")
	if e != nil {
		t.Fatal(e)
	}
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest("POST", "/mcp", nil))
	if out.Code != 401 {
		t.Fatal("enabled MCP accepted absent explicit credentials")
	}
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest("GET", "/api/v1/me", nil))
	if out.Header().Get("X-Ordinary-API") != "yes" {
		t.Fatal("MCP mount replaced ordinary API")
	}
}
func TestIndependentMCPCLIServeRequiresTLSAndLoopback(t *testing.T) {
	_, dsn, b := independentCLIDB(t)
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"plaintext_mcp", "requires TLS", []string{"serve", "--mcp"}},
		{"mutation_flag_without_mcp", "requires --mcp", []string{"serve", "--mcp-mutations", "platform_tasks_create"}},
		{"origin_flag_without_mcp", "requires --mcp", []string{"serve", "--mcp-origins", "https://workspace.example"}},
		{"incomplete_tls_pair", "invalid serve arguments", []string{"serve", "--mcp", "--tls-cert", "missing.pem"}},
		{"nonloopback_proxy", "requires a loopback", []string{"serve", "--mcp", "--trusted-tls-proxy", "--listen", "0.0.0.0:0"}},
		{"positional_argument", "invalid serve arguments", []string{"serve", "--mcp", "--trusted-tls-proxy", "unexpected"}},
	} {
		t.Run(tc.name, func(t *testing.T) { independentCLIRejected(t, dsn, b.Token, tc.want, tc.args...) })
	}
}
func TestIndependentMCPCLICredentialsMustBeExplicitPrivateAndCanonical(t *testing.T) {
	_, dsn, b := independentCLIDB(t)
	good := independentCredentialFile(t, b)
	independentCLIRejected(t, dsn, b.Token, "requires an explicit private", "mcp-stdio")
	public := independentCredentialFile(t, b)
	os.Chmod(public, 0644)
	independentCLIRejected(t, dsn, b.Token, "invalid private MCP credentials", "mcp-stdio", "--credentials", public)
	link := filepath.Join(t.TempDir(), "symlink.json")
	if e := os.Symlink(good, link); e != nil {
		t.Fatal(e)
	}
	independentCLIRejected(t, dsn, b.Token, "invalid private MCP credentials", "mcp-stdio", "--credentials", link)
	raw, _ := json.Marshal(b)
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"duplicate_token", bytes.Replace(raw, []byte(`"token":`), []byte(`"token":"fixture-shadow","token":`), 1)},
		{"noncanonical_token", bytes.Replace(raw, []byte(`"token":`), []byte(`"TOKEN":`), 1)},
		{"unknown_principal_override", append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"impersonate":"principal_other"}`)...)},
		{"two_objects", append(append([]byte{}, raw...), raw...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "malformed.json")
			if e := os.WriteFile(path, tc.raw, 0600); e != nil {
				t.Fatal(e)
			}
			independentCLIRejected(t, dsn, b.Token, "invalid private MCP credentials", "mcp-stdio", "--credentials", path)
		})
	}
	wrong := b
	wrong.Token = "independent-invalid-token"
	independentCLIRejected(t, dsn, b.Token, "not currently authorized", "mcp-stdio", "--credentials", independentCredentialFile(t, wrong))
	independentCLIRejected(t, dsn, b.Token, "invalid private MCP mutation allowlist", "mcp-stdio", "--credentials", good, "--allow-mutations", "platform_unknown")
}
func TestIndependentMCPCLIStdioProtocolOnlyAndCurrentAuthority(t *testing.T) {
	s, dsn, b := independentCLIDB(t)
	stored := b
	stored.PrincipalID = "principal_forged_metadata"
	credential := independentCredentialFile(t, stored)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := independentCLICommand(t, ctx, dsn, "mcp-stdio", "--credentials", credential)
	input, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	output, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	lines := make(chan []byte, 8)
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			lines <- append([]byte{}, scanner.Bytes()...)
		}
		close(lines)
	}()
	call := func(id int, method string, params map[string]any) map[string]any {
		t.Helper()
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, e := input.Write(append(frame, '\n')); e != nil {
			t.Fatal("CLI stdin unavailable")
		}
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("CLI protocol stdout closed")
			}
			var response map[string]any
			if json.Unmarshal(line, &response) != nil || response["jsonrpc"] != "2.0" || response["id"] != float64(id) {
				t.Fatal("nonprotocol or miscorrelated stdout")
			}
			if bytes.Contains(line, []byte(b.Token)) {
				t.Fatal("CLI stdout exposed token")
			}
			return response
		case <-time.After(3 * time.Second):
			t.Fatal("CLI protocol response timeout")
		}
		return nil
	}
	discovery := call(1, "server/discover", map[string]any{})
	if discovery["error"] != nil {
		t.Fatal("CLI discovery failed")
	}
	listing := call(2, "tools/list", map[string]any{})
	tools := listing["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 51 {
		t.Fatal("CLI default catalogue not read-only")
	}
	names := map[string]bool{}
	for _, tool := range tools {
		name := tool.(map[string]any)["name"].(string)
		if names[name] {
			t.Fatal("CLI default catalogue contains duplicate tool names")
		}
		names[name] = true
		if name == "platform_tasks_create" {
			t.Fatal("CLI mutation default enabled")
		}
	}
	for _, name := range []string{
		"platform_configuration_catalog", "platform_configuration_destinations_list", "platform_configuration_resources_list",
		"platform_configuration_grants_list", "platform_configuration_grants_get", "platform_configuration_plans_list",
		"platform_configuration_plans_get", "platform_configuration_verifications_list", "platform_configuration_verifications_get",
		"platform_configuration_audit_list",
	} {
		if !names[name] {
			t.Fatalf("CLI default catalogue omitted configuration read %s", name)
		}
	}
	for _, name := range []string{
		"platform_configuration_grants_create", "platform_configuration_grants_revoke", "platform_configuration_plans_create",
		"platform_configuration_plans_validate", "platform_configuration_plans_approve", "platform_configuration_plans_apply",
		"platform_configuration_plans_verify",
	} {
		if names[name] {
			t.Fatalf("CLI configuration mutation default enabled: %s", name)
		}
	}
	identity := call(3, "tools/call", map[string]any{"name": "platform_identity", "arguments": map[string]any{}})
	data := identity["result"].(map[string]any)["structuredContent"].(map[string]any)["data"].(map[string]any)
	if data["principal_id"] != b.PrincipalID || data["organization_id"] != b.OrganizationID {
		t.Fatal("CLI principal did not come from private current credentials")
	}
	if _, e = s.Pool.Exec(context.Background(), "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", storage.Hash([]byte(b.Token))); e != nil {
		t.Fatal(e)
	}
	if call(4, "tools/list", map[string]any{})["error"] == nil {
		t.Fatal("CLI kept startup-only authority after revocation")
	}
	input.Close()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal("CLI did not exit cleanly on EOF")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CLI EOF did not stop process")
	}
	if ctx.Err() != nil || bytes.Contains(stderr.Bytes(), []byte(b.Token)) || strings.Contains(stderr.String(), dsn) {
		t.Fatal("CLI lifetime or credential output boundary failed")
	}
}
func TestIndependentCurrentIdentityAndMaterialQueriesUseCurrentAuthority(t *testing.T) {
	s, _, b := independentCLIDB(t)
	handler := (&httpapi.Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	read := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+b.Token)
		r.Header.Set("X-Organization-ID", b.OrganizationID)
		r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	assertIdentity := func(role string) {
		t.Helper()
		w := read("GET", "/api/v1/me")
		if w.Code != 200 {
			t.Fatal("current identity rejected")
		}
		var envelope struct {
			Data domain.CurrentIdentity `json:"data"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &envelope); e != nil {
			t.Fatal(e)
		}
		if envelope.Data.PrincipalID != b.PrincipalID || envelope.Data.OrganizationID != b.OrganizationID || envelope.Data.Role != role || !reflect.DeepEqual(envelope.Data.Permissions, domain.RolePermissions[role]) {
			t.Fatal("identity did not reflect current database authority")
		}
	}
	assertIdentity("owner")
	if _, e := s.Pool.Exec(context.Background(), "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", b.OrganizationID, b.PrincipalID); e != nil {
		t.Fatal(e)
	}
	assertIdentity("viewer")
	for _, path := range []string{
		"/api/v1/me?principal_id=other", "/api/v1/me?role=owner", "/api/v1/me?x=%zz",
		"/api/v1/tasks/tsk_absent/materials?limit=1&limit=2", "/api/v1/tasks/tsk_absent/materials?cursor=a&cursor=b", "/api/v1/tasks/tsk_absent/materials?owner_id=other", "/api/v1/tasks/tsk_absent/materials?limit=1;cursor=x",
		"/api/v1/tasks/tsk_absent/materials/mat_absent?version=1", "/api/v1/tasks/tsk_absent/materials/mat_absent/revisions?limit=1&limit=2", "/api/v1/tasks/tsk_absent/materials/mat_absent/revisions/1?version=2",
		"/api/v1/tasks/tsk_absent/artifacts/art_absent?version=", "/api/v1/tasks/tsk_absent/artifacts/art_absent?version=1&version=2", "/api/v1/tasks/tsk_absent/artifacts/art_absent?version=nan", "/api/v1/tasks/tsk_absent/artifacts/art_absent?principal_id=other",
	} {
		t.Run(path, func(t *testing.T) {
			if w := read("GET", path); w.Code != 400 {
				t.Fatalf("ambiguous query returned %d instead of400", w.Code)
			}
		})
	}
	if _, e := s.Pool.Exec(context.Background(), "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", b.OrganizationID, b.PrincipalID); e != nil {
		t.Fatal(e)
	}
	if w := read("GET", "/api/v1/me"); w.Code != 401 {
		t.Fatal("identity survived revoked membership")
	}
}
