// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const maxTaskEventSubscribers = 4096

type taskEventNotification struct {
	OrganizationID string `json:"organization_id"`
	TaskID         string `json:"task_id"`
}

func notifyTaskEventsTx(ctx context.Context, tx pgx.Tx, org, task string) error {
	notification, err := json.Marshal(taskEventNotification{OrganizationID: org, TaskID: task})
	if err != nil {
		return err
	}
	// PostgreSQL delivers this coalescible hint only if its transaction commits.
	// Durable replay/window state, never a notification, is the authority.
	_, err = tx.Exec(ctx, "SELECT pg_notify('platform_task_events',$1)", string(notification))
	return err
}

type taskEventSubscriber struct {
	wake chan struct{}
	stop func() bool
}

// One listener serves every subscription on a Store. Notifications are hints:
// channels hold at most one wakeup, and reconnect wakes everyone for replay.
type taskEventHub struct {
	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	ready       chan struct{}
	readyOnce   sync.Once
	closed      bool
	nextID      uint64
	subscribers map[taskEventNotification]map[uint64]*taskEventSubscriber
	count       int
	backendPID  uint32
}

// SubscribeTaskEvents is a trusted storage primitive, not an authorization
// decision. HTTP callers must freshly authorize before subscribing and before
// writing any replay/heartbeat. Cancellation unregisters and closes the channel.
func (s *Store) SubscribeTaskEvents(ctx context.Context, org, task string) (<-chan struct{}, error) {
	if !validText(org, 128) || !validText(task, 128) {
		return nil, domain.Err("invalid_request", "Некорректная область событий", 400)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.eventMu.Lock()
	if s.closed || s.Pool == nil {
		s.eventMu.Unlock()
		return nil, errors.New("event listener store is closed")
	}
	h := s.eventHub
	if h == nil {
		run, cancel := context.WithCancel(context.Background())
		h = &taskEventHub{cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), subscribers: map[taskEventNotification]map[uint64]*taskEventSubscriber{}}
		s.eventHub = h
		config := s.Pool.Config().ConnConfig.Copy()
		if config.RuntimeParams == nil {
			config.RuntimeParams = map[string]string{}
		}
		config.RuntimeParams["application_name"] = "platform-event-listener-" + domain.NewID("hub")
		go h.run(run, config)
	}
	s.eventMu.Unlock()

	scope := taskEventNotification{OrganizationID: org, TaskID: task}
	h.mu.Lock()
	if h.closed || h.count >= maxTaskEventSubscribers {
		h.mu.Unlock()
		return nil, domain.Err("invalid_transition", "Достигнут предел подписок на события", 409)
	}
	h.nextID++
	id := h.nextID
	sub := &taskEventSubscriber{wake: make(chan struct{}, 1)}
	if h.subscribers[scope] == nil {
		h.subscribers[scope] = map[uint64]*taskEventSubscriber{}
	}
	h.subscribers[scope][id] = sub
	h.count++
	sub.stop = context.AfterFunc(ctx, func() { h.remove(scope, id) })
	h.mu.Unlock()

	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-h.ready:
		if err := ctx.Err(); err != nil {
			h.remove(scope, id)
			return nil, err
		}
		return sub.wake, nil
	case <-h.done:
		h.remove(scope, id)
		return nil, errors.New("event listener store is closed")
	case <-wait.Done():
		h.remove(scope, id)
		return nil, wait.Err()
	}
}

func (h *taskEventHub) remove(scope taskEventNotification, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	group := h.subscribers[scope]
	if sub := group[id]; sub != nil {
		delete(group, id)
		h.count--
		if len(group) == 0 {
			delete(h.subscribers, scope)
		}
		sub.stop()
		close(sub.wake)
	}
}

func (h *taskEventHub) wake(scope *taskEventNotification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, group := range h.subscribers {
		if scope != nil && key != *scope {
			continue
		}
		for _, sub := range group {
			select {
			case sub.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (h *taskEventHub) run(ctx context.Context, config *pgx.ConnConfig) {
	defer func() {
		h.mu.Lock()
		h.closed = true
		h.backendPID = 0
		for _, group := range h.subscribers {
			for _, sub := range group {
				sub.stop()
				close(sub.wake)
			}
		}
		h.subscribers = nil
		h.count = 0
		h.mu.Unlock()
		close(h.done)
	}()
	backoff := 25 * time.Millisecond
	for ctx.Err() == nil {
		connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := pgx.ConnectConfig(connectCtx, config)
		if err == nil {
			_, err = conn.Exec(connectCtx, "LISTEN platform_task_events")
		}
		cancel()
		if err == nil {
			h.mu.Lock()
			h.backendPID = conn.PgConn().PID()
			h.mu.Unlock()
			// A missed notification during reconnect cannot lose durable history.
			h.wake(nil)
			h.readyOnce.Do(func() { close(h.ready) })
			backoff = 25 * time.Millisecond
			for ctx.Err() == nil {
				notification, waitErr := conn.WaitForNotification(ctx)
				if waitErr != nil {
					break
				}
				if scope, ok := decodeTaskEventNotification(notification.Payload); ok {
					h.wake(&scope)
				}
			}
		}
		if conn != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
			_ = conn.Close(closeCtx)
			closeCancel()
		}
		h.mu.Lock()
		h.backendPID = 0
		h.mu.Unlock()
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 2*time.Second {
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}
}

func decodeTaskEventNotification(payload string) (taskEventNotification, bool) {
	var value taskEventNotification
	if len(payload) > 512 || !utf8.ValidString(payload) {
		return value, false
	}
	decoder := json.NewDecoder(bytes.NewBufferString(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || !validText(value.OrganizationID, 128) || !validText(value.TaskID, 128) || decoder.Decode(&struct{}{}) != io.EOF {
		return value, false
	}
	return value, true
}
