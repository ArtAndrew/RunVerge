// SPDX-License-Identifier: Elastic-2.0
package worker

import (
	"agentplatform.local/platform/internal/configurationruntime"
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/toolruntime"
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"
)

type Worker struct {
	Store                    *storage.Store
	Log                      *slog.Logger
	ID                       string
	Concurrency              int
	ResolveModelCredential   func(string) (string, error)
	ResolveToolCredential    func(string) (string, error)
	ResolveMCPCredential     func(string) (string, error)
	ResolveWebhookSigningKey func(string) (string, error)
	// Internal prerelease switch until the complete0.14 contract is published.
	EnableWebhooks bool
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.ID == "" {
		w.ID = domain.NewID("worker")
	}
	if w.Concurrency == 0 {
		w.Concurrency = 4
	}
	if w.Concurrency < 1 || w.Concurrency > 64 {
		return domain.Err("invalid_request", "Invalid worker concurrency", 400)
	}
	if err := w.Store.CheckRuntimeAdmission(ctx); err != nil {
		return err
	}
	if w.ResolveModelCredential == nil {
		w.ResolveModelCredential = func(name string) (string, error) {
			value, ok := os.LookupEnv(name)
			if !ok || value == "" {
				return "", domain.Forbidden
			}
			return value, nil
		}
	}
	if w.ResolveToolCredential == nil {
		w.ResolveToolCredential = func(name string) (string, error) {
			if name == "" {
				return "", nil
			}
			value, ok := os.LookupEnv(name)
			if !ok || value == "" {
				return "", domain.Forbidden
			}
			return value, nil
		}
	}
	if w.ResolveMCPCredential == nil {
		w.ResolveMCPCredential = func(name string) (string, error) {
			value, ok := os.LookupEnv(name)
			if !ok || value == "" {
				return "", domain.Forbidden
			}
			return value, nil
		}
	}
	if w.ResolveWebhookSigningKey == nil {
		w.ResolveWebhookSigningKey = func(name string) (string, error) {
			value, ok := os.LookupEnv(name)
			if !ok || value == "" {
				return "", domain.Forbidden
			}
			return value, nil
		}
	}
	slots := make(chan struct{}, w.Concurrency)
	var running sync.WaitGroup
	defer running.Wait()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	toolTurn := false
	configurationTurn := 0
	webhookTurn := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if w.EnableWebhooks {
				if _, e := w.Store.ReconcileWebhookSent(ctx); e != nil && ctx.Err() == nil {
					w.Log.Error("webhook reconciliation failed")
				}
			}
			if _, e := w.Store.ReconcileFixtureReservation(ctx); e != nil && ctx.Err() == nil {
				w.Log.Error("fixture reconciliation failed")
			}
			if _, e := w.Store.ReconcileModelUnsent(ctx); e != nil && ctx.Err() == nil {
				w.Log.Error("unsent model reconciliation failed")
			}
			if _, e := w.Store.ReconcileTeam(ctx); e != nil && ctx.Err() == nil {
				w.Log.Error("team coordination failed")
			}
			if _, e := w.Store.ReconcileToolUnsent(ctx); e != nil && ctx.Err() == nil {
				w.Log.Error("tool reconciliation failed")
			}
			if _, e := w.Store.ReconcileTaskAutomation(ctx); e != nil && ctx.Err() == nil {
				w.Log.Error("task automation coordination failed")
			}
			if _, e := w.Store.ReconcileConfigurationVerifications(ctx); e != nil && ctx.Err() == nil {
				w.Log.Error("configuration verification reconciliation failed")
			}
			if len(slots) >= cap(slots) {
				continue
			}
			toolTurn = !toolTurn
			configurationTurn = (configurationTurn + 1) % 3
			webhookTurn = (webhookTurn + 1) % 4
			startWebhook := func() bool {
				if !w.EnableWebhooks {
					return false
				}
				lease, profile, err := w.Store.ClaimWebhook(ctx, w.ID, 45*time.Second)
				if err != nil {
					if ctx.Err() == nil {
						w.Log.Error("webhook claim failed")
					}
					return false
				}
				if lease == nil {
					return false
				}
				slots <- struct{}{}
				running.Add(1)
				go func() { defer running.Done(); defer func() { <-slots }(); w.executeWebhook(ctx, lease, profile) }()
				return true
			}
			if webhookTurn == 0 && startWebhook() {
				continue
			}
			startConfiguration := func() bool {
				lease, config, err := w.Store.ClaimConfigurationVerification(ctx, w.ID, time.Minute)
				if err != nil {
					if ctx.Err() == nil {
						w.Log.Error("configuration verification claim failed")
					}
					return false
				}
				if lease == nil {
					return false
				}
				slots <- struct{}{}
				running.Add(1)
				go func() {
					defer running.Done()
					defer func() { <-slots }()
					w.executeConfigurationVerification(ctx, lease, config)
				}()
				return true
			}
			if configurationTurn == 0 && startConfiguration() {
				continue
			}
			startTool := func() bool {
				lease, connection, err := w.Store.ClaimToolExecution(ctx, w.ID, 30*time.Second)
				if err != nil {
					if ctx.Err() == nil {
						w.Log.Error("tool claim failed")
					}
					return false
				}
				if lease == nil {
					return false
				}
				slots <- struct{}{}
				running.Add(1)
				go func() { defer running.Done(); defer func() { <-slots }(); w.executeTool(ctx, lease, connection) }()
				return true
			}
			if toolTurn && startTool() {
				continue
			}
			l, e := w.Store.ClaimFixture(ctx, w.ID, 30*time.Second)
			if e != nil {
				if ctx.Err() != nil {
					return nil
				}
				w.Log.Error("fixture claim failed")
				continue
			}
			if l == nil {
				modelLease, connection, claimErr := w.Store.ClaimModel(ctx, w.ID, 30*time.Second)
				if claimErr != nil {
					if ctx.Err() == nil {
						w.Log.Error("model claim failed")
					}
					continue
				}
				if modelLease == nil {
					if !toolTurn {
						if startTool() {
							continue
						}
					}
					if !startConfiguration() {
						startWebhook()
					}
					continue
				}
				slots <- struct{}{}
				running.Add(1)
				go func() { defer running.Done(); defer func() { <-slots }(); w.executeModel(ctx, modelLease, connection) }()
				continue
			}
			if e = w.Store.CheckLease(ctx, l); e != nil {
				continue
			}
			// A deterministic in-process fixture, explicitly not a model or code sandbox.
			output := "Тестовый результат\n" + domain.ExecutionInput(*l)
			if e = w.Store.CompleteFixture(ctx, l, output); e != nil {
				w.Log.Error("fixture completion failed", "operation_id", l.OperationID)
			}
		}
	}
}

func (w *Worker) executeConfigurationVerification(ctx context.Context, lease *domain.ConfigurationVerificationLease, config domain.ConfigurationProbeConfig) {
	credential, err := w.ResolveMCPCredential(config.Purpose.CredentialEnvironment)
	if err != nil || credential == "" {
		_ = w.Store.PauseConfigurationVerification(ctx, lease, "mcp_credentials_unavailable")
		return
	}
	out, err := configurationruntime.Execute(ctx, w.Store, lease, configurationruntime.Config{Probe: config, Credential: credential})
	credential = ""
	if err != nil {
		w.Log.Warn("configuration verification requires attention", "verification_id", lease.ID, "outcome", out.Status)
		persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		// Storage uses its marker, never the transport error, to decide whether
		// this was proven unsent. A sent/unknown occurrence cannot be queued.
		_ = w.Store.PauseConfigurationVerification(persist, lease, "mcp_verification_requires_attention")
	}
}

func (w *Worker) executeTool(ctx context.Context, l *domain.ToolLease, c domain.ToolConnection) {
	credential, err := w.ResolveToolCredential(c.CredentialEnvironment)
	if err != nil {
		_ = w.Store.PauseToolExecution(ctx, l, "tool_credentials_unavailable")
		return
	}
	runtime, err := toolruntime.New(w.Store, toolruntime.Config{Connection: c, Credential: credential, PersistenceTimeout: 5 * time.Second})
	credential = ""
	if err != nil {
		_ = w.Store.PauseToolExecution(ctx, l, "tool_profile_unavailable")
		return
	}
	execute, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-execute.Done():
				return
			case <-ticker.C:
				renew, stop := context.WithTimeout(execute, 3*time.Second)
				err := w.Store.RenewToolLease(renew, l, 30*time.Second)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	out, err := runtime.Execute(execute, l)
	cancel()
	<-done
	if err != nil {
		w.Log.Warn("tool attempt requires attention", "execution_id", l.Execution.ID, "outcome", out.Status)
	}
	if err != nil || out.Status == "not_dispatched" {
		persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		// Storage decides from the durable dispatch marker, never from a
		// transport error or a missing acknowledgement, whether release is safe.
		_ = w.Store.PauseToolExecution(persist, l, "tool_attempt_requires_attention")
	}
}

func (w *Worker) executeModel(ctx context.Context, l *domain.Lease, connection domain.ModelConnection) {
	key, e := w.ResolveModelCredential(connection.CredentialEnvironment)
	if e != nil || key == "" {
		_ = w.Store.PauseModelAfterRuntimeError(ctx, l, "model_credentials_unavailable")
		return
	}
	runtime, e := modelruntime.New(w.Store, storage.RuntimeModelConfig(connection, key))
	key = ""
	if e != nil {
		_ = w.Store.PauseModelAfterRuntimeError(ctx, l, "model_profile_unavailable")
		return
	}
	execute, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-execute.Done():
				return
			case <-tick.C:
				renew, stop := context.WithTimeout(execute, 3*time.Second)
				e := w.Store.RenewModelLease(renew, l, 30*time.Second)
				stop()
				if e != nil {
					cancel()
					return
				}
			}
		}
	}()
	out, e := runtime.Execute(execute, l)
	cancel()
	<-heartbeatDone
	if e != nil {
		w.logModelFailure(l.OperationID, out.Status, e)
	}
	if out.Status == modelruntime.AlreadyAttempted {
		persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = w.Store.PauseModelAttempt(persist, l, out.ReservationID, "model_accounting_pending")
	}
	// A deterministic input rejection cannot improve by reclaiming the same
	// immutable task. Stop promptly instead of burning all lease attempts. Do
	// not apply this to a reserve/commit error, whose durable outcome is unknown.
	var executionError *modelruntime.ExecutionError
	if out.Status == modelruntime.NotDispatched && out.ReservationID == "" && errors.As(e, &executionError) && (executionError.Code == "input_limit_exceeded" || executionError.Code == "unsupported_execution_request") {
		persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = w.Store.PauseModelAttempt(persist, l, "", "model_input_rejected")
	}
	// Preserve specific known reasons above. If their current-authority guard
	// rejects cleanup, still inspect the durable marker/fence without requiring
	// the revoked identity. A recorded stop is not overwritten by this fallback.
	if e != nil {
		persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
		_ = w.Store.PauseModelAfterRuntimeError(persist, l, "model_attempt_requires_attention")
		stop()
	}
}
