// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateConfigurationGrant(ctx context.Context, p domain.Principal, key string, in domain.CreateConfigurationGrant) (json.RawMessage, error) {
	if err := domain.ValidateCreateConfigurationGrant(in); err != nil {
		return nil, err
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if !configurationMember(p) {
			return domain.Forbidden
		}
		if in.ParentGrantID == "" {
			if !configurationOwner(p) {
				return domain.Forbidden
			}
			return nil
		}
		_, err := configurationAuthorityTx(ctx, tx, p, in.ParentGrantID, "delegate", nil)
		return err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /configuration/grants", key, in, configurationPreAuthorization, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if _, _, _, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, in.SubjectID); err != nil {
			if configurationAuthorityDenied(err) {
				return nil, domain.NotFound
			}
			return nil, err
		}
		if err := lockAuthorityPrincipalsTx(ctx, tx, p.OrganizationID, []string{p.ID, in.SubjectID}); err != nil {
			return nil, err
		}
		if err := lockConfigurationOrganizationTx(ctx, tx, p.OrganizationID); err != nil {
			return nil, err
		}
		_, ie, ip, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, p.ID)
		if err != nil {
			return nil, err
		}
		subject, se, sp, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, in.SubjectID)
		if err != nil {
			return nil, domain.NotFound
		}
		if !configurationMember(subject) {
			return nil, domain.Forbidden
		}
		var now time.Time
		if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return nil, err
		}
		g := domain.CapabilityEnvelope{ID: domain.NewID("cfg_grant"), OrganizationID: p.OrganizationID, Audience: domain.ConfigurationAudience, IssuerID: p.ID, SubjectID: in.SubjectID, ParentGrantID: in.ParentGrantID, Version: 1, Actions: append([]string{}, in.Actions...), Scopes: []domain.ConfigurationScope{}, MaxPlans: in.MaxPlans, MaxVerifications: in.MaxVerifications, MaxAllocatedChargeMicroUSD: in.MaxAllocatedChargeMicroUSD, Status: "active", CreatedAt: now, ExpiresAt: now.Add(time.Duration(in.TTLSeconds) * time.Second), AvailableCommands: []string{}}
		for _, ref := range in.Destinations {
			d, e := loadConfigurationDestinationTx(ctx, tx, p.OrganizationID, ref.ID, ref.Version, true)
			if e != nil {
				return nil, e
			}
			purpose, e := loadCredentialPurposeProfileTx(ctx, tx, p.OrganizationID, d.Destination.CredentialPurposeID, d.Destination.CredentialPurposeVersion, true)
			if e != nil {
				return nil, e
			}
			if !d.Destination.Active || !purpose.Active {
				return nil, domain.Forbidden
			}
			g.Scopes = append(g.Scopes, configurationScope(d.Destination))
		}
		if in.ParentGrantID != "" {
			chain, e := configurationAuthorityTx(ctx, tx, p, in.ParentGrantID, "delegate", g.Scopes)
			if e != nil {
				return nil, e
			}
			parent := chain[len(chain)-1]
			if parent.Version != in.ExpectedParentVersion {
				return nil, configurationProfileConflict()
			}
			if g.ExpiresAt.After(parent.ExpiresAt) {
				g.ExpiresAt = parent.ExpiresAt
			}
			if parent.Depth >= domain.MaxConfigurationGrantDepth || g.MaxPlans > parent.MaxPlans || g.MaxVerifications > parent.MaxVerifications || g.MaxAllocatedChargeMicroUSD > parent.MaxAllocatedChargeMicroUSD {
				return nil, domain.Forbidden
			}
			g.Depth = parent.Depth + 1
			for _, action := range g.Actions {
				if !configurationHasAction(parent.CapabilityEnvelope, action) {
					return nil, domain.Forbidden
				}
			}
		}
		body, err := json.Marshal(g)
		if err != nil {
			return nil, err
		}
		created, err := scanConfigurationGrant(tx.QueryRow(ctx, `INSERT INTO configuration_grants(organization_id,id,issuer_id,subject_id,parent_grant_id,depth,issuer_authority_epoch,subject_authority_epoch,issuer_policy_epoch,subject_policy_epoch,body,max_plans,max_verifications,max_allocated_charge_microusd,created_at,expires_at)
 VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING `+configurationGrantColumns, g.OrganizationID, g.ID, g.IssuerID, g.SubjectID, g.ParentGrantID, g.Depth, ie, se, ip, sp, body, g.MaxPlans, g.MaxVerifications, g.MaxAllocatedChargeMicroUSD, g.CreatedAt, g.ExpiresAt))
		if err != nil {
			return nil, err
		}
		if err = guard(ctx, tx, p); err != nil {
			return nil, err
		}
		if err = appendConfigurationAuditTx(ctx, tx, g.OrganizationID, "principal", p.ID, "grant", g.ID, "grant.created", Hash(body), in.Reason); err != nil {
			return nil, err
		}
		return configurationGrantViewTx(ctx, tx, p, created)
	})
}

func configurationGrantReadable(p domain.Principal, g configurationGrantRecord) bool {
	return configurationHumanManager(p) || p.ID == g.SubjectID || p.ID == g.IssuerID
}
func configurationGrantViewTx(ctx context.Context, tx pgx.Tx, p domain.Principal, g configurationGrantRecord) (domain.CapabilityEnvelope, error) {
	if !configurationGrantReadable(p, g) {
		return domain.CapabilityEnvelope{}, domain.NotFound
	}
	var timely bool
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()<$1", g.ExpiresAt).Scan(&timely); err != nil {
		return domain.CapabilityEnvelope{}, err
	}
	if !g.Revoked && !timely {
		g.Status = "expired"
	}
	if g.Status == "active" {
		chain, err := configurationGrantChainTx(ctx, tx, g.OrganizationID, g.ID, false)
		if err != nil {
			return domain.CapabilityEnvelope{}, err
		}
		if err = checkConfigurationGrantChainTx(ctx, tx, chain); err != nil {
			if _, ok := err.(*domain.Error); !ok {
				return domain.CapabilityEnvelope{}, err
			}
			g.Status = "invalidated"
		}
	}
	g.AvailableCommands = []string{}
	if !g.Revoked && configurationMember(p) {
		g.AvailableCommands = append(g.AvailableCommands, "revoke")
	}
	if g.Status == "active" && p.ID == g.SubjectID && configurationMember(p) && configurationHasAction(g.CapabilityEnvelope, "delegate") && g.Depth < domain.MaxConfigurationGrantDepth {
		g.AvailableCommands = append(g.AvailableCommands, "delegate")
	}

	return g.CapabilityEnvelope, nil
}

func (s *Store) RevokeConfigurationGrant(ctx context.Context, p domain.Principal, id, key string, in domain.RevokeConfigurationGrant) (json.RawMessage, error) {
	if !configurationIdentifier.MatchString(id) || in.ExpectedVersion < 1 {
		return nil, configurationProfileInvalid()
	}
	if err := domain.ValidateConfigurationReason(in.Reason); err != nil {
		return nil, err
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		g, err := loadConfigurationGrantTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return err
		}
		if !configurationGrantReadable(p, g) {
			return domain.NotFound
		}
		if !configurationMember(p) {
			return domain.Forbidden
		}
		return nil
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /configuration/grants/"+id+"/revoke", key, in, configurationPreAuthorization, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockConfigurationOrganizationTx(ctx, tx, p.OrganizationID); err != nil {
			return nil, err
		}
		g, err := loadConfigurationGrantTx(ctx, tx, p.OrganizationID, id, true)
		if err != nil {
			return nil, err
		}
		if g.Version != in.ExpectedVersion || g.Revoked {
			return nil, configurationProfileConflict()
		}
		g, err = scanConfigurationGrant(tx.QueryRow(ctx, "UPDATE configuration_grants SET revoked=true WHERE organization_id=$1 AND id=$2 RETURNING "+configurationGrantColumns, p.OrganizationID, id))
		if err != nil {
			return nil, err
		}
		if err = appendConfigurationAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "grant", id, "grant.revoked", digestValue(in), in.Reason); err != nil {
			return nil, err
		}
		return configurationGrantViewTx(ctx, tx, p, g)
	})
}
