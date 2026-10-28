// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/capabilities"
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
)

func capabilityDTOs() map[string]reflect.Type {
	return map[string]reflect.Type{"ProductCapabilities": reflect.TypeFor[domain.ProductCapabilities](), "CapabilityOperation": reflect.TypeFor[domain.CapabilityOperation](), "CapabilityBinding": reflect.TypeFor[domain.CapabilityBinding](), "CapabilityProfile": reflect.TypeFor[domain.CapabilityProfile](), "CapabilityRole": reflect.TypeFor[domain.CapabilityRole](), "CapabilityOnboarding": reflect.TypeFor[domain.CapabilityOnboarding](), "CapabilityStep": reflect.TypeFor[domain.CapabilityStep]()}
}

var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

func capabilityOperation(path, method, input, output, status string, list bool) domain.CapabilityOperation {
	id := method + strings.NewReplacer("/", "_", "{", "", "}", "").Replace(path)
	code, err := strconv.Atoi(status)
	if err != nil {
		panic(err)
	}
	kind := "object"
	if list {
		kind = "list"
	}
	if strings.HasSuffix(path, "/event-stream") {
		kind = "sse"
	}
	o := capabilities.Describe(domain.CapabilityOperation{ID: id, Method: strings.ToUpper(method), Path: "/api/v1" + path, InputSchema: input, OutputSchema: output, ResponseKind: kind, SuccessStatus: code})
	o.Bindings = []domain.CapabilityBinding{{Surface: "http", Status: "available", Name: o.Method + " " + o.Path}}
	cli := ""
	if path == "/capabilities" {
		cli = "platform capabilities"
	}
	if strings.HasPrefix(path, "/configuration/") {
		parts := strings.Split(strings.TrimPrefix(path, "/configuration/"), "/")
		if parts[0] == "catalog" {
			cli = "platform config catalog"
		} else {
			action := "list"
			if method == "post" {
				action = "create"
			} else if len(parts) > 1 {
				action = "get"
			}
			if len(parts) > 2 {
				action = parts[2]
			}
			cli = "platform config " + parts[0] + " " + action
		}
	}
	b := domain.CapabilityBinding{Surface: "cli", Status: "not_exposed"}
	if cli != "" {
		b.Status = "available"
		b.Name = cli
	}
	o.Bindings = append(o.Bindings, b)
	o.Bindings = append(o.Bindings, capabilities.SurfaceBindings(o.Method, o.Path)...)
	b = domain.CapabilityBinding{Surface: "mcp", Status: "not_exposed"}
	tools, err := mcpbridge.PublicToolRoutes()
	if err != nil {
		panic(err)
	}
	for _, tool := range tools {
		if tool.Method == o.Method && pathParameter.ReplaceAllString(tool.Path, "{}") == pathParameter.ReplaceAllString(o.Path, "{}") {
			if b.Name != "" {
				panic("ambiguous MCP binding")
			}
			b.Status = "available"
			if tool.OperatorOptIn {
				b.Status = "operator_opt_in"
			}
			b.Name = tool.Name
		}
	}
	o.Bindings = append(o.Bindings, b)
	return o
}

func writeCapabilities(operations []domain.CapabilityOperation) {
	d := capabilities.NewDefinition(operations)
	observation, err := d.Observation(domain.Principal{ID: "catalog_fixture", OrganizationID: "org_catalog_fixture", Kind: "human", Role: "viewer"}, time.Unix(1, 0))
	if err != nil {
		panic(err)
	}
	write("tests/contracts/fixtures/product-capabilities.json", observation)
	write("contracts/capability-catalog.json", d)
	write("internal/capabilities/catalog.json", d)
}

func refineCapabilitySchema(name string, result schema) schema {
	if _, ok := capabilityDTOs()[name]; !ok {
		return result
	}
	p := result["properties"].(map[string]any)
	for _, raw := range p {
		v := raw.(schema)
		if v["type"] == "string" {
			v["maxLength"] = 4096
			v["x-max-utf8-bytes"] = 4096
		}
	}
	for field, limit := range map[string]int{"authorization_rules": 4096, "remediation_command": 512, "runtime_guards": 128, "documentation": 256, "constraints": 4096, "prerequisites": 4096, "permissions": 128, "operation_ids": 128} {
		if raw, ok := p[field]; ok {
			v := raw.(schema)
			if v["type"] == "array" {
				item := v["items"].(schema)
				item["minLength"] = 1
				item["maxLength"] = limit
				item["x-max-utf8-bytes"] = limit
				item["pattern"] = nonblankPattern
			}
		}
	}
	enum := func(field string, values ...string) { p[field].(schema)["enum"] = values }
	array := func(field string, lo, hi int) { v := p[field].(schema); v["minItems"] = lo; v["maxItems"] = hi }
	bounded := func(field string, max int, empty bool) {
		v := p[field].(schema)
		v["maxLength"] = max
		v["x-max-utf8-bytes"] = max
		if !empty {
			v["minLength"] = 1
			v["pattern"] = nonblankPattern
		}
	}
	switch name {
	case "ProductCapabilities":
		enum("catalog_version", "1")
		enum("api_contract", domain.ContractVersion)
		enum("assessment", "support_and_role_observation")
		p["read_only"].(schema)["const"] = true
		array("operations", 1, 128)
		array("profiles", 1, 32)
		array("roles", 6, 6)
		array("onboarding", 6, 6)
		array("authorization_rules", 1, 32)
		array("remediation_command", 1, 20)
		p["remediation_command"].(schema)["prefixItems"] = []any{schema{"const": "platform"}}
		identity := p["identity"].(schema)
		required := identity["required"].([]string)
		identity["required"] = append(required, "principal_kind")
		fields := identity["properties"].(map[string]any)
		fields["principal_kind"].(schema)["enum"] = []string{"human", "service"}
		fields["role"].(schema)["enum"] = []string{"viewer", "member", "admin", "owner"}
		fields["permissions"].(schema)["maxItems"] = 64
	case "CapabilityOperation":
		enum("method", "GET", "POST")
		enum("response_kind", "object", "list", "sse")
		enum("access_model", "public", "current_membership", "role_and_resource", "scoped_configuration", "human_owner")
		enum("effect", "read_only", "platform_write", "queues_external_work", "external_dispatch", "records_observation")
		enum("cost_semantics", "none", "may_reserve_and_queue", "reserves_upper_bound", "possible_charge_existing_reservation", "observation_no_settlement")
		enum("retry_policy", "read_with_current_authorization", "explicit_same_key_after_inspection", "scoped_stream_resume")
		p["success_status"].(schema)["enum"] = []int{200, 201}
		array("bindings", 6, 6)
		array("runtime_guards", 1, 32)
		array("documentation", 1, 8)
		array("permission_alternatives", 0, 4)
		p["permission_alternatives"].(schema)["items"] = schema{"type": "array", "minItems": 1, "maxItems": 4, "items": schema{"type": "string", "minLength": 1, "maxLength": 128, "x-max-utf8-bytes": 128}}
		for _, f := range []string{"id", "capability_id", "output_schema"} {
			bounded(f, 128, false)
		}
		bounded("input_schema", 128, true)
		bounded("path", 512, false)
	case "CapabilityBinding":
		enum("surface", "http", "cli", "python", "typescript", "mcp", "workspace")
		enum("status", "available", "operator_opt_in", "not_exposed")
		bounded("name", 512, true)
	case "CapabilityProfile":
		enum("status", "implemented", "bounded", "not_implemented")
		array("constraints", 1, 32)
		array("documentation", 1, 8)
		bounded("id", 128, false)
	case "CapabilityRole":
		enum("principal_kind", "human", "service")
		enum("role", "viewer", "member", "admin", "owner")
		array("permissions", 0, 64)
		result["allOf"] = []any{schema{"if": schema{"properties": schema{"principal_kind": schema{"const": "service"}}}, "then": schema{"properties": schema{"role": schema{"enum": []string{"viewer", "member"}}}}}}
	case "CapabilityOnboarding":
		enum("status", "available", "partial", "not_implemented")
		array("prerequisites", 1, 32)
		array("steps", 0, 12)
		array("documentation", 1, 8)
		bounded("id", 128, false)
	case "CapabilityStep":
		array("operation_ids", 0, 16)
		array("commands", 0, 4)
		p["commands"].(schema)["items"] = schema{"type": "array", "minItems": 1, "maxItems": 20, "prefixItems": []any{schema{"const": "platform"}}, "items": schema{"type": "string", "minLength": 1, "maxLength": 512, "x-max-utf8-bytes": 512}}
		enum("effect", "read_only", "platform_write", "queues_external_work", "external_dispatch", "records_observation")
		bounded("id", 128, false)
	}
	return result
}
