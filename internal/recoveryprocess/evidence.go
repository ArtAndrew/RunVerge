// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"context"
	"encoding/json"
	"net/netip"
	"net/url"
	"strings"
)

// EvidenceDatabaseURL is private operator data and must never enter a plan,
// receipt, output or error. This profile requires a native Linux Docker host
// which can reach its own private bridge. No container port is published.
func (c *Client) EvidenceDatabaseURL(ctx context.Context, p Intent, b SourceBinding) (string, error) {
	return c.evidenceDatabaseURL(ctx, p, b, true)
}

func (c *Client) PreflightEvidenceDatabaseURL(ctx context.Context, p Intent, b SourceBinding) (string, error) {
	if _, _, err := c.sourceProfile(ctx, p, b.RuntimeImage); err != nil {
		return "", err
	}
	return c.evidenceDatabaseURL(ctx, p, b, false)
}

func (c *Client) evidenceDatabaseURL(ctx context.Context, p Intent, b SourceBinding, stopped bool) (string, error) {
	o, err := c.InspectBoundSource(ctx, p, b)
	if err != nil {
		return "", err
	}
	for _, item := range o.Containers {
		if stopped && ((item.Service != "db" && !item.Stopped) || item.RestartPolicy != "no") {
			return "", sourceError("evidence_executors")
		}
	}
	r, err := c.container(ctx, b.DatabaseContainer, p.Project)
	if err != nil || !r.State.Running || r.Config.Image != sourceDatabaseImage || len(r.NetworkSettings.Networks) != 1 {
		return "", sourceError("evidence_database")
	}
	ep, ok := r.NetworkSettings.Networks[p.Project+"_default"]
	ip, parseErr := netip.ParseAddr(ep.IPAddress)
	if !ok || parseErr != nil || !ip.Is4() || ip.IsLoopback() || ip.IsUnspecified() || !fullID.MatchString(ep.NetworkID) || !fullID.MatchString(ep.EndpointID) {
		return "", sourceError("evidence_endpoint")
	}
	var n struct {
		ID, Name, Driver, Scope string
		Containers              map[string]struct{ EndpointID, IPv4Address string }
		Labels                  map[string]string
	}
	if c.call(ctx, "GET", "/networks/"+ep.NetworkID, nil, &n, 200) != nil || n.ID != ep.NetworkID || n.Name != p.Project+"_default" || n.Driver != "bridge" || n.Scope != "local" || n.Labels["com.docker.compose.project"] != p.Project {
		return "", sourceError("evidence_network")
	}
	allowed := map[string]bool{}
	for _, item := range p.Containers {
		allowed[item.ID] = true
	}
	for id := range n.Containers {
		if !allowed[id] {
			return "", sourceError("evidence_peer")
		}
	}
	peer, ok := n.Containers[b.DatabaseContainer]
	if !ok || peer.EndpointID != ep.EndpointID || strings.Split(peer.IPv4Address, "/")[0] != ip.String() {
		return "", sourceError("evidence_endpoint")
	}
	env := map[string]string{}
	for _, entry := range r.Config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return "", sourceError("evidence_environment")
		}
		if _, exists := env[key]; exists {
			return "", sourceError("evidence_environment")
		}
		env[key] = value
	}
	if env["POSTGRES_USER"] != "runverge_admin" || env["POSTGRES_DB"] != "runverge" || env["POSTGRES_PASSWORD"] == "" {
		return "", sourceError("evidence_credentials")
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword("runverge_admin", env["POSTGRES_PASSWORD"]), Host: ip.String() + ":5432", Path: "/runverge", RawQuery: "sslmode=disable&search_path=public"}
	return u.String(), nil
}

// ObserveEvidenceBackend proves a fresh unpredictable nonce inside the exact
// source container. Copyable database identifiers alone are insufficient.
func (c *Client) ObserveEvidenceBackend(ctx context.Context, p Intent, b SourceBinding, nonce string) error {
	if !operationID.MatchString(nonce) {
		return ErrProfile
	}
	if _, err := c.EvidenceDatabaseURL(ctx, p, b); err != nil {
		return err
	}
	query := `SELECT pg_catalog.json_build_object('source_id',r.installation_id,'system_id',s.system_identifier::text,'database_oid',d.oid::bigint,'matches',(SELECT count(*) FROM pg_catalog.pg_stat_activity a WHERE a.datid=d.oid AND a.usename='runverge_admin' AND a.application_name='rvg_evidence_` + nonce + `' AND a.state='idle in transaction')) FROM public.runtime_recovery_control r,pg_catalog.pg_control_system() s,pg_catalog.pg_database d WHERE r.singleton AND r.state='normal' AND d.datname=current_database() AND NOT pg_catalog.pg_is_in_recovery()`
	command := []string{"/bin/sh", "-ec", `exec env -i PATH=/usr/local/bin:/usr/bin:/bin PGPASSWORD="$POSTGRES_PASSWORD" PGCONNECT_TIMEOUT=2 PGOPTIONS="-c statement_timeout=2000 -c default_transaction_read_only=on" /usr/lib/postgresql/18/bin/psql -X -w -h 127.0.0.1 -p 5432 -U runverge_admin -d runverge -At -v ON_ERROR_STOP=1 -c "$1"`, "evidence-observer", query}
	raw, err := c.execRead(ctx, b.DatabaseContainer, "postgres", command)
	if err != nil {
		return err
	}
	defer clear(raw)
	var reply struct {
		SourceID    string `json:"source_id"`
		SystemID    string `json:"system_id"`
		DatabaseOID uint32 `json:"database_oid"`
		Matches     int    `json:"matches"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.SourceID != b.SourceID || reply.SystemID != b.SystemID || reply.DatabaseOID != b.DatabaseOID || reply.Matches != 1 {
		return sourceError("evidence_witness")
	}
	return nil
}
