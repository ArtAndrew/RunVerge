// SPDX-License-Identifier: Elastic-2.0
package main

// This is a black-box onboarding scenario. PostgreSQL is used directly only to
// create/drop a disposable schema. All product setup uses the shipped CLI/API;
// there are intentionally no domain, storage, worker, or configuration imports.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type onboardingBlackBox struct {
	t                               *testing.T
	binary, root, private, dsn, url string
	env                             []string
	secrets                         []string
	sequence                        int
}

func onboardingRandom() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func onboardingString(v map[string]any, field string) string { s, _ := v[field].(string); return s }
func onboardingObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		t.Fatal("expected JSON object")
	}
	return v
}
func onboardingData(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	v, ok := onboardingObject(t, raw)["data"].(map[string]any)
	if !ok {
		t.Fatal("expected product data envelope")
	}
	return v
}
func (b *onboardingBlackBox) safe(raw []byte) {
	b.t.Helper()
	for _, secret := range b.secrets {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			b.t.Fatal("credential appeared in public output")
		}
	}
}
func (b *onboardingBlackBox) file(label string, value any) string {
	b.t.Helper()
	b.sequence++
	path := filepath.Join(b.private, fmt.Sprintf("%03d-%s.json", b.sequence, label))
	raw, err := json.Marshal(value)
	if err != nil || os.WriteFile(path, raw, 0600) != nil {
		b.t.Fatal("cannot create private input")
	}
	return path
}
func (b *onboardingBlackBox) command(host bool, args ...string) ([]byte, error) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.binary, args...)
	cmd.Dir = b.root
	cmd.Env = append(append([]string{}, b.env...), "DATABASE_URL=deliberately-unusable-client-database")
	if host {
		cmd.Env = append(append([]string{}, b.env...), "DATABASE_URL="+b.dsn)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	b.safe(raw)
	b.safe(stderr.Bytes())
	if len(raw) == 0 {
		raw = stderr.Bytes()
	}
	return raw, err
}
func (b *onboardingBlackBox) host(args ...string) {
	b.t.Helper()
	raw, err := b.command(true, args...)
	if err != nil {
		b.t.Fatalf("host CLI %s failed (safe output %q)", args[0], raw)
	}
	b.t.Logf("operator CLI %s: accepted", args[0])
}
func (b *onboardingBlackBox) config(credential, group, action, id, key string, input any) ([]byte, error) {
	b.t.Helper()
	args := []string{"config", group}
	if action != "" {
		args = append(args, action)
	}
	args = append(args, "--url", b.url, "--credentials", credential, "--json")
	if id != "" {
		args = append(args, "--id", id)
	}
	if input != nil {
		args = append(args, "--file", b.file("command", input), "--key", key)
	}
	return b.command(false, args...)
}
func (b *onboardingBlackBox) ok(credential, group, action, id, key string, input any) map[string]any {
	b.t.Helper()
	raw, err := b.config(credential, group, action, id, key, input)
	if err != nil {
		b.t.Fatalf("config %s %s failed: %q", group, action, raw)
	}
	return onboardingData(b.t, raw)
}
func (b *onboardingBlackBox) deny(credential, group, action, id, key string, input any, code string) {
	b.t.Helper()
	raw, err := b.config(credential, group, action, id, key, input)
	if err == nil || !bytes.Contains(raw, []byte(code)) {
		b.t.Fatalf("config %s %s did not reject with %s: %q", group, action, code, raw)
	}
	b.t.Logf("negative %s %s: %s", group, action, code)
}
func (b *onboardingBlackBox) api(credential, method, path, key string, input any) map[string]any {
	b.t.Helper()
	rawCreds, err := os.ReadFile(credential)
	if err != nil {
		b.t.Fatal("cannot read private API credential")
	}
	c := onboardingObject(b.t, rawCreds)
	raw, err := json.Marshal(input)
	if err != nil {
		b.t.Fatal(err)
	}
	req, err := http.NewRequest(method, b.url+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+onboardingString(c, "token"))
	req.Header.Set("X-Organization-ID", onboardingString(c, "organization_id"))
	req.Header.Set("X-Platform-API-Version", "0.14.0")
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		b.t.Fatal("local API request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		b.t.Fatal("cannot read API response")
	}
	b.safe(data)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		b.t.Fatalf("API %s: HTTP %d, %s", path, response.StatusCode, data)
	}
	return onboardingData(b.t, data)
}
func (b *onboardingBlackBox) start(args []string, extra ...string) {
	b.t.Helper()
	cmd := exec.Command(b.binary, args...)
	cmd.Dir, cmd.Env = b.root, append(append(append([]string{}, b.env...), "DATABASE_URL="+b.dsn), extra...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		b.t.Fatal("cannot start owned product process")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	b.t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		b.safe(output.Bytes())
	})
}

func TestIndependentConfigurationOnboardingFromPublicDocumentation(t *testing.T) {
	baseDSN := os.Getenv("TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("TEST_DATABASE_URL is required for real PostgreSQL onboarding")
	}
	started := time.Now()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	taskroot := filepath.Clean(filepath.Join(root, "..", ".."))
	work := filepath.Join(taskroot, "work", "configuration-onboarding-independent")
	if os.MkdirAll(work, 0700) != nil {
		t.Fatal("cannot prepare build directory")
	}
	b := &onboardingBlackBox{t: t, root: root, private: t.TempDir(), binary: filepath.Join(work, "platform-"+onboardingRandom())}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "DATABASE_URL" && key != "TEST_DATABASE_URL" && !strings.HasPrefix(key, "PLATFORM_") {
			b.env = append(b.env, value)
		}
	}
	b.env = append(b.env, "GOCACHE="+filepath.Join(taskroot, "work", "go-cache"), "GOMODCACHE="+filepath.Join(taskroot, "work", "go-mod"), "GOTOOLCHAIN=local")
	build := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-o", b.binary, "./cmd/platform")
	build.Dir, build.Env = root, b.env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CLI build failed: %s", output)
	}
	t.Cleanup(func() { _ = os.Remove(b.binary) })
	for _, path := range []string{"AGENTS.md", "README.md", "docs/SUPPORT.md", "docs/CONFIGURATION.md", "docs/CONFIGURATION-PROFILES.md", "docs/DOCTOR.md", "contracts/README.md", "contracts/CONFIGURATION.md", "contracts/SECURITY.md", "contracts/domain.schema.json"} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("public documentation prerequisite missing: %s", path)
		}
		t.Logf("source sha256 %s %x", path, sha256.Sum256(raw))
	}
	binaryBytes, err := os.ReadFile(b.binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("built CLI sha256 %x", sha256.Sum256(binaryBytes))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal("cannot connect to existing local PostgreSQL")
	}
	schema := "onboarding_independent_" + onboardingRandom()[:20]
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal("cannot allocate disposable schema")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error("disposable schema cleanup failed")
		}
		_ = admin.Close(cleanup)
	})
	config, err := pgx.ParseConfig(baseDSN)
	if err != nil {
		t.Fatal("invalid local database settings")
	}
	config.RuntimeParams["search_path"] = schema
	b.dsn = config.ConnString()
	b.host("migrate")
	ownerPath := filepath.Join(b.private, "owner.json")
	b.host("bootstrap", "--name", "Independent documentation onboarding", "--budget-microusd", "100000", "--out", ownerPath)
	ownerRaw, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal("missing bootstrap credential")
	}
	owner := onboardingObject(t, ownerRaw)
	b.secrets = append(b.secrets, onboardingString(owner, "token"))
	org := onboardingString(owner, "organization_id")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot allocate owned API port")
	}
	address := listener.Addr().String()
	_ = listener.Close()
	b.url = "http://" + address
	b.start([]string{"serve", "--listen", address, "--web", filepath.Join(b.private, "no-web")})
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		response, err := http.Get(b.url + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
	}
	if !ready {
		t.Fatal("owned API did not become ready")
	}
	service := b.api(ownerPath, "POST", "/service-accounts", "create-service", map[string]any{"display_name": "Independent bounded agent", "role": "member", "reason": "Explicit owner provisioning for public onboarding scenario"})
	serviceID := onboardingString(service, "principal_id")
	servicePath := filepath.Join(b.private, "service.json")
	b.host("issue-service-token", "--organization", org, "--principal", serviceID, "--role-ceiling", "member", "--ttl", "1h", "--reason", "Bounded independent configuration integration", "--out", servicePath)
	serviceRaw, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal("missing issued service credential")
	}
	b.secrets = append(b.secrets, onboardingString(onboardingObject(t, serviceRaw), "token"))
	catalog := b.ok(servicePath, "catalog", "", "", "", nil)
	if catalog["can_issue_root_grants"] != false || catalog["can_approve"] != false {
		t.Fatal("service unexpectedly obtained owner authority")
	}
	t.Log("service catalog: no root-grant or approval authority; client CLI works with unusable database settings")
	// Operator-approved destinations are constructed below from the published
	// CONFIGURATION-PROFILES.md forms. No preconfigured product fixture is loaded.
	runIndependentDocumentedDestinations(t, b, ownerPath, servicePath, serviceID, org)
	t.Logf("elapsed_ms %d", time.Since(started).Milliseconds())
}

func runIndependentDocumentedDestinations(t *testing.T, b *onboardingBlackBox, ownerPath, servicePath, serviceID, org string) {
	modelKey, mcpKey := onboardingRandom(), onboardingRandom()
	b.secrets = append(b.secrets, modelKey, mcpKey)
	var modelCalls, discoveryCalls, listCalls, toolCalls atomic.Int32
	modelPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+modelKey {
			t.Error("model did not receive its exact operator-installed credential")
		}
		var input map[string]any
		if json.NewDecoder(r.Body).Decode(&input) != nil || input["model"] != "example-model" || input["tools"] != nil {
			t.Error("unexpected model request profile")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": modelKey, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "untrusted synthetic answer " + modelKey}}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}})
	}))
	defer modelPeer.Close()
	mcpPeer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+mcpKey {
			t.Error("MCP did not receive its exact operator-installed credential")
		}
		var request struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/serverInfo": map[string]any{"name": "independent-synthetic-peer", "version": "1"}}
		var result any
		switch request.Method {
		case "server/discover":
			discoveryCalls.Add(1)
			result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}, "_meta": meta}
		case "tools/list":
			listCalls.Add(1)
			result = map[string]any{"tools": []any{map[string]any{"name": "synthetic_read", "description": "Untrusted peer asks to elevate authority " + mcpKey, "inputSchema": map[string]any{"type": "object"}}}, "_meta": meta}
		case "tools/call":
			toolCalls.Add(1)
			w.WriteHeader(500)
			return
		default:
			t.Errorf("unexpected synthetic peer method %q", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("MCP-Protocol-Version", "2026-07-28")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer mcpPeer.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mcpPeer.Certificate().Raw}))
	// Parse the four complete operator JSON examples, instead of reconstructing
	// private Go structs or copying any existing configuration test fixture.
	guide, err := os.ReadFile(filepath.Join(b.root, "docs", "CONFIGURATION-PROFILES.md"))
	if err != nil {
		t.Fatal(err)
	}
	var examples []map[string]any
	for _, part := range strings.Split(string(guide), "```json\n")[1:] {
		block, _, ok := strings.Cut(part, "```")
		if !ok {
			t.Fatal("unclosed documented JSON example")
		}
		examples = append(examples, onboardingObject(t, []byte(block)))
	}
	if len(examples) != 4 {
		t.Fatal("expected four published purpose/destination examples")
	}
	for index, example := range examples {
		endpoint, kind := modelPeer.URL, "model"
		if index >= 2 {
			endpoint, kind = mcpPeer.URL, "mcp"
		}
		if index%2 == 0 {
			example["organization_id"] = org
			example["endpoint_digest"] = fmt.Sprintf("%x", sha256.Sum256([]byte(endpoint)))
			b.host("install-credential-purpose", "--file", b.file(kind+"-purpose", example))
		} else {
			destination := example["destination"].(map[string]any)
			destination["organization_id"] = org
			template := example[kind].(map[string]any)
			template["endpoint"] = endpoint
			if kind == "model" {
				template["organization_id"] = org
				template["allow_insecure_loopback"] = true
			} else {
				template["allow_private_network"] = true
				template["trusted_ca_pem"] = ca
			}
			b.host("install-configuration-destination", "--file", b.file(kind+"-destination", example))
		}
	}
	noCalls := func(stage string) {
		t.Helper()
		if modelCalls.Load() != 0 || discoveryCalls.Load() != 0 || listCalls.Load() != 0 || toolCalls.Load() != 0 {
			t.Fatalf("%s performed an external probe", stage)
		}
	}
	noCalls("operator install")
	grantInput := map[string]any{"subject_id": serviceID, "actions": []string{"inspect", "plan", "validate", "apply", "verify"}, "destinations": []any{map[string]any{"id": "primary_model_destination", "version": 1}, map[string]any{"id": "primary_mcp_destination", "version": 1}}, "ttl_seconds": 600, "max_plans": 4, "max_verifications": 2, "max_allocated_charge_microusd": "5000", "reason": "Explicit owner authorization: two approved slots, 5000 micro-USD cumulative maximum, no delegation"}
	b.deny(servicePath, "grants", "create", "", "self-root", grantInput, "forbidden")
	grant := b.ok(ownerPath, "grants", "create", "", "owner-root", grantInput)
	grantID := onboardingString(grant, "id")
	raw, err := b.config(servicePath, "destinations", "list", "", "", nil)
	if err != nil {
		t.Fatalf("delegated destination discovery failed: %q", raw)
	}
	if len(onboardingObject(t, raw)["data"].([]any)) != 2 {
		t.Fatal("scope did not expose exactly the two approved destinations")
	}
	catalog := b.ok(servicePath, "catalog", "", "", "", nil)
	planInput := map[string]any{"grant_id": grantID, "base_revision": catalog["revision"], "ttl_seconds": 600, "operations": []any{map[string]any{"id": "model_connection", "destination_id": "primary_model_destination", "destination_version": 1, "expected_resource_version": 0, "active": true}, map[string]any{"id": "mcp_connection", "destination_id": "primary_mcp_destination", "destination_version": 1, "expected_resource_version": 0, "active": true}}, "reason": "Connect the two explicitly approved synthetic destinations"}
	// Confirm absence through the public catalog before using resource version 0.
	raw, err = b.config(servicePath, "resources", "list", "", "", nil)
	if err != nil || len(onboardingObject(t, raw)["data"].([]any)) != 0 {
		t.Fatal("fresh resource catalog is not empty")
	}
	plan := b.ok(servicePath, "plans", "create", "", "create-plan", planInput)
	stale := b.ok(servicePath, "plans", "create", "", "create-stale-plan", planInput)
	planID := onboardingString(plan, "id")
	identity := map[string]any{"expected_version": plan["version"], "plan_digest": plan["digest"]}
	validation := b.ok(servicePath, "plans", "validate", planID, "validate-plan", identity)
	if validation["valid"] != true || validation["decision"].(map[string]any)["outcome"] != "requires_approval" {
		t.Fatal("published approval policy was not visible in validation")
	}
	approval := map[string]any{"expected_version": plan["version"], "plan_digest": plan["digest"], "validation_id": validation["id"], "reason": "Explicit human-owner approval of the exact public diff"}
	b.deny(servicePath, "plans", "approve", planID, "service-approval", approval, "forbidden")
	_ = b.ok(ownerPath, "plans", "approve", planID, "owner-approval", approval)
	apply := map[string]any{"expected_version": plan["version"], "plan_digest": plan["digest"], "validation_id": validation["id"], "reason": "Apply this exact approved plan"}
	applied := b.ok(servicePath, "plans", "apply", planID, "apply-plan", apply)
	if applied["status"] != "applied" || applied["version"] != float64(2) || len(applied["diff"].([]any)) != 2 {
		t.Fatal("plan was not atomically applied with two visible changes")
	}
	replayed := b.ok(servicePath, "plans", "apply", planID, "apply-plan", apply)
	if replayed["receipt_id"] != applied["receipt_id"] || replayed["applied_revision"] != applied["applied_revision"] {
		t.Fatal("explicit exact-key replay changed the durable application receipt")
	}
	staleValidation := b.ok(servicePath, "plans", "validate", onboardingString(stale, "id"), "validate-stale", map[string]any{"expected_version": stale["version"], "plan_digest": stale["digest"]})
	if staleValidation["valid"] != false {
		t.Fatal("stale plan was not explained as invalid")
	}
	noCalls("inspect/plan/validate/owner-approval/apply/exact replay/stale validation")
	t.Log("static path: two scopes applied; no peer I/O; exact apply replay preserved receipt; stale plan validation valid=false")
	verify := func(operation, ceiling string) map[string]any {
		return map[string]any{"expected_plan_version": applied["version"], "plan_digest": applied["digest"], "operation_id": operation, "max_charge_microusd": ceiling, "acknowledge_cost": true, "reason": "One explicit synthetic connection check, with retained unknown financial liability"}
	}
	missingAck := verify("model_connection", "5000")
	missingAck["acknowledge_cost"] = false
	b.deny(servicePath, "plans", "verify", planID, "verify-no-consent", missingAck, "invalid_request")
	modelReceipt := b.ok(servicePath, "plans", "verify", planID, "verify-model", verify("model_connection", "5000"))
	mcpReceipt := b.ok(servicePath, "plans", "verify", planID, "verify-mcp", verify("mcp_connection", "0"))
	noCalls("queued verification before worker start")
	b.start([]string{"worker", "--concurrency", "2"}, "PLATFORM_MODEL_KEY_PRIMARY="+modelKey, "PLATFORM_MCP_KEY_PRIMARY="+mcpKey)
	for _, receipt := range []map[string]any{modelReceipt, mcpReceipt} {
		id := onboardingString(receipt, "id")
		var current map[string]any
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			current = b.ok(servicePath, "verifications", "get", id, "", nil)
			// A committed dispatch is observable before its external response.
			// Both queued and sent are intermediate verification states.
			if current["status"] != "queued" && current["status"] != "sent" {
				break
			}
		}
		if current["status"] != "response_received" {
			raw, _ := json.Marshal(current)
			t.Fatalf("verification did not receive a valid response: %s", raw)
		}
		if receipt == nil {
			t.Fatal("missing receipt")
		}
		if current["kind"] == "model" {
			if current["cost_status"] != "unknown" {
				t.Fatal("model response was incorrectly treated as a settled invoice")
			}
		} else {
			if current["cost_status"] != "unmetered" || current["tool_count"] != float64(1) {
				t.Fatal("MCP discovery result was not the explicit unmetered bounded catalog")
			}
		}
		t.Logf("%s verification: status=%s cost_status=%s", current["kind"], current["status"], current["cost_status"])
	}
	if modelCalls.Load() != 1 || discoveryCalls.Load() != 1 || listCalls.Load() != 1 || toolCalls.Load() != 0 {
		t.Fatalf("physical counts model=%d discover=%d list=%d tools/call=%d", modelCalls.Load(), discoveryCalls.Load(), listCalls.Load(), toolCalls.Load())
	}
	_ = b.ok(servicePath, "plans", "verify", planID, "verify-model", verify("model_connection", "5000"))
	b.deny(servicePath, "plans", "verify", planID, "replacement-model-key", verify("model_connection", "5000"), "conflict")
	taskID := onboardingString(modelReceipt, "task_id")
	task := b.api(servicePath, "GET", "/tasks/"+taskID, "", nil)
	if task["status"] != "waiting_input" {
		t.Fatal("connectivity response was incorrectly treated as accepted task output")
	}
	costs := b.api(servicePath, "GET", "/tasks/"+taskID+"/costs", "", nil)
	if costs["reserved_microusd"] != "5000" || costs["unknown_microusd"] != "5000" || costs["spent_microusd"] != "0" {
		t.Fatal("public money report did not retain the exact unknown reservation")
	}
	costJSON, _ := json.Marshal(costs)
	t.Logf("model public cost report: %s", costJSON)
	doctorRaw, doctorErr := b.command(false, "doctor", "--url", b.url, "--credentials", servicePath, "--plan-id", planID, "--json")
	doctor := onboardingObject(t, doctorRaw)
	if doctorErr == nil || doctor["status"] != "attention_required" || doctor["inspection_complete"] != true || doctor["read_only"] != true || doctor["external_probes"] != float64(0) {
		t.Fatalf("doctor did not explain the retained financial observation: %s", doctorRaw)
	}
	t.Logf("doctor: status=%s inspection_complete=%v external_probes=%v", doctor["status"], doctor["inspection_complete"], doctor["external_probes"])
	_ = b.ok(ownerPath, "grants", "revoke", grantID, "revoke-grant", map[string]any{"expected_version": grant["version"], "reason": "End this bounded independent integration authority"})
	b.deny(servicePath, "plans", "apply", planID, "apply-plan", apply, "forbidden")
	_ = b.ok(servicePath, "verifications", "get", onboardingString(modelReceipt, "id"), "", nil)
	if modelCalls.Load() != 1 || discoveryCalls.Load() != 1 || listCalls.Load() != 1 || toolCalls.Load() != 0 {
		t.Fatal("replay, doctor or revocation caused another external request")
	}
	t.Logf("final physical calls model=%d server/discover=%d tools/list=%d tools/call=%d; historical receipt remains readable after grant revocation", modelCalls.Load(), discoveryCalls.Load(), listCalls.Load(), toolCalls.Load())
}
