// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentplatform.local/platform/internal/webhookruntime"
)

// The parent owns a random test schema and the TLS receiver. This child never
// migrates/bootstrap/chooses another schema or accepts a normal product command.
func TestWebhookRuntimeAuthorCrashChild(t *testing.T) {
	if os.Getenv("PLATFORM_TEST_WEBHOOK_CRASH_CHILD") != "1" {
		t.Skip("owned process-kill child entrypoint")
	}
	dsn := os.Getenv("PLATFORM_TEST_WEBHOOK_CRASH_DSN")
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(u.Query().Get("search_path"), "test_") {
		t.Fatal("child requires an owned disposable schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal("child open failed")
	}
	defer s.Close()
	var schema string
	if err = s.Pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil || schema != u.Query().Get("search_path") {
		t.Fatal("child schema mismatch")
	}
	l, p, err := s.ClaimWebhook(ctx, "author_process_worker", time.Second)
	if err != nil || l == nil {
		t.Fatal("child claim failed")
	}
	mode := os.Getenv("PLATFORM_TEST_WEBHOOK_CRASH_MODE")
	ready := func() {
		if err := os.WriteFile(os.Getenv("PLATFORM_TEST_WEBHOOK_CRASH_READY"), []byte("owned-child-ready"), 0600); err != nil {
			t.Fatal("child handshake failed")
		}
	}
	if mode == "before_marker" {
		ready()
		<-ctx.Done()
		return
	}
	if mode == "after_marker" {
		if _, err = s.PrepareWebhookDispatch(ctx, l); err != nil {
			t.Fatal("child marker failed")
		}
		ready()
		<-ctx.Done()
		return
	}
	if mode != "after_receiver_commit" {
		t.Fatal("invalid child stage")
	}
	r, err := webhookruntime.New(s, webhookruntime.Config{Profile: p, EncodedSigningKey: os.Getenv("PLATFORM_TEST_WEBHOOK_CRASH_KEY"), PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal("child runtime failed")
	}
	_, _ = r.Execute(ctx, l)
}

func TestWebhookRuntimeAuthorProcessKillBoundaries(t *testing.T) {
	for _, mode := range []string{"before_marker", "after_marker", "after_receiver_commit"} {
		t.Run(mode, func(t *testing.T) {
			receiverMode := "ok"
			if mode == "after_receiver_commit" {
				receiverMode = "block_after_commit"
			}
			f := webhookRuntimeSetup(t, receiverMode)
			ctx := context.Background()
			deliveries, _, err := f.s.WebhookDeliveries(ctx, f.p, f.task.ID, "", 10)
			if err != nil || len(deliveries) != 1 {
				t.Fatal("parent delivery setup failed", err)
			}
			id := deliveries[0].ID
			ready := filepath.Join(t.TempDir(), "ready")
			var childOutput bytes.Buffer
			cmd := exec.Command(os.Args[0], "-test.run=^TestWebhookRuntimeAuthorCrashChild$", "-test.timeout=45s")
			cmd.Stdout = &childOutput
			cmd.Stderr = &childOutput
			cmd.Env = append(os.Environ(), "PLATFORM_TEST_WEBHOOK_CRASH_CHILD=1", "PLATFORM_TEST_WEBHOOK_CRASH_DSN="+f.s.Pool.Config().ConnString(), "PLATFORM_TEST_WEBHOOK_CRASH_MODE="+mode, "PLATFORM_TEST_WEBHOOK_CRASH_READY="+ready, "PLATFORM_TEST_WEBHOOK_CRASH_KEY="+f.encoded)
			if err = cmd.Start(); err != nil {
				t.Fatal("start owned webhook child")
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					_ = cmd.Process.Kill()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("owned child cleanup timed out")
					}
				}
			})
			if mode == "after_receiver_commit" {
				select {
				case <-f.committed:
				case <-done:
					stopped = true
					t.Fatal("child exited before receiver commit")
				case <-time.After(8 * time.Second):
					t.Fatal("receiver never committed child request")
				}
			} else {
				deadline := time.Now().Add(8 * time.Second)
				for {
					if b, e := os.ReadFile(ready); e == nil && string(b) == "owned-child-ready" {
						break
					}
					select {
					case <-done:
						stopped = true
						t.Fatal("child exited before handshake")
					default:
					}
					if time.Now().After(deadline) {
						t.Fatal("child marker handshake timed out")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal("kill owned webhook child")
			}
			select {
			case err = <-done:
				stopped = true
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal("child was not killed")
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal("child did not terminate by SIGKILL")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("killed child did not exit")
			}
			d := f.delivery(t, id)
			want := "sent"
			if mode == "before_marker" {
				want = "leased"
			}
			if d.Status != want {
				t.Fatal("child boundary durable state changed", d.Status, want)
			}
			var leaseUntil time.Time
			if err = f.s.Pool.QueryRow(ctx, "SELECT lease_until FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, d.CurrentAttemptID).Scan(&leaseUntil); err != nil {
				t.Fatal(err)
			}
			if delay := time.Until(leaseUntil) + 30*time.Millisecond; delay > 0 {
				time.Sleep(delay)
			}
			if mode == "before_marker" {
				l := f.claim(t)
				if l.Token < 2 {
					t.Fatal("new worker did not fence killed unsent claim")
				}
				if _, err = f.runtime(t, f.s).Execute(ctx, l); err != nil {
					t.Fatal(err)
				}
				if f.calls.Load() != 1 {
					t.Fatal("proven-unsent child recovery request count", f.calls.Load())
				}
			} else {
				if changed, e := f.s.ReconcileWebhookSent(ctx); e != nil || !changed {
					t.Fatal("killed marked attempt not classified unknown", e)
				}
				d = f.delivery(t, id)
				if d.Status != "unknown" || d.EverUnknownAttempts != 1 {
					t.Fatal("kill erased marked uncertainty", d.Status)
				}
				for i := 0; i < 3; i++ {
					l, _, e := f.s.ClaimWebhook(ctx, "no_automatic_resend", time.Second)
					if e != nil || l != nil {
						t.Fatal("killed marked attempt automatically resent", e)
					}
				}
				wantBefore := int64(0)
				if mode == "after_receiver_commit" {
					wantBefore = 1
				}
				if f.calls.Load() != wantBefore {
					t.Fatal("physical requests changed before explicit retry", f.calls.Load())
				}
				f.retry(t, d)
				l := f.claim(t)
				if _, err = f.runtime(t, f.s).Execute(ctx, l); err != nil {
					t.Fatal(err)
				}
				if f.calls.Load() != wantBefore+1 {
					t.Fatal("explicit retry physical request count", f.calls.Load())
				}
			}
			var inbox int
			if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM author_webhook_receiver_inbox").Scan(&inbox); err != nil || inbox != 1 {
				t.Fatal("process-kill duplicate inbox processing", err, inbox)
			}
			d = f.delivery(t, id)
			if d.Status != "acknowledged" {
				t.Fatal("recovered delivery unacknowledged")
			}
			t.Logf("SIGKILL stage=%s physical_requests=%d durable_inbox_rows=%d attempts=%d", mode, f.calls.Load(), inbox, d.AttemptCount)
		})
	}
}
