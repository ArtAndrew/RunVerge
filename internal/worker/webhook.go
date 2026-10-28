// SPDX-License-Identifier: Elastic-2.0
package worker

import (
	"context"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/webhookruntime"
)

func (w *Worker) executeWebhook(ctx context.Context, l *domain.WebhookLease, profile domain.WebhookDestinationProfile) {
	stopUnsent := func(code string) {
		persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Store.StopWebhookUnsent(persist, l, code)
	}
	if webhookruntime.ValidateProfile(profile) != nil {
		stopUnsent("invalid_profile")
		return
	}
	encoded, err := w.ResolveWebhookSigningKey(profile.SigningKeyEnvironment)
	if err != nil || encoded == "" {
		stopUnsent("credentials_unavailable")
		return
	}
	runtime, err := webhookruntime.New(w.Store, webhookruntime.Config{Profile: profile, EncodedSigningKey: encoded, PersistenceTimeout: 5 * time.Second})
	encoded = ""
	if err != nil {
		stopUnsent("credentials_unavailable")
		return
	}
	if _, err = runtime.Execute(ctx, l); err != nil {
		// SQL's exact sent marker decides whether this is still proven unsent.
		// A generic execution/COMMIT exception never resets a marked attempt.
		code := "execution_not_admitted"
		if ctx.Err() != nil {
			code = "execution_cancelled"
		}
		stopUnsent(code)
		if ctx.Err() == nil {
			w.Log.Warn("webhook execution requires attention")
		}
	}
}
