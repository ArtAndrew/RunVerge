// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/storage"
)

// Accept the exact private files emitted by bootstrap, issue-token and
// issue-service-token. Optional issuance metadata is never authorization: the
// current credential, organization and principal are checked in PostgreSQL.
type mcpCredentialFile struct {
	OrganizationID string            `json:"organization_id"`
	PrincipalID    string            `json:"principal_id"`
	Token          string            `json:"token"`
	ExpiresAt      time.Time         `json:"expires_at,omitempty"`
	Key            domain.ServiceKey `json:"key,omitempty"`
}

func splitMCPList(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	values := strings.Split(raw, ",")
	for i, value := range values {
		values[i] = strings.TrimSpace(value)
		if values[i] == "" {
			return nil, errors.New("empty MCP configuration value")
		}
	}
	return values, nil
}

func mcpHTTPHandler(api http.Handler, enabled bool, mutations, origins string) (http.Handler, error) {
	if !enabled {
		if mutations != "" || origins != "" {
			return nil, errors.New("MCP configuration requires --mcp")
		}
		return api, nil
	}
	allowed, e := splitMCPList(mutations)
	if e != nil {
		return nil, e
	}
	allowedOrigins, e := splitMCPList(origins)
	if e != nil {
		return nil, e
	}
	bridge, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: mcpbridge.HandlerBackend(api), AllowedMutations: allowed, AllowedOrigins: allowedOrigins})
	if e != nil {
		return nil, errors.New("invalid private MCP profile")
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", bridge.Handler())
	mux.Handle("/", api)
	return mux, nil
}

func runMCPStdio(ctx context.Context, s *storage.Store, args []string) error {
	f := flag.NewFlagSet("mcp-stdio", flag.ContinueOnError)
	path := f.String("credentials", "", "private platform credential JSON file (0600)")
	mutations := f.String("allow-mutations", "", "explicit comma-separated MCP mutation tool names; default read-only")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 || *path == "" {
		return errors.New("mcp-stdio requires an explicit private credentials file")
	}
	var credentials mcpCredentialFile
	if readPrivateJSON(*path, &credentials) != nil {
		return errors.New("invalid private MCP credentials file")
	}
	if _, e := s.Authenticate(ctx, credentials.Token, credentials.OrganizationID); e != nil {
		return errors.New("MCP principal is not currently authorized")
	}
	allowed, e := splitMCPList(*mutations)
	if e != nil {
		return e
	}
	api := (&httpapi.Server{Store: s, Log: slog.Default()}).Handler()
	bridge, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: mcpbridge.HandlerBackend(api), AllowedMutations: allowed})
	if e != nil {
		return errors.New("invalid private MCP mutation allowlist")
	}
	return bridge.RunStdio(ctx, mcpbridge.Credentials{Token: credentials.Token, OrganizationID: credentials.OrganizationID})
}
