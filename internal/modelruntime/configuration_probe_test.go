// SPDX-License-Identifier: Elastic-2.0
package modelruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

type configurationProbeFaultStore struct {
	*faultStore
	observation            domain.ConfigurationProbeObservation
	observations           int
	revokeAfterObservation bool
	observationError       bool
}

func (s *configurationProbeFaultStore) RecordConfigurationModelProbeObservation(ctx context.Context, l *domain.Lease, reservationID string, in domain.ConfigurationProbeObservation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations++
	if ctx.Err() != nil || l.ConfigurationVerificationID != "cfg_probe" || reservationID != s.reservation.ID || in.ReservationID != reservationID || in.IntentDigest != s.intent.IntentHash || in.WorkerID != l.OwnerID || in.FencingToken != l.Token || in.VerificationID != l.ConfigurationVerificationID || in.ResponseDigest == "" {
		return true, errors.New("invalid safe fact binding")
	}
	if s.observationError {
		return true, errors.New("private ingestion failure")
	}
	s.observation = in
	if s.revokeAfterObservation {
		s.authorized = false
	}
	return true, nil
}

func TestConfigurationModelProbeDiscardsCredentialEchoAndPreservesLateSafeFact(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "safe_candidate", true: "revoked_publication"}[revoke], func(t *testing.T) {
			store := &configurationProbeFaultStore{faultStore: newStore(), revokeAfterObservation: revoke}
			const echo = "synthetic-credential-echo-from-peer"
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": echo, "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": echo}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2}})
			}))
			defer peer.Close()
			runtime, err := modelruntime.New(store, testConfig(peer.URL))
			if err != nil {
				t.Fatal(err)
			}
			lease := testLease()
			lease.ConfigurationVerificationID = "cfg_probe"
			out, err := runtime.Execute(context.Background(), lease)
			if revoke {
				checkCode(t, err, "candidate_not_committed", true)
				if store.artifact.ID != "" {
					t.Fatal("revoked candidate published")
				}
			} else {
				if err != nil || out.Status != modelruntime.CandidateWaitingInput {
					t.Fatal("probe did not finish", err)
				}
				if store.candidate.ProviderRequestID != "" || strings.Contains(store.candidate.Text, echo) || len(store.candidate.ToolCalls) != 0 {
					t.Fatal("credential echo persisted")
				}
			}
			if store.observations != 1 || len(store.observation.ResponseDigest) != 64 || store.reservation.Status != "unknown" || store.reservation.ActualMicroUSD != nil {
				t.Fatal("safe late receipt or financial hold lost")
			}
			raw, _ := json.Marshal(store.observation)
			if strings.Contains(string(raw), echo) {
				t.Fatal("raw peer content crossed receipt boundary")
			}
		})
	}
}

func TestConfigurationProbeReceiptFailureSuppressesPublicationAndOrdinaryModelsSkipObserver(t *testing.T) {
	for _, probe := range []bool{false, true} {
		store := &configurationProbeFaultStore{faultStore: newStore(), observationError: true}
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { success(w, "ordinary candidate", 3, 2) }))
		runtime, err := modelruntime.New(store, testConfig(peer.URL))
		if err != nil {
			peer.Close()
			t.Fatal(err)
		}
		lease := testLease()
		if probe {
			lease.ConfigurationVerificationID = "cfg_probe"
		}
		out, err := runtime.Execute(context.Background(), lease)
		peer.Close()
		if probe {
			checkCode(t, err, "probe_receipt_not_committed", true)
			if store.artifact.ID != "" || store.observations != 1 {
				t.Fatal("failed receipt published candidate")
			}
		} else if err != nil || out.Status != modelruntime.CandidateWaitingInput || store.observations != 0 {
			t.Fatal("ordinary completion entered probe ingestion", err)
		}
	}
}
