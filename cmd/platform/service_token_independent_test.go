// SPDX-License-Identifier: Elastic-2.0
package main

// Independent interoperation tests run the actual CLI entrypoint in child
// processes, isolated PostgreSQL and newline-framed MCP stdio. No provider or
// main database is used; these cases do not simulate COMMIT acknowledgement loss.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
)

type independentIssuedServiceFile struct {
	OrganizationID string            `json:"organization_id"`
	PrincipalID    string            `json:"principal_id"`
	Token          string            `json:"token"`
	Key            domain.ServiceKey `json:"key"`
}

func independentCLIService(t *testing.T, s *storage.Store, b storage.Bootstrap) domain.ServiceAccount {
	t.Helper()
	p, err := s.Authenticate(context.Background(), b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateServiceAccount(context.Background(), p, domain.NewID("key"), domain.CreateServiceAccount{DisplayName: "Independent CLI service", Role: "member", Reason: "Bounded private-file interoperation fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var v domain.ServiceAccount
	if json.Unmarshal(raw, &v) != nil || v.PrincipalKind != domain.PrincipalService {
		t.Fatal("missing actual service identity")
	}
	return v
}

func independentCLIServiceIssue(t *testing.T, dsn string, v domain.ServiceAccount, ceiling, ttl string) (string, independentIssuedServiceFile) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "service.json")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := independentCLICommand(t, ctx, dsn, "issue-service-token", "--organization", v.OrganizationID, "--principal", v.PrincipalID, "--role-ceiling", ceiling, "--ttl", ttl, "--reason", "Independent actual CLI credential delivery", "--out", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		t.Fatal("actual service issuance command failed", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file independentIssuedServiceFile
	if json.Unmarshal(raw, &file) != nil || file.Token == "" || file.OrganizationID != v.OrganizationID || file.PrincipalID != v.PrincipalID || file.Key.ID == "" || file.Key.RoleCeiling != ceiling {
		t.Fatal("actual CLI private file lost its credential metadata")
	}
	for _, p := range []struct {
		path string
		mode os.FileMode
	}{{path, 0600}, {filepath.Dir(path), 0700}} {
		st, err := os.Stat(p.path)
		if err != nil || st.Mode().Perm() != p.mode {
			t.Fatal("CLI credential path permissions are not private", err)
		}
	}
	if bytes.Contains(stdout.Bytes(), []byte(file.Token)) || bytes.Contains(stderr.Bytes(), []byte(file.Token)) || strings.Contains(stdout.String(), dsn) || strings.Contains(stderr.String(), dsn) {
		t.Fatal("CLI delivery exposed a secret in diagnostics")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if len(fields) != 4 || fields["key"] == nil {
		t.Fatal("service private file unexpectedly omitted metadata or added fields")
	}
	return path, file
}

type independentServiceStdio struct {
	t           *testing.T
	input       io.WriteCloser
	lines       chan []byte
	done        chan error
	cancel      context.CancelFunc
	once        sync.Once
	stderr      *bytes.Buffer
	secret, dsn string
	id          int
}

func independentServiceStdioStart(t *testing.T, dsn, path, secret string, mutations bool) *independentServiceStdio {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	args := []string{"mcp-stdio", "--credentials", path}
	if mutations {
		args = append(args, "--allow-mutations", "platform_agents_create")
	}
	cmd := independentCLICommand(t, ctx, dsn, args...)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &independentServiceStdio{t: t, input: input, lines: make(chan []byte, 16), done: make(chan error, 1), cancel: cancel, stderr: stderr, secret: secret, dsn: dsn}
	go func() { c.done <- cmd.Wait() }()
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			c.lines <- append([]byte{}, scanner.Bytes()...)
		}
		close(c.lines)
	}()
	t.Cleanup(c.finish)
	return c
}

func (c *independentServiceStdio) finish() {
	c.once.Do(func() {
		_ = c.input.Close()
		select {
		case err := <-c.done:
			if err != nil {
				c.t.Error("MCP child did not exit cleanly", err)
			}
		case <-time.After(3 * time.Second):
			c.cancel()
			select {
			case <-c.done:
			case <-time.After(2 * time.Second):
			}
			c.t.Error("MCP child failed bounded EOF shutdown")
		}
		c.cancel()
		if bytes.Contains(c.stderr.Bytes(), []byte(c.secret)) || strings.Contains(c.stderr.String(), c.dsn) {
			c.t.Error("MCP diagnostics exposed a secret")
		}
	})
}

func (c *independentServiceStdio) call(method string, params map[string]any) map[string]any {
	c.t.Helper()
	c.id++
	params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if _, err := c.input.Write(append(raw, '\n')); err != nil {
		c.t.Fatal("MCP input unavailable")
	}
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatal("MCP protocol closed before a response")
		}
		var response map[string]any
		if json.Unmarshal(line, &response) != nil || response["jsonrpc"] != "2.0" || response["id"] != float64(c.id) {
			c.t.Fatal("MCP stdout was nonprotocol or miscorrelated")
		}
		if bytes.Contains(line, []byte(c.secret)) || bytes.Contains(line, []byte(c.dsn)) {
			c.t.Fatal("MCP protocol exposed a secret")
		}
		return response
	case <-time.After(3 * time.Second):
		c.t.Fatal("MCP response exceeded bounded wait")
	}
	return nil
}

func independentServiceStdioData(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok || response["error"] != nil || result["isError"] == true {
		t.Fatal("MCP tool call failed")
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatal("MCP structured result missing")
	}
	data, ok := structured["data"].(map[string]any)
	if !ok {
		t.Fatal("MCP data missing")
	}
	return data
}

func independentServiceStdioUnauthorized(t *testing.T, response map[string]any) {
	t.Helper()
	failure, ok := response["error"].(map[string]any)
	if !ok || failure["code"] != float64(-32001) {
		t.Fatal("MCP did not enforce current credential authorization")
	}
}

func TestIndependentServiceCLIPrivateFileInteroperatesWithoutMetadataAuthority(t *testing.T) {
	s, dsn, b := independentCLIDB(t)
	v := independentCLIService(t, s, b)
	path, file := independentCLIServiceIssue(t, dsn, v, "viewer", "1m")
	// The actual token has a viewer ceiling. Forge every displayed principal,
	// expiry and key ceiling while retaining the real organization and token.
	file.PrincipalID = b.PrincipalID
	file.Key.PrincipalID = b.PrincipalID
	file.Key.RoleCeiling = "member"
	file.Key.Revoked = false
	file.Key.ExpiresAt = time.Now().Add(24 * time.Hour)
	raw, _ := json.Marshal(file)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c := independentServiceStdioStart(t, dsn, path, file.Token, true)
	if c.call("server/discover", map[string]any{})["error"] != nil {
		t.Fatal("CLI discovery failed for actual issued service file")
	}
	data := independentServiceStdioData(t, c.call("tools/call", map[string]any{"name": "platform_identity", "arguments": map[string]any{}}))
	if data["principal_id"] != v.PrincipalID || data["organization_id"] != v.OrganizationID || data["principal_kind"] != "service" || data["role"] != "viewer" {
		t.Fatal("file metadata changed current service identity or role ceiling")
	}
	denied := c.call("tools/call", map[string]any{"name": "platform_agents_create", "arguments": map[string]any{"idempotency_key": "independent-metadata-ceiling", "input": map[string]any{"name": "Must not exist", "instructions": "Viewer ceiling denies explicit mutation"}}})
	result, ok := denied["result"].(map[string]any)
	if !ok || result["isError"] != true {
		t.Fatal("forged key metadata expanded viewer write authority")
	}
	var count int
	if err := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM agents WHERE organization_id=$1", v.OrganizationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("denied viewer request left an agent", count, err)
	}
	c.finish()
}

func TestIndependentServiceCLIStdioChecksLiveExpiryRevocationAndHomeOrganization(t *testing.T) {
	for _, boundary := range []string{"expiry", "revoke"} {
		t.Run(boundary, func(t *testing.T) {
			s, dsn, b := independentCLIDB(t)
			v := independentCLIService(t, s, b)
			ttl := "1m"
			if boundary == "expiry" {
				ttl = "4s"
			}
			path, file := independentCLIServiceIssue(t, dsn, v, "member", ttl)
			actualExpiry := file.Key.ExpiresAt
			file.Key.ExpiresAt = time.Now().Add(24 * time.Hour)
			file.Key.Revoked = false
			raw, _ := json.Marshal(file)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			c := independentServiceStdioStart(t, dsn, path, file.Token, false)
			data := independentServiceStdioData(t, c.call("tools/call", map[string]any{"name": "platform_identity", "arguments": map[string]any{}}))
			if data["principal_kind"] != "service" || data["role"] != "member" {
				t.Fatal("actual CLI key did not identify the service")
			}
			if boundary == "revoke" {
				owner, err := s.Authenticate(context.Background(), b.Token, b.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.RevokeServiceKey(context.Background(), owner, v.PrincipalID, file.Key.ID, domain.NewID("key"), domain.RevokeServiceKey{ExpectedVersion: file.Key.Version, Reason: "Stop new MCP admissions"}); err != nil {
					t.Fatal(err)
				}
			} else {
				deadline := time.Now().Add(6 * time.Second)
				for {
					var expired bool
					if err := s.Pool.QueryRow(context.Background(), "SELECT clock_timestamp()>=$1", actualExpiry).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					if expired {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("CLI key failed natural expiration")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			independentServiceStdioUnauthorized(t, c.call("tools/list", map[string]any{}))
			c.finish()
			independentCLIRejected(t, dsn, file.Token, "not currently authorized", "mcp-stdio", "--credentials", path)
		})
	}
	s, dsn, b := independentCLIDB(t)
	v := independentCLIService(t, s, b)
	path, file := independentCLIServiceIssue(t, dsn, v, "viewer", "1m")
	other, err := s.Bootstrap(context.Background(), "Other CLI organization", 100000)
	if err != nil {
		t.Fatal(err)
	}
	file.OrganizationID = other.OrganizationID
	raw, _ := json.Marshal(file)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	independentCLIRejected(t, dsn, file.Token, "not currently authorized", "mcp-stdio", "--credentials", path)
}

func TestIndependentServiceCLIRefusesOverwriteSymlinkAndNoncanonicalMetadata(t *testing.T) {
	s, dsn, b := independentCLIDB(t)
	v := independentCLIService(t, s, b)
	path, file := independentCLIServiceIssue(t, dsn, v, "viewer", "1m")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"issue-service-token", "--organization", v.OrganizationID, "--principal", v.PrincipalID, "--role-ceiling", "viewer", "--ttl", "1m", "--reason", "Must not overwrite existing credential", "--out", path}
	independentCLIRejected(t, dsn, file.Token, "new private file", args...)
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(original, current) {
		t.Fatal("CLI overwrite rejection changed an existing private credential", err)
	}
	link := filepath.Join(t.TempDir(), "service-link.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = link
	independentCLIRejected(t, dsn, file.Token, "new private file", args...)
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(original, current) {
		t.Fatal("CLI symlink rejection changed its target", err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("CLI removed or followed existing symlink", err)
	}
	var count int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM service_credentials WHERE organization_id=$1", v.OrganizationID).Scan(&count); err != nil || count != 1 {
		t.Fatal("refused output created additional credentials", count, err)
	}
	for _, bad := range [][]byte{
		bytes.Replace(original, []byte(`"key":{`), []byte(`"key":{"scopes":["*"],`), 1),
		bytes.Replace(original, []byte(`"role_ceiling":"viewer"`), []byte(`"role_ceiling":"member","role_ceiling":"viewer"`), 1),
		bytes.Replace(original, []byte(`"role_ceiling"`), []byte(`"ROLE_CEILING"`), 1),
		append(append([]byte{}, bytes.TrimSpace(original)[:len(bytes.TrimSpace(original))-1]...), []byte(`,"ready":true}`)...),
	} {
		if bytes.Equal(bad, original) {
			t.Fatal("negative fixture did not mutate the issued file")
		}
		malformed := filepath.Join(t.TempDir(), "bad.json")
		if err = os.WriteFile(malformed, bad, 0600); err != nil {
			t.Fatal(err)
		}
		independentCLIRejected(t, dsn, file.Token, "invalid private MCP credentials", "mcp-stdio", "--credentials", malformed)
	}
	var nullKey map[string]json.RawMessage
	_ = json.Unmarshal(original, &nullKey)
	nullKey["key"] = json.RawMessage("null")
	nullRaw, _ := json.Marshal(nullKey)
	nullPath := filepath.Join(t.TempDir(), "null-key.json")
	if err = os.WriteFile(nullPath, nullRaw, 0600); err != nil {
		t.Fatal(err)
	}
	independentCLIRejected(t, dsn, file.Token, "invalid private MCP credentials", "mcp-stdio", "--credentials", nullPath)
}

func TestIndependentHumanCLIExpiresAtPrivateFileStillInteroperatesWithMCP(t *testing.T) {
	_, dsn, b := independentCLIDB(t)
	path := filepath.Join(t.TempDir(), "human.json")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := independentCLICommand(t, ctx, dsn, "issue-token", "--organization", b.OrganizationID, "--principal", b.PrincipalID, "--ttl", "1m", "--out", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal("legacy human private credential issuance failed", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		OrganizationID string    `json:"organization_id"`
		PrincipalID    string    `json:"principal_id"`
		Token          string    `json:"token"`
		ExpiresAt      time.Time `json:"expires_at"`
	}
	if json.Unmarshal(raw, &file) != nil || file.Token == "" || file.ExpiresAt.IsZero() {
		t.Fatal("legacy human file omitted its real expires_at metadata")
	}
	if bytes.Contains(stdout.Bytes(), []byte(file.Token)) || bytes.Contains(stderr.Bytes(), []byte(file.Token)) {
		t.Fatal("human issuance exposed its secret")
	}
	c := independentServiceStdioStart(t, dsn, path, file.Token, false)
	data := independentServiceStdioData(t, c.call("tools/call", map[string]any{"name": "platform_identity", "arguments": map[string]any{}}))
	if data["principal_id"] != b.PrincipalID || data["organization_id"] != b.OrganizationID || data["principal_kind"] != "human" || data["role"] != "owner" {
		t.Fatal("legacy human expires_at file no longer interoperates")
	}
	c.finish()
}
