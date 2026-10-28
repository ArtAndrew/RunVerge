// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type taskEventCursor struct {
	Version        int    `json:"v"`
	OrganizationID string `json:"org"`
	TaskID         string `json:"task"`
	After          string `json:"after"`
}

func encodeTaskEventCursor(org, task string, after int64) string {
	body, _ := json.Marshal(taskEventCursor{domain.EventCursorVersion, org, task, strconv.FormatInt(after, 10)})
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeTaskEventCursor(raw, org, task string) (int64, error) {
	invalid := domain.Err("invalid_request", "Некорректный cursor событий", 400)
	if raw == "" {
		return 0, nil
	}
	if len(raw) > 1024 {
		return 0, invalid
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || !utf8.Valid(body) {
		return 0, invalid
	}
	var value taskEventCursor
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF || value.Version != domain.EventCursorVersion || value.OrganizationID != org || value.TaskID != task {
		return 0, invalid
	}
	after, err := strconv.ParseInt(value.After, 10, 64)
	if err != nil || after < 0 || strconv.FormatInt(after, 10) != value.After || encodeTaskEventCursor(org, task, after) != raw {
		return 0, invalid
	}
	return after, nil
}

func eventReplayUnavailable() error {
	return domain.Err("invalid_transition", "История содержит событие, недоступное для потокового чтения; получите снимок задачи", 409)
}

// Bound the value returned by PostgreSQL as well as the serialized page. An
// arbitrary historical JSON value must not bypass stream memory/frame limits.
const taskEventColumns = `id,organization_id,task_id,sequence,type,
 CASE WHEN octet_length(data::text)<=$3 THEN data::text ELSE NULL END,created_at,payload_version`

func scanTaskEvent(row scanner) (domain.Event, error) {
	var event domain.Event
	var data *string
	if err := row.Scan(&event.ID, &event.OrganizationID, &event.TaskID, &event.Sequence, &event.Type, &data, &event.CreatedAt, &event.PayloadVersion); err != nil {
		return event, err
	}
	if data == nil {
		return event, eventReplayUnavailable()
	}
	switch event.PayloadVersion {
	case 0:
		// Preserve PostgreSQL's exact stored JSON representation, including wide
		// integer tokens. Historical maps are not relabeled as typed v1 facts.
		event.LegacyDataJSON = *data
	case domain.EventPayloadVersion:
		decoder := json.NewDecoder(bytes.NewBufferString(*data))
		decoder.UseNumber()
		if err := decoder.Decode(&event.Data); err != nil {
			return event, eventReplayUnavailable()
		}
		if err := domain.ValidateEventWire(event); err != nil {
			return event, eventReplayUnavailable()
		}
	default:
		return event, eventReplayUnavailable()
	}
	return event, nil
}

func taskEventWindowTx(ctx context.Context, tx pgx.Tx, org, task string) (domain.EventReplayWindow, error) {
	var window domain.EventReplayWindow
	err := tx.QueryRow(ctx, `SELECT after_sequence,version,
 COALESCE((SELECT max(sequence) FROM events e WHERE e.organization_id=w.organization_id AND e.task_id=w.task_id),0)
 FROM task_event_windows w WHERE organization_id=$1 AND task_id=$2`, org, task).Scan(&window.AfterSequence, &window.Version, &window.ThroughSequence)
	return window, rowExists(err)
}

// TaskEventSnapshot observes task state, retained-window coordinates, and its
// bounded recent preview in one freshly authorized database snapshot.
func (s *Store) TaskEventSnapshot(ctx context.Context, p domain.Principal, taskID string) (domain.TaskEventSnapshot, error) {
	out := domain.TaskEventSnapshot{OrganizationID: p.OrganizationID, TaskID: taskID, Events: []domain.Event{}}
	tx, err := s.beginMaterialRead(ctx, p, taskID)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if out.Task, err = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, taskID)); err != nil {
		return out, err
	}
	if out.Window, err = taskEventWindowTx(ctx, tx, p.OrganizationID, taskID); err != nil {
		return out, err
	}
	if err = tx.QueryRow(ctx, "SELECT transaction_timestamp()").Scan(&out.ObservedAt); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT `+taskEventColumns+` FROM events WHERE organization_id=$1 AND task_id=$2 AND sequence>$4 ORDER BY sequence DESC LIMIT $5`, p.OrganizationID, taskID, domain.MaxSnapshotEventBytes, out.Window.AfterSequence, domain.EventSnapshotLimit+1)
	if err != nil {
		return out, err
	}
	bytesUsed := 2
	for rows.Next() {
		event, scanErr := scanTaskEvent(rows)
		if scanErr != nil {
			var publicError *domain.Error
			if errors.As(scanErr, &publicError) {
				out.HasEarlierEvents = true
				break
			}
			rows.Close()
			return out, scanErr
		}
		encoded, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			rows.Close()
			return out, marshalErr
		}
		if len(out.Events) == domain.EventSnapshotLimit || bytesUsed+len(encoded)+1 > domain.MaxSnapshotEventBytes {
			out.HasEarlierEvents = true
			break
		}
		bytesUsed += len(encoded) + 1
		out.Events = append(out.Events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for left, right := 0, len(out.Events)-1; left < right; left, right = left+1, right-1 {
		out.Events[left], out.Events[right] = out.Events[right], out.Events[left]
	}
	// A floor may fall in a legitimate global-sequence gap before this task's
	// first event. Report actual earlier task events, not merely a nonzero floor.
	if !out.HasEarlierEvents {
		if len(out.Events) == 0 {
			out.HasEarlierEvents = out.Window.ThroughSequence > 0
		} else if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE organization_id=$1 AND task_id=$2 AND sequence<$3)", p.OrganizationID, taskID, out.Events[0].Sequence).Scan(&out.HasEarlierEvents); err != nil {
			return out, err
		}
	}
	out.Cursor = encodeTaskEventCursor(p.OrganizationID, taskID, out.Window.ThroughSequence)
	out.ReplayCursor = encodeTaskEventCursor(p.OrganizationID, taskID, out.Window.AfterSequence)
	return out, tx.Commit(ctx)
}

func (s *Store) ReplayTaskEvents(ctx context.Context, p domain.Principal, taskID, raw string, limit int) (domain.TaskEventBatch, error) {
	out := domain.TaskEventBatch{OrganizationID: p.OrganizationID, TaskID: taskID, Events: []domain.Event{}}
	if limit < 1 || limit > 100 {
		return out, domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	tx, err := s.beginMaterialRead(ctx, p, taskID)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// The cursor is only a scoped read coordinate. Authorize first so a foreign
	// coordinate cannot be used as an identity or task-existence oracle.
	after, err := decodeTaskEventCursor(raw, p.OrganizationID, taskID)
	if err != nil {
		return out, err
	}
	if out.Window, err = taskEventWindowTx(ctx, tx, p.OrganizationID, taskID); err != nil {
		return out, err
	}
	if after > out.Window.ThroughSequence {
		return out, domain.Err("invalid_request", "cursor опережает историю задачи", 400)
	}
	if after < out.Window.AfterSequence {
		out.ResetRequired, out.ResetReason = true, "history_expired"
		return out, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT `+taskEventColumns+` FROM events WHERE organization_id=$1 AND task_id=$2 AND sequence>$4 ORDER BY sequence LIMIT $5`, p.OrganizationID, taskID, domain.MaxEventBatchBytes, after, limit+1)
	if err != nil {
		return out, err
	}
	bytesUsed := 2
	for rows.Next() {
		event, scanErr := scanTaskEvent(rows)
		if scanErr != nil {
			rows.Close()
			return out, scanErr
		}
		encoded, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			rows.Close()
			return out, marshalErr
		}
		if len(out.Events) == limit || bytesUsed+len(encoded)+1 > domain.MaxEventBatchBytes {
			if len(out.Events) == 0 {
				rows.Close()
				return out, eventReplayUnavailable()
			}
			out.HasMore = true
			break
		}
		out.Events = append(out.Events, event)
		bytesUsed += len(encoded) + 1
		after = event.Sequence
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Cursor = encodeTaskEventCursor(p.OrganizationID, taskID, after)
	return out, tx.Commit(ctx)
}

// A fresh single-statement read after the replay transaction checks current
// credential, effective permissions and task scope immediately before I/O.
func (s *Store) CheckEventReadAccess(ctx context.Context, p domain.Principal, taskID string) error {
	var actor domain.Principal
	var exists bool
	tx, err := s.beginRuntimeTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `WITH actor(id,organization_id,role,kind) AS (`+authQuery(p)+`)
 SELECT id,organization_id,role,kind,EXISTS(SELECT 1 FROM tasks WHERE organization_id=$2 AND id=$3) FROM actor`, p.TokenHash, p.OrganizationID, taskID).Scan(&actor.ID, &actor.OrganizationID, &actor.Role, &actor.Kind, &exists)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (actor.ID != p.ID || actor.OrganizationID != p.OrganizationID)) {
		return domain.Unauthenticated
	}
	if err != nil {
		return err
	}
	if !domain.PrincipalHasPermission(actor, "tasks.read") {
		return domain.Forbidden
	}
	if !exists {
		return domain.NotFound
	}
	return nil
}

// AdvanceEventReplayWindow is a trusted-host operation. It changes only the
// logical live replay window; archived events, outbox, and facts remain intact.
func (s *Store) AdvanceEventReplayWindow(ctx context.Context, org, task string, after int64, reason string) (domain.EventReplayWindow, error) {
	var out domain.EventReplayWindow
	if !validText(org, 128) || !validText(task, 128) || after < 0 || !validText(reason, 4096) {
		return out, domain.Err("invalid_request", "Некорректное окно событий или причина", 400)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var found string
	if err = tx.QueryRow(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", org).Scan(&found); err != nil {
		return out, rowExists(err)
	}
	if err = tx.QueryRow(ctx, "SELECT task_id FROM task_event_windows WHERE organization_id=$1 AND task_id=$2 FOR UPDATE", org, task).Scan(&found); err != nil {
		return out, rowExists(err)
	}
	if out, err = taskEventWindowTx(ctx, tx, org, task); err != nil {
		return out, err
	}
	if after < out.AfterSequence {
		return out, domain.Err("invalid_transition", "Окно событий нельзя вернуть назад", 409)
	}
	if after > out.ThroughSequence {
		return out, domain.Err("invalid_request", "Окно опережает историю задачи", 400)
	}
	if after == out.AfterSequence {
		return out, tx.Commit(ctx)
	}
	previous := out.AfterSequence
	if err = tx.QueryRow(ctx, `UPDATE task_event_windows SET after_sequence=$3 WHERE organization_id=$1 AND task_id=$2 RETURNING version`, org, task, after).Scan(&out.Version); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO event_replay_window_audit(organization_id,id,task_id,old_after_sequence,new_after_sequence,reason) VALUES($1,$2,$3,$4,$5,$6)`, org, domain.NewID("ewa"), task, previous, after, reason); err != nil {
		return out, err
	}
	if err = notifyTaskEventsTx(ctx, tx, org, task); err != nil {
		return out, err
	}
	out.AfterSequence = after
	return out, tx.Commit(ctx)
}
