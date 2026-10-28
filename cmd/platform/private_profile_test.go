// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func privateProfileFile(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved-profile.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func privateDestinationFixture(kind string) domain.ConfigurationDestinationProfile {
	p := domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{
		ID: "destination-fixture", OrganizationID: "org_fixture", Version: 1, Kind: kind,
		Name: "Approved private destination", TargetID: "fixed-connection", CredentialPurposeID: "purpose-fixture",
		CredentialPurposeVersion: 1, Active: true,
	}}
	if kind == "model" {
		p.Destination.Model, p.Destination.MaxVerifyChargeMicroUSD = "model-fixture", 300
		p.Model = &domain.ModelConnection{ID: "fixed-connection", OrganizationID: "org_fixture", Version: 1,
			Model: "model-fixture", ProviderAccount: "fixture-account", PriceVersion: "fixture-price-v1",
			Endpoint: "https://approved-model.invalid/v1/chat/completions", MaxChargeMicroUSD: 300,
			MaxInputTokens: 100, MaxOutputTokens: 64, InputMicroUSDPerMillion: 1000000, OutputMicroUSDPerMillion: 2000000,
			FixedMicroUSD: 7, MaxInputBytes: 65536, MaxOutputBytes: 65536, TimeoutMilliseconds: 2000,
			BoundAssumptions: "Private parser fixture; no external request", TokenLimitField: "max_tokens", Active: true}
	} else {
		p.Destination.DiscoveryUnmeteredApproved = true
		p.MCP = &domain.MCPConnectionProfile{Endpoint: "https://approved-mcp.invalid/mcp", TimeoutSeconds: 3}
	}
	return p
}

func TestPrivateConfigurationDestinationFilesDecodeNestedProfiles(t *testing.T) {
	for _, kind := range []string{"model", "mcp"} {
		t.Run(kind, func(t *testing.T) {
			want := privateDestinationFixture(kind)
			raw, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var got domain.ConfigurationDestinationProfile
			if err = readPrivateJSON(privateProfileFile(t, raw), &got); err != nil || !reflect.DeepEqual(want, got) {
				t.Fatal("private nested profile was rejected or changed", err)
			}
			if (kind == "model" && (got.Model == nil || got.MCP != nil)) || (kind == "mcp" && (got.MCP == nil || got.Model != nil)) {
				t.Fatal("omitted alternative pointer must remain nil")
			}
		})
	}
}

func TestPrivateConfigurationDestinationFilesRejectNestedAmbiguity(t *testing.T) {
	for _, kind := range []string{"model", "mcp"} {
		t.Run(kind, func(t *testing.T) {
			raw, err := json.Marshal(privateDestinationFixture(kind))
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			endpoint := `"endpoint":"https://approved-` + kind + `.invalid/`
			if !strings.Contains(body, endpoint) {
				t.Fatal("fixture does not contain the exact nested field")
			}
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			cases := map[string][]byte{
				"duplicate":     []byte(strings.Replace(body, endpoint, `"endpoint":"https://shadow.invalid/","endpoint":"https://approved-`+kind+`.invalid/`, 1)),
				"unknown":       []byte(strings.Replace(body, endpoint, `"raw_token":"untrusted-extra","endpoint":"https://approved-`+kind+`.invalid/`, 1)),
				"nested_null":   []byte(strings.Replace(body, `"allow_private_network":false`, `"allow_private_network":null`, 1)),
				"wrong_shape":   []byte(strings.Replace(body, `"`+kind+`":{`, `"`+kind+`":[{`, 1)),
				"invalid_utf8":  []byte(strings.Replace(body, "Approved private destination", string([]byte{0xff}), 1)),
				"bad_surrogate": []byte(strings.Replace(body, "Approved private destination", `\ud800`, 1)),
				"nul_string":    []byte(strings.Replace(body, "Approved private destination", `\u0000`, 1)),
				"trailing_json": append(append([]byte{}, raw...), []byte(` {"unexpected":true}`)...),
			}
			if kind == "model" {
				cases["nested_null"] = []byte(strings.Replace(body, `"allow_insecure_loopback":false`, `"allow_insecure_loopback":null`, 1))
			}
			for _, value := range []string{"null", "false", `"invalid-profile"`, "[]"} {
				fields[kind] = json.RawMessage(value)
				changed, e := json.Marshal(fields)
				if e != nil {
					t.Fatal(e)
				}
				cases["pointer_"+value] = changed
			}
			for name, invalid := range cases {
				t.Run(name, func(t *testing.T) {
					var got domain.ConfigurationDestinationProfile
					if err := readPrivateJSON(privateProfileFile(t, invalid), &got); err == nil {
						t.Fatal("ambiguous or malformed private profile accepted")
					}
				})
			}
		})
	}
}

func TestPrivateJSONPointerFieldsKeepOmittedAndExplicitFalseDistinct(t *testing.T) {
	type nested struct {
		Active *bool `json:"active,omitempty"`
	}
	type shape struct {
		Model *nested `json:"model,omitempty"`
		Flags *[]bool `json:"flags,omitempty"`
	}
	var omitted shape
	if err := readPrivateJSON(privateProfileFile(t, []byte(`{}`)), &omitted); err != nil || omitted.Model != nil || omitted.Flags != nil {
		t.Fatal("omitted optional pointer should remain nil", err)
	}
	var present shape
	if err := readPrivateJSON(privateProfileFile(t, []byte(`{"model":{"active":false},"flags":[false,true]}`)), &present); err != nil || present.Model == nil || present.Model.Active == nil || *present.Model.Active || present.Flags == nil || !reflect.DeepEqual(*present.Flags, []bool{false, true}) {
		t.Fatal("pointer field lost its explicit value or type", err)
	}
	for _, body := range []string{`{"model":null}`, `{"model":{"active":null}}`, `{"flags":null}`, `{"flags":[null]}`, `{"model":{"active":0}}`, `{"flags":{}}`, `{"flags":["false"]}`, `{"model":{"active":false,"active":true}}`} {
		t.Run(body, func(t *testing.T) {
			var got shape
			if err := readPrivateJSON(privateProfileFile(t, []byte(body)), &got); err == nil {
				t.Fatal("explicit null, incompatible type, or duplicate accepted")
			}
		})
	}
}
