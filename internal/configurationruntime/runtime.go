// SPDX-License-Identifier: Elastic-2.0
// Package configurationruntime performs one explicitly admitted unmetered MCP
// discovery occurrence. It never installs executable tools or calls a tool.
package configurationruntime

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
)

const MaxDiscoveryTools = 128
const MaxDiscoveryPages = 16

type Store interface {
	PrepareConfigurationVerificationDispatch(context.Context, *domain.ConfigurationVerificationLease) (domain.ConfigurationDispatchPermit, error)
	RecordConfigurationVerificationObservation(context.Context, domain.ConfigurationProbeObservation) (domain.ConfigurationVerification, error)
	PauseConfigurationVerification(context.Context, *domain.ConfigurationVerificationLease, string) error
}

type Config struct {
	Probe      domain.ConfigurationProbeConfig
	Credential string
}

func (Config) String() string   { return "configurationruntime.Config{redacted}" }
func (Config) GoString() string { return "configurationruntime.Config{redacted}" }

type Outcome struct{ Status, VerificationID string }

// No error wraps a peer body, URL, certificate, key, tool name or description.
var errExecution = errors.New("configuration verification requires attention")

func Execute(ctx context.Context, store Store, lease *domain.ConfigurationVerificationLease, config Config) (Outcome, error) {
	out := Outcome{Status: "not_dispatched"}
	if ctx == nil || store == nil || lease == nil {
		return out, errExecution
	}
	out.VerificationID = lease.ID
	d := config.Probe.Destination
	p := config.Probe.Purpose
	if d.MCP == nil || d.Model != nil || d.Destination.Kind != "mcp" || !d.Destination.Active || !d.Destination.DiscoveryUnmeteredApproved || d.Destination.MaxVerifyChargeMicroUSD != 0 ||
		p.Kind != "mcp" || !p.Active || p.OrganizationID != lease.OrganizationID || d.Destination.OrganizationID != lease.OrganizationID ||
		p.ID != d.Destination.CredentialPurposeID || p.Version != d.Destination.CredentialPurposeVersion || d.MCP.TimeoutSeconds < 1 || d.MCP.TimeoutSeconds > 30 || config.Credential == "" {
		return out, errExecution
	}
	endpointHash := sha256.Sum256([]byte(d.MCP.Endpoint))
	if hex.EncodeToString(endpointHash[:]) != p.EndpointDigest {
		return out, errExecution
	}
	scope := domain.ConfigurationScope{DestinationID: d.Destination.ID, DestinationVersion: d.Destination.Version, Kind: d.Destination.Kind, TargetID: d.Destination.TargetID, CredentialPurposeID: p.ID, CredentialPurposeVersion: p.Version}
	if lease.Scope != scope || lease.ResourceVersion < 1 || len(lease.ProfileDigest) != 64 || lease.ProfileDigest != domain.ConfigurationProbeDigest(config.Probe, lease.ResourceVersion) {
		return out, errExecution
	}
	var roots *x509.CertPool
	if d.MCP.TrustedCAPEM != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(d.MCP.TrustedCAPEM)) {
			return out, errExecution
		}
	}
	started := time.Now()
	out.Status = "unknown"
	permit, err := store.PrepareConfigurationVerificationDispatch(ctx, lease)
	if err != nil {
		return out, errExecution
	}
	deadline := started.Add(permit.ValidFor)
	if permit.OrganizationID != lease.OrganizationID || permit.VerificationID != lease.ID || permit.WorkerID != lease.WorkerID || permit.FencingToken != lease.Token || permit.IssuedAt.IsZero() || permit.ValidFor <= 0 || permit.ValidFor > 2*time.Second || permit.NotAfter.Sub(permit.IssuedAt) != permit.ValidFor || !time.Now().Before(deadline) {
		return out, errExecution
	}
	if permit.Scope != lease.Scope || permit.ResourceVersion != lease.ResourceVersion || permit.ProfileDigest != lease.ProfileDigest {
		return out, errExecution
	}
	execute, cancel := context.WithTimeout(ctx, time.Duration(d.MCP.TimeoutSeconds)*time.Second)
	defer cancel()
	client, err := mcpbridge.Connect(execute, mcpbridge.ClientConfig{
		Endpoint: d.MCP.Endpoint, AccessToken: config.Credential, OrganizationID: lease.OrganizationID,
		AllowPrivateNetwork: d.MCP.AllowPrivateNetwork, TrustedRoots: roots, Timeout: time.Duration(d.MCP.TimeoutSeconds) * time.Second,
		MaxConcurrent: 1, DispatchDeadline: deadline, DispatchIdempotencyKey: "configuration:" + lease.ID,
		// AuthorizeCall deliberately remains nil: peer tool hints confer no power.
	})
	config.Credential = ""
	if err != nil {
		return out, errExecution
	}
	defer client.Close()
	hash := sha256.New()
	cursor, totalTools, totalBytes := "", 0, 0
	seen := map[string]bool{}
	complete := false
	for page := 0; page < MaxDiscoveryPages; page++ {
		result, listErr := client.ListTools(execute, cursor)
		if listErr != nil || result == nil {
			return out, errExecution
		}
		raw, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return out, errExecution
		}
		totalBytes += len(raw)
		totalTools += len(result.Tools)
		if totalBytes > mcpbridge.MaxResponseBytes || totalTools > MaxDiscoveryTools || len(result.NextCursor) > 4096 {
			return out, errExecution
		}
		// Page boundaries are framed, so distinct page sequences cannot produce
		// the same byte stream through ambiguous concatenation.
		frame, _ := json.Marshal(struct {
			Page int             `json:"page"`
			Body json.RawMessage `json:"body"`
		}{page, raw})
		_, _ = hash.Write(frame)
		if result.NextCursor == "" {
			complete = true
			break
		}
		if seen[result.NextCursor] {
			return out, errExecution
		}
		seen[result.NextCursor] = true
		cursor = result.NextCursor
	}
	if !complete {
		return out, errExecution
	}
	observation := domain.ConfigurationProbeObservation{OrganizationID: lease.OrganizationID, VerificationID: lease.ID, WorkerID: lease.WorkerID, FencingToken: lease.Token,
		Outcome: "response_received", ProtocolVersion: mcpbridge.ProtocolVersion, ToolCount: totalTools, CatalogDigest: hex.EncodeToString(hash.Sum(nil))}
	persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	receipt, err := store.RecordConfigurationVerificationObservation(persist, observation)
	if err != nil || receipt.OrganizationID != lease.OrganizationID || receipt.ID != lease.ID || receipt.Status != "response_received" {
		return out, errExecution
	}
	out.Status = "response_received"
	return out, nil
}
