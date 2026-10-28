// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"github.com/jackc/pgx/v5"
)

var configurationIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var configurationDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var mcpCredentialReference = regexp.MustCompile(`^PLATFORM_MCP_KEY_[A-Z0-9_]{1,64}$`)

func configurationProfileInvalid() error {
	return domain.Err("invalid_request", "Некорректный доверенный профиль настройки", 400)
}
func configurationProfileConflict() error {
	return domain.Err("conflict", "Версия доверенного профиля уже имеет другое содержимое или нарушает порядок версий", 409)
}

// The IAM gate precedes authority locks; the configuration gate precedes every
// configuration writer's organization/profile locks. Neither admits a caller.
func configurationGateTx(ctx context.Context, tx pgx.Tx, org string) error {
	if err := organizationIAMGate(ctx, tx, org); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "organization-configuration-administration\x1f"+org)
	return err
}

func lockConfigurationOrganizationTx(ctx context.Context, tx pgx.Tx, org string) error {
	if err := lockOrganizationPolicyTx(ctx, tx, org); err != nil {
		return err
	}
	return lockConfigurationOrganizationOnlyTx(ctx, tx, org)
}

func lockConfigurationOrganizationOnlyTx(ctx context.Context, tx pgx.Tx, org string) error {
	var active bool
	if err := tx.QueryRow(ctx, "SELECT active FROM organizations WHERE id=$1 FOR UPDATE", org).Scan(&active); err != nil {
		return rowExists(err)
	}
	if !active {
		return domain.Forbidden
	}
	return nil
}

func normalizeCredentialPurpose(c domain.CredentialPurposeProfile) (domain.CredentialPurposeProfile, error) {
	if !configurationIdentifier.MatchString(c.ID) || !configurationIdentifier.MatchString(c.OrganizationID) || c.Version < 1 || !configurationDigest.MatchString(c.EndpointDigest) {
		return c, configurationProfileInvalid()
	}
	if (c.Kind != "model" || !modelCredentialReference.MatchString(c.CredentialEnvironment)) && (c.Kind != "mcp" || !mcpCredentialReference.MatchString(c.CredentialEnvironment)) {
		return c, configurationProfileInvalid()
	}
	c.Active = false
	c.CreatedAt = time.Time{}
	return c, nil
}

func scanCredentialPurposeProfile(row scanner) (c domain.CredentialPurposeProfile, err error) {
	var raw []byte
	var active bool
	var created time.Time
	if err = row.Scan(&raw, &active, &created); err != nil {
		return c, rowExists(err)
	}
	if len(raw) > 4096 || json.Unmarshal(raw, &c) != nil {
		return c, configurationProfileConflict()
	}
	c, err = normalizeCredentialPurpose(c)
	if err != nil {
		return c, configurationProfileConflict()
	}
	c.Active, c.CreatedAt = active, created
	return c, nil
}

func loadCredentialPurposeProfileTx(ctx context.Context, tx pgx.Tx, org, id string, version int, lock bool) (domain.CredentialPurposeProfile, error) {
	query := "SELECT profile,active,created_at FROM credential_purpose_profiles WHERE organization_id=$1 AND id=$2 AND version=$3"
	if lock {
		query += " FOR SHARE"
	}
	c, err := scanCredentialPurposeProfile(tx.QueryRow(ctx, query, org, id, version))
	if err == nil && (c.OrganizationID != org || c.ID != id || c.Version != version) {
		err = configurationProfileConflict()
	}
	return c, err
}

func appendConfigurationAuditTx(ctx context.Context, tx pgx.Tx, org, actorType, actorID, objectType, objectID, action, digest, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO configuration_audit(organization_id,id,actor_type,actor_id,object_type,object_id,action,digest,reason)
 VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9)`, org, domain.NewID("cfg_audit"), actorType, actorID, objectType, objectID, action, digest, reason)
	return err
}

// InstallCredentialPurposeProfile is available only to the trusted host. It
// persists a restricted secret reference, never resolves or accepts a raw key.
// An identical immutable version does not reactivate or rewrite an old record.
func (s *Store) InstallCredentialPurposeProfile(ctx context.Context, in domain.CredentialPurposeProfile) (domain.CredentialPurposeProfile, error) {
	var empty domain.CredentialPurposeProfile
	c, err := normalizeCredentialPurpose(in)
	if err != nil {
		return empty, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return empty, err
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = configurationGateTx(ctx, tx, c.OrganizationID); err != nil {
		return empty, err
	}
	if err = lockConfigurationOrganizationTx(ctx, tx, c.OrganizationID); err != nil {
		return empty, err
	}
	existing, err := loadCredentialPurposeProfileTx(ctx, tx, c.OrganizationID, c.ID, c.Version, true)
	if err == nil {
		old, _ := normalizeCredentialPurpose(existing)
		if digestValue(old) != Hash(raw) {
			return empty, configurationProfileConflict()
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, domain.NotFound) {
		return empty, err
	}
	var latest int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM credential_purpose_profiles WHERE organization_id=$1 AND id=$2", c.OrganizationID, c.ID).Scan(&latest); err != nil {
		return empty, err
	}
	if c.Version != latest+1 {
		return empty, configurationProfileConflict()
	}
	if latest > 0 {
		previous, e := loadCredentialPurposeProfileTx(ctx, tx, c.OrganizationID, c.ID, latest, true)
		if e != nil {
			return empty, e
		}
		if previous.Kind != c.Kind {
			return empty, configurationProfileConflict()
		}
	}
	if in.Active {
		if _, err = tx.Exec(ctx, "UPDATE credential_purpose_profiles SET active=false WHERE organization_id=$1 AND id=$2 AND active", c.OrganizationID, c.ID); err != nil {
			return empty, err
		}
	}
	created, err := scanCredentialPurposeProfile(tx.QueryRow(ctx, `INSERT INTO credential_purpose_profiles(organization_id,id,version,kind,endpoint_digest,profile,active)
 VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING profile,active,created_at`, c.OrganizationID, c.ID, c.Version, c.Kind, c.EndpointDigest, raw, in.Active))
	if err != nil {
		return empty, err
	}
	if err = appendConfigurationAuditTx(ctx, tx, c.OrganizationID, "trusted_host", "", "credential_purpose", c.ID, "credential_purpose.installed", Hash(raw), "Trusted host credential purpose installation"); err != nil {
		return empty, err
	}
	return created, tx.Commit(ctx)
}

func validateMCPConnectionProfile(c domain.MCPConnectionProfile) error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || len(c.Endpoint) > 2048 || !validText(c.Endpoint, 2048) || strings.ContainsAny(c.Endpoint, "\x00\r\n") || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 30 {
		return configurationProfileInvalid()
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return configurationProfileInvalid()
		}
	}
	if len(c.TrustedCAPEM) > 65536 {
		return configurationProfileInvalid()
	}
	remaining := strings.TrimSpace(c.TrustedCAPEM)
	for remaining != "" {
		if !strings.HasPrefix(remaining, "-----BEGIN CERTIFICATE-----") {
			return configurationProfileInvalid()
		}
		block, tail := pem.Decode([]byte(remaining))
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return configurationProfileInvalid()
		}
		if _, err = x509.ParseCertificate(block.Bytes); err != nil {
			return configurationProfileInvalid()
		}
		remaining = strings.TrimSpace(string(tail))
	}
	return nil
}

func normalizeConfigurationDestination(c domain.ConfigurationDestinationProfile) (domain.ConfigurationDestinationProfile, string, error) {
	d := &c.Destination
	if !configurationIdentifier.MatchString(d.ID) || !configurationIdentifier.MatchString(d.OrganizationID) || !configurationIdentifier.MatchString(d.TargetID) || !configurationIdentifier.MatchString(d.CredentialPurposeID) || d.Version < 1 || d.CredentialPurposeVersion < 1 || !validText(d.Name, 120) || d.MaxVerifyChargeMicroUSD < 0 || d.MaxVerifyChargeMicroUSD > domain.MaxMoney {
		return c, "", configurationProfileInvalid()
	}
	endpoint := ""
	switch d.Kind {
	case "model":
		if c.Model == nil || c.MCP != nil || c.Model.ID != d.TargetID || c.Model.OrganizationID != d.OrganizationID || c.Model.Version < 1 || d.Model != c.Model.Model || d.DiscoveryUnmeteredApproved || c.Model.CredentialEnvironment != "" || c.Model.MaxChargeMicroUSD > d.MaxVerifyChargeMicroUSD || c.Model.MaxOutputBytes > 262144 || c.Model.TimeoutMilliseconds < 1 || c.Model.TimeoutMilliseconds > 600000 {
			return c, "", configurationProfileInvalid()
		}
		model := *c.Model
		model.Active, model.CreatedAt = false, time.Time{}
		c.Model = &model
		validation := model
		validation.CredentialEnvironment = "PLATFORM_MODEL_KEY_CONFIGURATION_VALIDATION"
		if _, err := modelruntime.New(&Store{}, RuntimeModelConfig(validation, "configuration-validation-only")); err != nil {
			return c, "", configurationProfileInvalid()
		}
		endpoint = model.Endpoint
	case "mcp":
		if c.Model != nil || c.MCP == nil || d.Model != "" || !d.DiscoveryUnmeteredApproved || d.MaxVerifyChargeMicroUSD != 0 || validateMCPConnectionProfile(*c.MCP) != nil {
			return c, "", configurationProfileInvalid()
		}
		mcp := *c.MCP
		c.MCP = &mcp
		endpoint = mcp.Endpoint
	default:
		return c, "", configurationProfileInvalid()
	}
	d.Active, d.CreatedAt = false, time.Time{}
	return c, endpoint, nil
}

func scanConfigurationDestination(row scanner) (c domain.ConfigurationDestinationProfile, err error) {
	var raw []byte
	var active bool
	var created time.Time
	if err = row.Scan(&raw, &active, &created); err != nil {
		return c, rowExists(err)
	}
	if len(raw) > 131072 || json.Unmarshal(raw, &c) != nil {
		return c, configurationProfileConflict()
	}
	c, _, err = normalizeConfigurationDestination(c)
	if err != nil {
		return c, configurationProfileConflict()
	}
	c.Destination.Active, c.Destination.CreatedAt = active, created
	return c, nil
}

func loadConfigurationDestinationTx(ctx context.Context, tx pgx.Tx, org, id string, version int, lock bool) (domain.ConfigurationDestinationProfile, error) {
	query := "SELECT profile,active,created_at FROM configuration_destinations WHERE organization_id=$1 AND id=$2 AND version=$3"
	if lock {
		query += " FOR SHARE"
	}
	c, err := scanConfigurationDestination(tx.QueryRow(ctx, query, org, id, version))
	if err == nil && (c.Destination.OrganizationID != org || c.Destination.ID != id || c.Destination.Version != version) {
		err = configurationProfileConflict()
	}
	return c, err
}

// InstallConfigurationDestination persists a complete host-approved template.
// It performs no DNS, credential resolution or network call. Only its redacted
// Destination descriptor is intended for tenant-facing configuration APIs.
func (s *Store) InstallConfigurationDestination(ctx context.Context, in domain.ConfigurationDestinationProfile) (domain.ConfigurationDestinationProfile, error) {
	var empty domain.ConfigurationDestinationProfile
	c, endpoint, err := normalizeConfigurationDestination(in)
	if err != nil {
		return empty, err
	}
	d := c.Destination
	raw, err := json.Marshal(c)
	if err != nil {
		return empty, err
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = configurationGateTx(ctx, tx, d.OrganizationID); err != nil {
		return empty, err
	}
	if err = lockConfigurationOrganizationTx(ctx, tx, d.OrganizationID); err != nil {
		return empty, err
	}
	existing, err := loadConfigurationDestinationTx(ctx, tx, d.OrganizationID, d.ID, d.Version, true)
	if err == nil {
		old, _, _ := normalizeConfigurationDestination(existing)
		if digestValue(old) != Hash(raw) {
			return empty, configurationProfileConflict()
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, domain.NotFound) {
		return empty, err
	}
	purpose, err := loadCredentialPurposeProfileTx(ctx, tx, d.OrganizationID, d.CredentialPurposeID, d.CredentialPurposeVersion, true)
	if err != nil {
		return empty, err
	}
	if !purpose.Active || purpose.Kind != d.Kind || purpose.EndpointDigest != Hash([]byte(endpoint)) {
		return empty, configurationProfileConflict()
	}
	var latest int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM configuration_destinations WHERE organization_id=$1 AND id=$2", d.OrganizationID, d.ID).Scan(&latest); err != nil {
		return empty, err
	}
	if d.Version != latest+1 {
		return empty, configurationProfileConflict()
	}
	if latest > 0 {
		previous, e := loadConfigurationDestinationTx(ctx, tx, d.OrganizationID, d.ID, latest, true)
		if e != nil {
			return empty, e
		}
		if previous.Destination.Kind != d.Kind || previous.Destination.TargetID != d.TargetID || previous.Destination.CredentialPurposeID != d.CredentialPurposeID {
			return empty, configurationProfileConflict()
		}
	}
	if c.Model != nil {
		var collision bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM model_connections WHERE organization_id=$1 AND model=$2 AND id<>$3 AND active)", d.OrganizationID, c.Model.Model, d.TargetID).Scan(&collision); err != nil {
			return empty, err
		}
		if collision {
			return empty, configurationProfileConflict()
		}
	}
	if in.Destination.Active {
		if _, err = tx.Exec(ctx, "UPDATE configuration_destinations SET active=false WHERE organization_id=$1 AND id=$2 AND active", d.OrganizationID, d.ID); err != nil {
			return empty, err
		}
	}
	created, err := scanConfigurationDestination(tx.QueryRow(ctx, `INSERT INTO configuration_destinations(organization_id,id,version,kind,target_id,credential_purpose_id,credential_purpose_version,profile,active)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING profile,active,created_at`, d.OrganizationID, d.ID, d.Version, d.Kind, d.TargetID, d.CredentialPurposeID, d.CredentialPurposeVersion, raw, in.Destination.Active))
	if err != nil {
		return empty, err
	}
	if err = appendConfigurationAuditTx(ctx, tx, d.OrganizationID, "trusted_host", "", "destination", d.ID, "destination.installed", Hash(raw), "Trusted host configuration destination installation"); err != nil {
		return empty, err
	}
	return created, tx.Commit(ctx)
}
