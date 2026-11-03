// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Only the explicitly identified GitHub service container can be restarted.
// Its image and actual PostgreSQL peer address are checked first. No saved
// development configuration or arbitrary Docker endpoint is consulted.
func recoveryUsageRestartOwnedPostgres(t *testing.T, s *Store) *Store {
	t.Helper()
	id := os.Getenv("RUNVERGE_RECOVERY_OWNED_PG_CONTAINER")
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_OWNED_USAGE_HTTPS") != "1" || len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatal("restart requires exact disposable CI service identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var beforeStart time.Time
	var peer, systemID string
	if err := s.Pool.QueryRow(ctx, "SELECT inet_server_addr()::text,pg_postmaster_start_time(),system_identifier::text FROM pg_control_system()").Scan(&peer, &beforeStart, &systemID); err != nil || net.ParseIP(peer) == nil || systemID == "" {
		t.Fatal("owned database identity unavailable")
	}
	type containerInfo struct {
		ID     string `json:"Id"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	call := func(method, suffix string, status int) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.45/containers/"+id+suffix, nil)
		if err != nil {
			t.Fatal("owned Docker request")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("owned Docker request failed")
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil || len(raw) > 1<<20 || response.StatusCode != status {
			t.Fatal("owned Docker response refused")
		}
		return raw
	}
	var info containerInfo
	if json.Unmarshal(call(http.MethodGet, "/json", http.StatusOK), &info) != nil || info.ID != id || !info.State.Running || info.Config.Image != "postgres:18.6-bookworm@sha256:5c4783a8cc3f0074912164f94dfa1be4e043438f0f707cf968d7af9a0d31a2c8" {
		t.Fatal("restart target is not pinned owned PostgreSQL service")
	}
	matched := false
	for _, network := range info.NetworkSettings.Networks {
		if net.ParseIP(peer).Equal(net.ParseIP(network.IPAddress)) {
			matched = true
		}
	}
	if !matched {
		t.Fatal("restart target differs from actual PostgreSQL peer")
	}
	dsn, schema := s.Pool.Config().ConnString(), s.runtimeSchema
	s.Close()
	call(http.MethodPost, "/restart?t=10", http.StatusNoContent)
	var fresh *Store
	var err error
	for ctx.Err() == nil {
		fresh, err = Open(ctx, dsn)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err != nil || fresh == nil {
		t.Fatal("owned PostgreSQL did not return after restart")
	}
	t.Cleanup(fresh.Close)
	var afterStart time.Time
	var afterSystem string
	if err = fresh.Pool.QueryRow(ctx, "SELECT pg_postmaster_start_time(),system_identifier::text FROM pg_control_system()").Scan(&afterStart, &afterSystem); err != nil || !afterStart.After(beforeStart) || afterSystem != systemID || fresh.runtimeSchema != schema {
		t.Fatal("restart did not preserve cluster and schema identity")
	}
	if !errors.Is(fresh.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("restart cleared execution hold")
	}
	return fresh
}
