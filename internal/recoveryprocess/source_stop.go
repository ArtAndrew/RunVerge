// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// SourceStopRecord is retained metadata, not current authority. Physical source
// identity comes from the earlier live binding; a stopped database cannot issue
// a new SQL witness. The live observation covers the exact local Docker inventory.
type SourceStopRecord struct {
	Intent      Intent        `json:"intent"`
	Binding     SourceBinding `json:"binding"`
	Observation Observation   `json:"observation"`
}

type ObservedSourceStop struct{ record SourceStopRecord }

func cloneSourceStop(r SourceStopRecord) SourceStopRecord {
	r.Intent.Containers = append([]Container(nil), r.Intent.Containers...)
	r.Binding.Processes = append([]SourceProcess(nil), r.Binding.Processes...)
	r.Observation.Containers = append([]ContainerState(nil), r.Observation.Containers...)
	return r
}
func (o ObservedSourceStop) Record() SourceStopRecord { return cloneSourceStop(o.record) }

// SourceStopRecordConsistent validates representation only. It constructs no
// opaque proof, authenticates no caller data and performs no live observation.
func SourceStopRecordConsistent(r SourceStopRecord) bool {
	p, b, o := r.Intent, r.Binding, r.Observation
	system, err := strconv.ParseUint(b.SystemID, 10, 64)
	if !valid(p) || b.Version != 1 || b.PlanSHA256 != planDigest(p) || !sourceID.MatchString(b.SourceID) || !fullID.MatchString(b.SnapshotSHA256) || err != nil || system == 0 || strconv.FormatUint(system, 10) != b.SystemID || b.DatabaseOID == 0 || b.ObservedAt.Before(p.CreatedAt) || o.ObservedAt.Before(b.ObservedAt) || !strings.HasPrefix(b.RuntimeImage, "sha256:") || !fullID.MatchString(strings.TrimPrefix(b.RuntimeImage, "sha256:")) {
		return false
	}
	for _, stamp := range []time.Time{p.CreatedAt, b.ObservedAt, o.ObservedAt} {
		_, offset := stamp.Zone()
		if stamp.IsZero() || offset != 0 {
			return false
		}
	}
	if o.OperationID != p.OperationID || o.Project != p.Project || o.SourceID != b.SourceID || !o.ProjectProcessesStopped || !o.SourceInstallationBound || !o.BoundSourceProcessesStopped || o.ExternalExecutionFenced || o.ExternalEffectsSettled || len(o.Containers) != len(p.Containers) {
		return false
	}
	apps := 0
	for i, c := range p.Containers {
		s := o.Containers[i]
		if s.ID != c.ID || s.Service != c.Service || !s.Stopped || s.RestartPolicy != "no" || (s.Status != "created" && s.Status != "exited") || c.RestartCount < 0 {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, c.Created); err != nil {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, c.StartedAt); err != nil {
			return false
		}
		switch c.Service {
		case "db":
			if c.ID != b.DatabaseContainer {
				return false
			}
		case "api", "worker":
			if c.Image != b.RuntimeImage || apps >= len(b.Processes) || b.Processes[apps].Container != c.ID || !operationID.MatchString(b.Processes[apps].BootID) {
				return false
			}
			apps++
		case "admin", "migrate":
			if c.Image != b.RuntimeImage {
				return false
			}
		default:
			return false
		}
	}
	return apps == len(b.Processes)
}

// ObserveSourceStop performs bounded reads only. Unlike destructive stop, a fresh
// recheck may use an old intent if every pinned identity still matches. The host
// operator must maintain exclusive control; this cannot prevent manual restart.
func (c *Client) ObserveSourceStop(ctx context.Context, p Intent, b SourceBinding) (ObservedSourceStop, error) {
	if c == nil || ctx == nil {
		return ObservedSourceStop{}, ErrProfile
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p.Containers = append([]Container(nil), p.Containers...)
	b.Processes = append([]SourceProcess(nil), b.Processes...)
	o, err := c.InspectBoundSource(ctx, p, b)
	r := SourceStopRecord{p, b, o}
	if err != nil || ctx.Err() != nil || !SourceStopRecordConsistent(r) {
		return ObservedSourceStop{}, ErrProfile
	}
	return ObservedSourceStop{cloneSourceStop(r)}, nil
}

// SameSourceStop compares retained identities/states, allowing only the fresh
// observation timestamp to differ. The caller still needs signature verification
// for historical records and ObserveSourceStop for current Docker state.
func SameSourceStop(a, b SourceStopRecord) bool {
	if !SourceStopRecordConsistent(a) || !SourceStopRecordConsistent(b) {
		return false
	}
	a.Observation.ObservedAt, b.Observation.ObservedAt = time.Time{}, time.Time{}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
