// SPDX-License-Identifier: Elastic-2.0
// Package recoveryprocess stops an explicitly selected local Compose project.
// It is not evidence of deployment ownership, revoked external credentials or
// settlement of requests already sent. Docker and exclusive host control are trusted.
package recoveryprocess

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

var ErrProfile = errors.New("container shutdown profile or exact inventory not verified")
var ErrUnknown = errors.New("container shutdown outcome unconfirmed; inspect the saved plan before explicit continuation")

var fullID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var operationID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service"`
	Created string `json:"created"`
	Image   string `json:"image"`
}
type Intent struct {
	Version     int         `json:"version"`
	OperationID string      `json:"operation_id"`
	DaemonID    string      `json:"daemon_id"`
	Project     string      `json:"project"`
	CreatedAt   time.Time   `json:"created_at"`
	Containers  []Container `json:"containers"`
}
type Observation struct {
	OperationID             string           `json:"operation_id"`
	Project                 string           `json:"project"`
	ObservedAt              time.Time        `json:"observed_at"`
	ProjectProcessesStopped bool             `json:"project_processes_stopped"`
	SourceInstallationBound bool             `json:"source_installation_bound"`
	ExternalExecutionFenced bool             `json:"external_execution_fenced"`
	ExternalEffectsSettled  bool             `json:"external_effects_settled"`
	Containers              []ContainerState `json:"containers"`
}
type ContainerState struct {
	ID            string `json:"id"`
	Service       string `json:"service"`
	Status        string `json:"status"`
	RestartPolicy string `json:"restart_policy"`
	Stopped       bool   `json:"stopped"`
}
type daemonInfo struct {
	ID                 string
	OSType             string
	LiveRestoreEnabled bool
	Swarm              struct{ LocalNodeState string }
}
type inspect struct {
	ID                   string `json:"Id"`
	Name, Created, Image string
	ExecIDs              []string
	Config               *struct {
		User   string
		Labels map[string]string
	}
	HostConfig *struct {
		Privileged, AutoRemove, ReadonlyRootfs                           bool
		PidMode, IpcMode, UTSMode, UsernsMode, CgroupnsMode, NetworkMode string
		CapAdd, CapDrop, VolumesFrom, DeviceCgroupRules                  []string
		Devices, DeviceRequests                                          []json.RawMessage
		RestartPolicy                                                    struct {
			Name              string
			MaximumRetryCount int
		}
	}
	State *struct {
		Status                            string
		Running, Paused, Restarting, Dead bool
		Pid                               int
	}
	Mounts []struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
}
type Client struct{ http *http.Client }

// New requires the operator to choose a local socket. No Docker context,
// DOCKER_HOST, proxy, credential helper or TCP fallback is consulted.
func New(socket string) (*Client, error) {
	if runtime.GOOS != "linux" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return nil, ErrProfile
	}
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0002 != 0 {
		return nil, ErrProfile
	}
	t := &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
		}}
	return &Client{http: &http.Client{Transport: t, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrProfile }}}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) call(ctx context.Context, method, path string, body []byte, out any, accepted ...int) error {
	request, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.45"+path, bytes.NewReader(body))
	if err != nil {
		return ErrProfile
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return ErrProfile
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	defer clear(raw)
	if err != nil || len(raw) > 2<<20 || !slices.Contains(accepted, response.StatusCode) {
		return ErrProfile
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return ErrProfile
	}
	return nil
}
func (c *Client) daemon(ctx context.Context) (string, error) {
	var d daemonInfo
	if c.call(ctx, "GET", "/info", nil, &d, 200) != nil || len(d.ID) < 1 || len(d.ID) > 128 || d.OSType != "linux" || d.LiveRestoreEnabled || d.Swarm.LocalNodeState != "inactive" {
		return "", ErrProfile
	}
	return d.ID, nil
}
func (c *Client) ids(ctx context.Context, project string) ([]string, error) {
	if !projectName.MatchString(project) {
		return nil, ErrProfile
	}
	filter, _ := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	var rows []struct {
		ID string `json:"Id"`
	}
	if c.call(ctx, "GET", "/containers/json?all=1&filters="+url.QueryEscape(string(filter)), nil, &rows, 200) != nil || len(rows) < 1 || len(rows) > 32 {
		return nil, ErrProfile
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		if !fullID.MatchString(row.ID) {
			return nil, ErrProfile
		}
		ids[i] = row.ID
	}
	sort.Strings(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		return nil, ErrProfile
	}
	return ids, nil
}
func (c *Client) container(ctx context.Context, id, project string) (inspect, error) {
	var row inspect
	if !fullID.MatchString(id) || c.call(ctx, "GET", "/containers/"+id+"/json", nil, &row, 200) != nil || row.ID != id || row.Config == nil || row.HostConfig == nil || row.State == nil {
		return inspect{}, ErrProfile
	}
	if _, err := time.Parse(time.RFC3339Nano, row.Created); err != nil {
		return inspect{}, ErrProfile
	}
	if !strings.HasPrefix(row.Name, "/") || len(row.Name) > 256 || !strings.HasPrefix(row.Image, "sha256:") || !fullID.MatchString(strings.TrimPrefix(row.Image, "sha256:")) {
		return inspect{}, ErrProfile
	}
	l, h, s := row.Config.Labels, row.HostConfig, row.State
	service := l["com.docker.compose.service"]
	if l["com.docker.compose.project"] != project || !slices.Contains([]string{"api", "worker", "db", "migrate", "admin"}, service) || l["com.docker.swarm.service.id"] != "" || h.Privileged || h.AutoRemove || len(h.CapAdd) > 0 || len(h.DeviceCgroupRules) > 0 || len(h.Devices) > 0 || len(h.DeviceRequests) > 0 || len(h.VolumesFrom) > 0 || h.PidMode != "" || h.UTSMode != "" || (h.UsernsMode != "" && h.UsernsMode != "private") || (h.IpcMode != "" && h.IpcMode != "private") || h.CgroupnsMode == "host" || h.NetworkMode == "host" || strings.HasPrefix(h.NetworkMode, "container:") {
		return inspect{}, ErrProfile
	}
	if !slices.Contains([]string{"no", "always", "unless-stopped", "on-failure"}, h.RestartPolicy.Name) || s.Dead || s.Paused || s.Restarting || !slices.Contains([]string{"created", "running", "exited"}, s.Status) {
		return inspect{}, ErrProfile
	}
	if service != "db" && (row.Config.User != "10001:10001" || !h.ReadonlyRootfs || !slices.Contains(h.CapDrop, "ALL")) {
		return inspect{}, ErrProfile
	}
	for _, m := range row.Mounts {
		switch m.Type {
		case "volume":
			if !((service == "db" && m.Destination == "/var/lib/postgresql") || (service == "admin" && m.Destination == "/state")) || m.Name == "" {
				return inspect{}, ErrProfile
			}
			var v struct {
				Name, Driver, Scope string
				Options             map[string]string
				Labels              map[string]string
			}
			if c.call(ctx, "GET", "/volumes/"+url.PathEscape(m.Name), nil, &v, 200) != nil || v.Name != m.Name || v.Driver != "local" || v.Scope != "local" || len(v.Options) != 0 || v.Labels["com.docker.compose.project"] != project {
				return inspect{}, ErrProfile
			}
		case "bind":
			if service != "db" || m.RW || !slices.Contains([]string{"/docker-entrypoint-initdb.d/10-runtime.sql", "/docker-entrypoint-initdb.d/20-profile.sh", "/usr/local/bin/runverge-db-healthcheck"}, m.Destination) {
				return inspect{}, ErrProfile
			}
			file, err := os.Lstat(m.Source)
			if err != nil || !file.Mode().IsRegular() || file.Mode().Perm()&0022 != 0 {
				return inspect{}, ErrProfile
			}
		case "tmpfs":
			if service == "db" || m.Destination != "/tmp" {
				return inspect{}, ErrProfile
			}
		default:
			return inspect{}, ErrProfile
		}
	}
	return row, nil
}
func identity(r inspect) Container {
	return Container{r.ID, r.Name, r.Config.Labels["com.docker.compose.service"], r.Created, r.Image}
}
func state(r inspect) ContainerState {
	s, h := r.State, r.HostConfig
	stopped := (s.Status == "created" || s.Status == "exited") && !s.Running && !s.Paused && !s.Restarting && !s.Dead && s.Pid == 0 && len(r.ExecIDs) == 0 && h.RestartPolicy.Name == "no" && h.RestartPolicy.MaximumRetryCount == 0
	return ContainerState{r.ID, r.Config.Labels["com.docker.compose.service"], s.Status, h.RestartPolicy.Name, stopped}
}
func valid(p Intent) bool {
	if p.Version != 1 || !operationID.MatchString(p.OperationID) || !projectName.MatchString(p.Project) || p.DaemonID == "" || p.CreatedAt.IsZero() || len(p.Containers) < 1 || len(p.Containers) > 32 {
		return false
	}
	last := ""
	counts := map[string]int{}
	for _, r := range p.Containers {
		if !fullID.MatchString(r.ID) || r.ID <= last {
			return false
		}
		last = r.ID
		counts[r.Service]++
	}
	return counts["db"] == 1 && counts["api"] == 1 && counts["worker"] >= 1
}
func (c *Client) Plan(ctx context.Context, project string) (Intent, error) {
	var zero Intent
	d, err := c.daemon(ctx)
	if err != nil {
		return zero, err
	}
	ids, err := c.ids(ctx, project)
	if err != nil {
		return zero, err
	}
	op := make([]byte, 16)
	if _, err = rand.Read(op); err != nil {
		return zero, ErrProfile
	}
	p := Intent{Version: 1, OperationID: hex.EncodeToString(op), DaemonID: d, Project: project, CreatedAt: time.Now().UTC()}
	for _, id := range ids {
		r, e := c.container(ctx, id, project)
		if e != nil {
			return zero, e
		}
		p.Containers = append(p.Containers, identity(r))
	}
	if _, err = c.Inspect(ctx, p); err != nil {
		return zero, err
	}
	return p, nil
}
func (c *Client) Inspect(ctx context.Context, p Intent) (Observation, error) {
	var zero Observation
	if !valid(p) {
		return zero, ErrProfile
	}
	d, err := c.daemon(ctx)
	if err != nil || d != p.DaemonID {
		return zero, ErrProfile
	}
	ids, err := c.ids(ctx, p.Project)
	if err != nil || len(ids) != len(p.Containers) {
		return zero, ErrProfile
	}
	o := Observation{OperationID: p.OperationID, Project: p.Project, ProjectProcessesStopped: true}
	for i, bound := range p.Containers {
		if ids[i] != bound.ID {
			return zero, ErrProfile
		}
		r, e := c.container(ctx, bound.ID, p.Project)
		if e != nil || identity(r) != bound {
			return zero, ErrProfile
		}
		s := state(r)
		o.Containers = append(o.Containers, s)
		o.ProjectProcessesStopped = o.ProjectProcessesStopped && s.Stopped
	}
	// Recheck the full inventory and daemon after observing individual processes.
	again, e := c.ids(ctx, p.Project)
	if e != nil || !slices.Equal(ids, again) {
		return zero, ErrProfile
	}
	last, e := c.daemon(ctx)
	if e != nil || last != d {
		return zero, ErrProfile
	}
	o.ObservedAt = time.Now().UTC()
	return o, nil
}

// Stop never retries an uncertain mutation. The saved plan can be inspected in
// a new process; continuing is a separate explicitly authorized invocation.
func (c *Client) Stop(ctx context.Context, p Intent) (Observation, error) {
	var zero Observation
	if time.Since(p.CreatedAt) < 0 || time.Since(p.CreatedAt) > 15*time.Minute {
		return zero, ErrProfile
	}
	if _, err := c.Inspect(ctx, p); err != nil {
		return zero, err
	}
	for _, bound := range p.Containers {
		if c.call(ctx, "POST", "/containers/"+bound.ID+"/update", []byte(`{"RestartPolicy":{"Name":"no","MaximumRetryCount":0}}`), nil, 200) != nil {
			return zero, ErrUnknown
		}
	}
	// Require all policy changes confirmed before stopping any container.
	o, err := c.Inspect(ctx, p)
	if err != nil {
		return zero, ErrUnknown
	}
	for _, s := range o.Containers {
		if s.RestartPolicy != "no" {
			return zero, ErrUnknown
		}
	}
	// Stop application processes first and PostgreSQL last. No graceful window:
	// this is a recovery stop, not a normal restart with task draining.
	for _, service := range []string{"worker", "api", "admin", "migrate", "db"} {
		for _, bound := range p.Containers {
			if bound.Service == service {
				if c.call(ctx, "POST", "/containers/"+bound.ID+"/stop?t=0&signal=SIGKILL", nil, nil, 204, 304) != nil {
					return zero, ErrUnknown
				}
			}
		}
	}
	o, err = c.Inspect(ctx, p)
	if err != nil || !o.ProjectProcessesStopped {
		return zero, ErrUnknown
	}
	return o, nil
}
