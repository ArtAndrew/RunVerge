// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoverycontrol"
)

const sourceDatabaseImage = "postgres:18.6-bookworm@sha256:5c4783a8cc3f0074912164f94dfa1be4e043438f0f707cf968d7af9a0d31a2c8"

var sourceID = regexp.MustCompile(`^inst_[a-f0-9]{32}$`)

type SourceBinding struct {
	Version           int             `json:"version"`
	PlanSHA256        string          `json:"plan_sha256"`
	SourceID          string          `json:"source_id"`
	SnapshotSHA256    string          `json:"snapshot_sha256"`
	RuntimeImage      string          `json:"runtime_image"`
	DatabaseContainer string          `json:"database_container"`
	DatabaseOID       uint32          `json:"database_oid"`
	SystemID          string          `json:"system_id"`
	ObservedAt        time.Time       `json:"observed_at"`
	Processes         []SourceProcess `json:"processes"`
}
type SourceProcess struct {
	Container string `json:"container"`
	BootID    string `json:"boot_id"`
}

func planDigest(p Intent) string {
	body, _ := json.Marshal(p)
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

// Stage names are fixed classifications: never include Docker payloads, command
// output, connection strings or archive data in an operator-facing error.
func sourceError(stage string) error { return fmt.Errorf("%w (source:%s)", ErrProfile, stage) }

func (c *Client) sourceProfile(ctx context.Context, p Intent, image string) (string, []Container, error) {
	if !strings.HasPrefix(image, "sha256:") || !fullID.MatchString(strings.TrimPrefix(image, "sha256:")) {
		return "", nil, sourceError("image_id")
	}
	if _, err := c.Inspect(ctx, p); err != nil {
		return "", nil, err
	}
	var db, network string
	var apps []Container
	members := map[string]bool{}
	active := map[string]string{}
	addresses := map[string]string{}
	for _, item := range p.Containers {
		members[item.ID] = true
		row, err := c.container(ctx, item.ID, p.Project)
		if err != nil {
			return "", nil, err
		}
		h := row.HostConfig
		if len(h.Dns)+len(h.DnsOptions)+len(h.DnsSearch)+len(h.ExtraHosts)+len(h.Links) != 0 {
			return "", nil, sourceError("dns")
		}
		if len(row.ExecIDs) != 0 {
			return "", nil, sourceError("active_exec")
		}
		if item.Service == "admin" || item.Service == "migrate" {
			if !state(row).Stopped || row.Image != image || !slices.Equal(row.Config.Entrypoint, []string{"/app/bin/platform"}) {
				return "", nil, sourceError("auxiliary_state")
			}
			continue
		}
		started, err := time.Parse(time.RFC3339Nano, row.State.StartedAt)
		if err != nil || started.IsZero() || !row.State.Running || len(row.NetworkSettings.Networks) != 1 {
			return "", nil, sourceError("runtime_state")
		}
		endpoint, ok := row.NetworkSettings.Networks[p.Project+"_default"]
		if !ok || !fullID.MatchString(endpoint.NetworkID) || !fullID.MatchString(endpoint.EndpointID) {
			return "", nil, sourceError("network_endpoint")
		}
		ip, e := netip.ParseAddr(endpoint.IPAddress)
		if e != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() {
			return "", nil, sourceError("network_address")
		}
		active[item.ID] = endpoint.EndpointID
		addresses[item.ID] = ip.String()
		if network == "" {
			network = endpoint.NetworkID
		} else if network != endpoint.NetworkID {
			return "", nil, sourceError("network_identity")
		}
		if item.Service == "db" {
			if row.Config.Image != sourceDatabaseImage || !slices.Contains(endpoint.Aliases, "db") {
				return "", nil, sourceError("database_image")
			}
			db = item.ID
			continue
		}
		if slices.Contains(endpoint.Aliases, "db") || row.Image != image || !slices.Equal(row.Config.Entrypoint, []string{"/app/bin/platform"}) {
			return "", nil, sourceError("runtime_image")
		}
		if item.Service == "api" && !slices.Equal(row.Config.Cmd, []string{"serve", "--container-preview", "--listen", "0.0.0.0:8080"}) {
			return "", nil, sourceError("api_command")
		}
		if item.Service == "worker" {
			if len(row.Config.Cmd) != 3 || row.Config.Cmd[0] != "worker" || row.Config.Cmd[1] != "--concurrency" {
				return "", nil, sourceError("worker_command")
			}
			n, err := strconv.Atoi(row.Config.Cmd[2])
			if err != nil || n < 1 || n > 64 || strconv.Itoa(n) != row.Config.Cmd[2] {
				return "", nil, sourceError("worker_concurrency")
			}
		}
		env := map[string]string{}
		for _, entry := range row.Config.Env {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				return "", nil, sourceError("environment_shape")
			}
			if _, exists := env[key]; exists {
				return "", nil, sourceError("environment_duplicate")
			}
			env[key] = value
			if strings.HasPrefix(key, "PG") && value != "" {
				return "", nil, sourceError("pg_environment")
			}
			if key == "POSTGRES_PASSWORD" || key == "RUNVERGE_APP_VERIFIER" {
				return "", nil, sourceError("admin_environment")
			}
		}
		u, err := url.Parse(env["DATABASE_URL"])
		if err != nil || u.Scheme != "postgres" || u.Host != "db:5432" || u.Path != "/runverge" || u.RawQuery != "sslmode=disable" || u.Fragment != "" || u.User == nil || u.User.Username() != "runverge" || env["RUNVERGE_RECOVERY_CONTROL"] != "container_v1" {
			return "", nil, sourceError("database_url")
		}
		password, present := u.User.Password()
		if !present || password == "" {
			return "", nil, sourceError("database_password")
		}
		apps = append(apps, item)
	}
	if db == "" || len(apps) < 2 || len(apps) > 8 {
		return "", nil, sourceError("service_count")
	}
	var n struct {
		ID, Name, Driver, Scope string
		Containers              map[string]struct{ EndpointID, IPv4Address string }
		Labels                  map[string]string
	}
	if c.call(ctx, "GET", "/networks/"+network, nil, &n, 200) != nil || n.ID != network || n.Name != p.Project+"_default" || n.Driver != "bridge" || n.Scope != "local" || n.Labels["com.docker.compose.project"] != p.Project {
		return "", nil, sourceError("network_inspection")
	}
	for id := range n.Containers {
		if !members[id] || active[id] == "" {
			return "", nil, sourceError("network_members")
		}
	}
	for id, endpoint := range active {
		peer, exists := n.Containers[id]
		if !exists || peer.EndpointID != endpoint || strings.Split(peer.IPv4Address, "/")[0] != addresses[id] {
			return "", nil, sourceError("network_endpoint_match")
		}
	}
	return db, apps, nil
}

// execRead runs only the fixed witness/observer commands constructed below. All
// raw output is private, bounded, cleared, and never attached to an error.
func (c *Client) execRead(ctx context.Context, container, user string, command []string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"AttachStdout": true, "AttachStderr": true, "Tty": false, "User": user, "Cmd": command})
	var created struct {
		ID string `json:"Id"`
	}
	if c.call(ctx, "POST", "/containers/"+container+"/exec", body, &created, 201) != nil || !fullID.MatchString(created.ID) {
		return nil, ErrProfile
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://docker/v1.45/exec/"+created.ID+"/start", strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return nil, ErrProfile
	}
	request.Header.Set("Content-Type", "application/json")
	client := *c.http
	client.Timeout = 28 * time.Second
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrProfile
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 32769))
	defer clear(raw)
	if err != nil || len(raw) > 32768 || response.StatusCode != 200 {
		return nil, ErrProfile
	}
	output, err := witnessOutput(raw)
	if err != nil {
		return nil, err
	}
	var final struct {
		ID, ContainerID string
		Running         bool
		ExitCode        int
	}
	settled, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		if c.call(settled, "GET", "/exec/"+created.ID+"/json", nil, &final, 200) != nil || final.ID != created.ID || final.ContainerID != container {
			clear(output)
			return nil, ErrProfile
		}
		if !final.Running {
			if final.ExitCode != 0 {
				clear(output)
				return nil, ErrProfile
			}
			break
		}
		select {
		case <-settled.Done():
			clear(output)
			return nil, ErrProfile
		case <-time.After(25 * time.Millisecond):
		}
	}
	return output, nil
}
func witnessOutput(raw []byte) ([]byte, error) {
	var output []byte
	for len(raw) > 0 {
		if len(raw) < 8 || raw[0] != 1 || raw[1] != 0 || raw[2] != 0 || raw[3] != 0 {
			return nil, ErrProfile
		}
		size := int(binary.BigEndian.Uint32(raw[4:8]))
		raw = raw[8:]
		if size > len(raw) || size > 8192 || len(output)+size > 8192 {
			return nil, ErrProfile
		}
		output = append(output, raw[:size]...)
		raw = raw[size:]
	}
	return output, nil
}

type sourceDatabaseObservation struct {
	SourceID    string            `json:"source_id"`
	SystemID    string            `json:"system_id"`
	DatabaseOID uint32            `json:"database_oid"`
	Nonces      []string          `json:"nonces"`
	Peers       map[string]string `json:"peers"`
}

func (c *Client) observeSource(ctx context.Context, db string, nonces []string) (sourceDatabaseObservation, error) {
	var zero sourceDatabaseObservation
	values := make([]string, len(nonces))
	for i, n := range nonces {
		if !operationID.MatchString(n) {
			return zero, ErrProfile
		}
		values[i] = "'rvg_witness_" + n + "'"
	}
	query := `SELECT pg_catalog.json_build_object('source_id',r.installation_id,'system_id',s.system_identifier::text,'database_oid',d.oid,'nonces',COALESCE((SELECT pg_catalog.json_agg(a.application_name ORDER BY a.application_name) FROM pg_catalog.pg_stat_activity a WHERE a.datid=d.oid AND a.usesysid=u.oid AND a.state='active' AND a.application_name IN (` + strings.Join(values, ",") + `)), '[]'::json), 'peers', COALESCE((SELECT pg_catalog.json_object_agg(a.application_name,pg_catalog.host(a.client_addr)) FROM pg_catalog.pg_stat_activity a WHERE a.datid=d.oid AND a.usesysid=u.oid AND a.state='active' AND a.application_name IN (` + strings.Join(values, ",") + `)), '{}'::json)) FROM public.runtime_recovery_control r,pg_catalog.pg_control_system() s,pg_catalog.pg_database d,pg_catalog.pg_roles u WHERE r.singleton AND r.state='normal' AND d.datname=current_database() AND d.datname='runverge' AND u.rolname='runverge' AND NOT u.rolsuper AND NOT u.rolcreatedb AND NOT u.rolcreaterole AND NOT u.rolreplication AND NOT u.rolbypassrls AND NOT pg_catalog.pg_is_in_recovery()`
	command := []string{"/bin/sh", "-ec", `exec env -i PATH=/usr/local/bin:/usr/bin:/bin PGPASSWORD="$POSTGRES_PASSWORD" PGCONNECT_TIMEOUT=2 PGOPTIONS="-c statement_timeout=2000 -c default_transaction_read_only=on" psql -X -w -h 127.0.0.1 -p 5432 -U runverge_admin -d runverge -At -v ON_ERROR_STOP=1 -c "$1"`, "source-observer", query}
	raw, err := c.execRead(ctx, db, "postgres", command)
	if err != nil {
		return zero, err
	}
	defer clear(raw)
	var observed sourceDatabaseObservation
	if json.Unmarshal(bytes.TrimSpace(raw), &observed) != nil || !sourceID.MatchString(observed.SourceID) || observed.SystemID == "" || observed.DatabaseOID == 0 {
		return zero, ErrProfile
	}
	return observed, nil
}

func (c *Client) BindSource(ctx context.Context, p Intent, source, snapshot, image string) (SourceBinding, error) {
	var zero SourceBinding
	if !sourceID.MatchString(source) || !fullID.MatchString(snapshot) {
		return zero, ErrProfile
	}
	bounded, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	db, apps, err := c.sourceProfile(bounded, p, image)
	if err != nil {
		return zero, err
	}
	nonces := make([]string, len(apps))
	type outcome struct {
		index int
		reply recoverycontrol.Reply
		err   error
	}
	replies := make(chan outcome, len(apps))
	for i, app := range apps {
		nonce := make([]byte, 16)
		if _, err = rand.Read(nonce); err != nil {
			return zero, ErrProfile
		}
		nonces[i] = hex.EncodeToString(nonce)
		go func(i int, app Container) {
			raw, e := c.execRead(bounded, app.ID, "10001:10001", []string{"/app/bin/platform", "recovery", "source-witness", "--expected-installation", source, "--nonce", nonces[i], "--role", app.Service})
			defer clear(raw)
			var reply recoverycontrol.Reply
			if e == nil && (json.Unmarshal(bytes.TrimSpace(raw), &reply) != nil || reply.SourceID != source || reply.Nonce != nonces[i] || reply.Role != app.Service || reply.PID != 1 || !operationID.MatchString(reply.BootID)) {
				e = ErrProfile
			}
			replies <- outcome{i, reply, e}
		}(i, app)
	}
	expected := make([]string, len(nonces))
	for i, n := range nonces {
		expected[i] = "rvg_witness_" + n
	}
	sort.Strings(expected)
	var observed sourceDatabaseObservation
	proved := false
	deadline := time.NewTimer(14 * time.Second)
	defer deadline.Stop()
	for !proved {
		observed, err = c.observeSource(bounded, db, nonces)
		if err != nil {
			err = sourceError("database_observation")
			break
		}
		if observed.SourceID != source {
			err = sourceError("installation_identity")
			break
		}
		proved = slices.Equal(observed.Nonces, expected)
		if proved {
			for i, app := range apps {
				row, e := c.container(bounded, app.ID, p.Project)
				if e != nil || observed.Peers["rvg_witness_"+nonces[i]] != row.NetworkSettings.Networks[p.Project+"_default"].IPAddress {
					proved = false
					err = sourceError("database_peer")
					break
				}
			}
			if err != nil {
				break
			}
		}
		if !proved {
			select {
			case <-bounded.Done():
				err = ErrProfile
			case <-deadline.C:
				err = sourceError("challenge_not_observed")
			case <-time.After(150 * time.Millisecond):
			}
			if err != nil {
				break
			}
		}
	}
	binding := SourceBinding{Version: 1, PlanSHA256: planDigest(p), SourceID: source, SnapshotSHA256: snapshot, RuntimeImage: image, DatabaseContainer: db, DatabaseOID: observed.DatabaseOID, SystemID: observed.SystemID, ObservedAt: time.Now().UTC(), Processes: make([]SourceProcess, len(apps))}
	// Collect every helper outcome. A failed binding is never permission to stop;
	// helpers have their own bounded timeout even if this observer disappears.
	for range apps {
		select {
		case result := <-replies:
			if result.err != nil && err == nil {
				err = sourceError("runtime_witness_response")
			}
			binding.Processes[result.index] = SourceProcess{apps[result.index].ID, result.reply.BootID}
		case <-bounded.Done():
			return zero, ErrProfile
		}
	}
	if err != nil {
		return zero, err
	}
	if !proved {
		return zero, sourceError("challenge_not_observed")
	}
	if _, _, err = c.sourceProfile(bounded, p, image); err != nil {
		return zero, err
	}
	return binding, nil
}

func (c *Client) InspectBoundSource(ctx context.Context, p Intent, b SourceBinding) (Observation, error) {
	if b.Version != 1 || b.PlanSHA256 != planDigest(p) || !sourceID.MatchString(b.SourceID) || !fullID.MatchString(b.SnapshotSHA256) || b.SystemID == "" || b.DatabaseOID == 0 || b.ObservedAt.IsZero() || !strings.HasPrefix(b.RuntimeImage, "sha256:") || !fullID.MatchString(strings.TrimPrefix(b.RuntimeImage, "sha256:")) {
		return Observation{}, ErrProfile
	}
	var apps []string
	db := ""
	for _, item := range p.Containers {
		if item.Service == "api" || item.Service == "worker" {
			if item.Image != b.RuntimeImage {
				return Observation{}, ErrProfile
			}
			apps = append(apps, item.ID)
		}
		if item.Service == "db" {
			db = item.ID
		}
	}
	if db != b.DatabaseContainer || len(apps) != len(b.Processes) {
		return Observation{}, ErrProfile
	}
	for i, item := range b.Processes {
		if item.Container != apps[i] || !operationID.MatchString(item.BootID) {
			return Observation{}, ErrProfile
		}
	}
	observed, err := c.Inspect(ctx, p)
	if err != nil {
		return Observation{}, err
	}
	observed.SourceInstallationBound = true
	observed.SourceID = b.SourceID
	observed.BoundSourceProcessesStopped = observed.ProjectProcessesStopped
	return observed, nil
}

func (c *Client) RevalidateSource(ctx context.Context, p Intent, b SourceBinding) error {
	if _, err := c.InspectBoundSource(ctx, p, b); err != nil {
		return err
	}
	fresh, err := c.BindSource(ctx, p, b.SourceID, b.SnapshotSHA256, b.RuntimeImage)
	if err != nil || fresh.SystemID != b.SystemID || fresh.DatabaseOID != b.DatabaseOID || fresh.DatabaseContainer != b.DatabaseContainer || !slices.Equal(fresh.Processes, b.Processes) {
		return ErrProfile
	}
	return nil
}
