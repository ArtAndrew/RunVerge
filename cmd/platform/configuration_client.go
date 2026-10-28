// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/jsonwire"
)

var configurationCLIIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type configurationCLICommand struct {
	method, path  string
	input, output reflect.Type
	list, exact   bool
}

func configurationCLICommandFor(group, action string) (configurationCLICommand, bool) {
	const base = "/api/v1/configuration"
	cmd := configurationCLICommand{method: http.MethodGet}
	switch group {
	case "capabilities":
		if action != "" {
			return cmd, false
		}
		cmd.path, cmd.output = "/api/v1/capabilities", reflect.TypeFor[domain.ProductCapabilities]()
	case "catalog":
		if action != "" {
			return cmd, false
		}
		cmd.path, cmd.output = base+"/catalog", reflect.TypeFor[domain.ConfigurationCatalog]()
	case "destinations", "resources", "audit":
		if action != "list" {
			return cmd, false
		}
		cmd.path, cmd.list = base+"/"+group, true
		switch group {
		case "destinations":
			cmd.output = reflect.TypeFor[domain.ConfigurationDestination]()
		case "resources":
			cmd.output = reflect.TypeFor[domain.ConfigurationResource]()
		case "audit":
			cmd.output = reflect.TypeFor[domain.ConfigurationAuditEntry]()
		}
	case "grants":
		cmd.path, cmd.output = base+"/grants", reflect.TypeFor[domain.CapabilityEnvelope]()
		switch action {
		case "list":
			cmd.list = true
		case "get":
			cmd.exact = true
		case "create":
			cmd.method, cmd.input = http.MethodPost, reflect.TypeFor[domain.CreateConfigurationGrant]()
		case "revoke":
			cmd.method, cmd.exact, cmd.input = http.MethodPost, true, reflect.TypeFor[domain.RevokeConfigurationGrant]()
		default:
			return cmd, false
		}
	case "plans":
		cmd.path, cmd.output = base+"/plans", reflect.TypeFor[domain.ConfigurationPlan]()
		switch action {
		case "list":
			cmd.list = true
		case "get":
			cmd.exact = true
		case "create":
			cmd.method, cmd.input = http.MethodPost, reflect.TypeFor[domain.CreateConfigurationPlan]()
		case "validate":
			cmd.method, cmd.exact, cmd.input, cmd.output = http.MethodPost, true, reflect.TypeFor[domain.ValidateConfigurationPlan](), reflect.TypeFor[domain.ConfigurationValidation]()
		case "approve":
			cmd.method, cmd.exact, cmd.input, cmd.output = http.MethodPost, true, reflect.TypeFor[domain.ApproveConfigurationPlan](), reflect.TypeFor[domain.ConfigurationApproval]()
		case "apply":
			cmd.method, cmd.exact, cmd.input = http.MethodPost, true, reflect.TypeFor[domain.ApplyConfigurationPlan]()
		case "verify":
			cmd.method, cmd.exact, cmd.input, cmd.output = http.MethodPost, true, reflect.TypeFor[domain.StartConfigurationVerification](), reflect.TypeFor[domain.ConfigurationVerification]()
		default:
			return cmd, false
		}
	case "verifications":
		cmd.path, cmd.output = base+"/verifications", reflect.TypeFor[domain.ConfigurationVerification]()
		switch action {
		case "list":
			cmd.list = true
		case "get":
			cmd.exact = true
		default:
			return cmd, false
		}
	default:
		return cmd, false
	}
	return cmd, true
}

// This client runs before any database is opened. It carries the same current
// credential and exact API-version header as SDK callers, with no host powers.
func runConfigurationClient(ctx context.Context, args []string, out io.Writer) error {
	return runConfigurationClientWithIdentity(ctx, args, out, nil)
}

// A composed read-only client can pin one credential for its entire inspection.
// The ordinary public CLI continues to load credentials once per invocation.
func runConfigurationClientWithIdentity(ctx context.Context, args []string, out io.Writer, pinned *mcpCredentialFile) error {
	usage := errors.New("usage: platform config catalog | destinations/resources/audit list | grants list/get/create/revoke | plans list/get/create/validate/approve/apply/verify | verifications list/get; flags --url --credentials --id --file --key --json")
	if len(args) == 0 {
		return usage
	}
	group, action, rest := args[0], "", args[1:]
	if group != "catalog" && group != "capabilities" {
		if len(rest) == 0 {
			return usage
		}
		action, rest = rest[0], rest[1:]
	}
	cmd, ok := configurationCLICommandFor(group, action)
	if !ok {
		return usage
	}
	f := flag.NewFlagSet("config", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	base := f.String("url", os.Getenv("PLATFORM_URL"), "platform HTTPS origin, or explicit loopback development HTTP")
	credentials := f.String("credentials", "", "private file emitted by platform bootstrap or token issuance")
	id := f.String("id", "", "exact object ID")
	file := f.String("file", "", "private command JSON file; sent unchanged")
	key := f.String("key", "", "explicit durable idempotency key")
	cursor := f.String("cursor", "", "opaque pagination cursor")
	limit := f.Int("limit", 50, "page size,1..100")
	planID := f.String("plan-id", "", "optional verification list filter")
	compact := f.Bool("json", false, "compact machine-readable JSON")
	if f.Parse(rest) != nil || f.NArg() != 0 {
		return usage
	}
	invalidFlag := false
	f.Visit(func(v *flag.Flag) {
		if (v.Name == "id" && !cmd.exact) || ((v.Name == "file" || v.Name == "key") && cmd.method != http.MethodPost) ||
			((v.Name == "cursor" || v.Name == "limit") && !cmd.list) || (v.Name == "plan-id" && !(group == "verifications" && cmd.list)) {
			invalidFlag = true
		}
	})
	if invalidFlag || *limit < 1 || *limit > 100 || len(*cursor) > 4096 || (cmd.exact && !configurationCLIIdentifier.MatchString(*id)) || (*planID != "" && !configurationCLIIdentifier.MatchString(*planID)) {
		return usage
	}
	u, err := url.Parse(*base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return errors.New("config requires a platform HTTPS origin, without credentials, query or path")
	}
	loopback := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback != nil && loopback.IsLoopback()) {
		return errors.New("config requires HTTPS; HTTP is limited to an explicit loopback address")
	}
	var identity mcpCredentialFile
	if pinned != nil {
		identity = *pinned
	} else {
		identity, err = configurationCLIIdentity(*credentials)
		if err != nil {
			return err
		}
	}
	var body []byte
	if cmd.method == http.MethodPost {
		if len(*key) < 1 || len(*key) > 128 {
			return errors.New("an explicit Idempotency-Key is required")
		}
		for _, ch := range *key {
			if ch < 33 || ch > 126 {
				return errors.New("invalid Idempotency-Key")
			}
		}
		body, err = readPrivateFile(*file, 1<<20)
		if err != nil || !configurationCLIValidJSON(body, cmd.input) {
			return errors.New("invalid private configuration command JSON")
		}
	}
	if cmd.exact {
		cmd.path += "/" + *id
	}
	if cmd.method == http.MethodPost && cmd.exact {
		cmd.path += "/" + action
	}
	u.Path = cmd.path
	q := url.Values{}
	if cmd.list {
		q.Set("limit", strconv.Itoa(*limit))
		if *cursor != "" {
			q.Set("cursor", *cursor)
		}
	}
	if *planID != "" {
		q.Set("plan_id", *planID)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, cmd.method, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create configuration request")
	}
	req.Header.Set("Authorization", "Bearer "+identity.Token)
	req.Header.Set("X-Organization-ID", identity.OrganizationID)
	req.Header.Set(domain.APIContractHeader, domain.ContractVersion)
	if cmd.method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", *key)
	}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 32768}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("configuration request was not acknowledged; read current state or repeat the same command and key explicitly")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("configuration response could not be read within its bound")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Do not print peer error messages or raw bodies: a peer can echo keys.
		code := "request_rejected"
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &failure) == nil {
			for _, known := range []string{"invalid_request", "unauthenticated", "forbidden", "not_found", "conflict", "incompatible_client", "budget_exceeded", "invalid_transition", "stale_lease", "unavailable", "internal_error"} {
				if failure.Error.Code == known {
					code = known
				}
			}
		}
		_ = json.NewEncoder(out).Encode(map[string]any{"error": map[string]any{"code": code, "status": response.StatusCode, "automatic_retry": false}})
		return errors.New("configuration command rejected: " + code)
	}
	if len(response.Header.Values(domain.APIContractHeader)) != 1 || response.Header.Get(domain.APIContractHeader) != domain.ContractVersion {
		return errors.New("configuration response has an incompatible API version")
	}
	dataType := cmd.output
	if cmd.list {
		dataType = reflect.SliceOf(dataType)
	}
	envelopeType := reflect.StructOf([]reflect.StructField{{Name: "Data", Type: dataType, Tag: `json:"data"`}, {Name: "NextCursor", Type: reflect.TypeFor[string](), Tag: `json:"next_cursor,omitempty"`}})
	if !configurationCLIValidResponse(raw, envelopeType, cmd, identity.OrganizationID, *id, *planID, action, identity.Token, *limit, body) {
		return errors.New("configuration response violated the public shape; no response body was printed")
	}
	var formatted bytes.Buffer
	if *compact {
		err = json.Compact(&formatted, raw)
	} else {
		err = json.Indent(&formatted, raw, "", "  ")
	}
	if err != nil {
		return errors.New("invalid configuration response")
	}
	_, err = fmt.Fprintln(out, formatted.String())
	return err
}

func configurationCLIIdentity(path string) (mcpCredentialFile, error) {
	var identity mcpCredentialFile
	if path != "" {
		if readPrivateJSON(path, &identity) != nil {
			return identity, errors.New("invalid private platform credentials file")
		}
	} else {
		identity.Token, identity.OrganizationID = os.Getenv("PLATFORM_TOKEN"), os.Getenv("PLATFORM_ORGANIZATION_ID")
	}
	if identity.Token == "" || len(identity.Token) > 4096 || strings.ContainsAny(identity.Token, "\r\n\x00 \t") || !configurationCLIIdentifier.MatchString(identity.OrganizationID) {
		return identity, errors.New("a current platform token and organization are required via a private credentials file or environment")
	}
	return identity, nil
}

func configurationCLIValidJSON(raw []byte, typ reflect.Type) bool {
	if len(raw) == 0 || !jsonwire.ValidUnicode(raw) || !json.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if strictConfigValue(d, typ, 0) != nil {
		return false
	}
	if _, err := d.Token(); err != io.EOF {
		return false
	}
	target := reflect.New(typ).Interface()
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target) == nil
}
