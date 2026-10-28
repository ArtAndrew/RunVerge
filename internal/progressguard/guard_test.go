// SPDX-License-Identifier: Elastic-2.0
package progressguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func observation(id, identity, effect, outcome string) Observation {
	return Observation{ID: id, IntentFingerprint: digest(identity), Effect: effect, Outcome: outcome}
}

func hasSignal(decision Decision, code string) bool {
	for _, signal := range decision.Signals {
		if signal.Code == code {
			return true
		}
	}
	return false
}

func checkDecision(t *testing.T, history []Observation, proposed *Intent) Decision {
	t.Helper()
	got, err := Evaluate(context.Background(), DefaultPolicy(), history, proposed)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestStopBeforeRepeatedSideEffectAcrossNewCallAndOperation(t *testing.T) {
	for _, effect := range []string{"write", "destructive"} {
		t.Run(effect, func(t *testing.T) {
			original := Intent{ToolID: "crm", Effect: effect, ArgumentsJSON: `{"id":9007199254740993,"name":"A"}`}
			fp, err := IntentFingerprint(context.Background(), original)
			if err != nil {
				t.Fatal(err)
			}
			history := []Observation{
				{ID: "old_run:call_a", IntentFingerprint: fp, Effect: effect, Outcome: "succeeded", ProgressDigest: digest("before")},
				{ID: "new_grant:model_output", Outcome: "output", OutputDigest: digest("new plan"), ProgressDigest: digest("after")},
			}
			proposed := Intent{ToolID: "crm", Effect: "read", ArgumentsJSON: ` { "name":"\u0041", "id":90071992547409930e-1 } `}
			got := checkDecision(t, history, &proposed)
			if !got.Stop || !hasSignal(got, "repeated_effect_intent") || got.ProposedIntentFingerprint != fp {
				t.Fatalf("repeated effect admitted: %+v", got)
			}
			if got.Signals[0].Count != 2 || !reflect.DeepEqual(got.Signals[0].EvidenceIDs, []string{"old_run:call_a"}) {
				t.Fatalf("incorrect witnesses: %+v", got.Signals)
			}
			// A nearby large integer is a different business intent, not float-rounded.
			proposed.ArgumentsJSON = `{"name":"A","id":9007199254740992}`
			if got := checkDecision(t, history, &proposed); got.Stop {
				t.Fatalf("distinct integer falsely aliased: %+v", got)
			}
		})
	}
}

func TestUnknownStopsEvenDifferentProposedIntentAndFreshProgress(t *testing.T) {
	history := []Observation{
		observation("old_run:unknown", "crm_create", "write", "unknown"),
		{ID: "new_plan", Outcome: "output", OutputDigest: digest("different plan"), ProgressDigest: digest("new")},
	}
	proposed := &Intent{ToolID: "other", Effect: "read", ArgumentsJSON: `{}`}
	for _, next := range []*Intent{nil, proposed} {
		got := checkDecision(t, history, next)
		if !got.Stop || !hasSignal(got, "unknown_effect") {
			t.Fatalf("unknown automatically resumed: %+v", got)
		}
	}
	// Caller resolves the existing durable execution rather than appending an
	// extra logical action. A proved rejection has no unknown hold in this view.
	history[0].Outcome = "rejected"
	if got := checkDecision(t, history, proposed); got.Stop {
		t.Fatalf("resolved uncertainty remained sticky: %+v", got)
	}
}

func TestRepeatedSuccessfulEffectsDetectedInHistory(t *testing.T) {
	history := []Observation{
		observation("first", "one", "write", "succeeded"),
		observation("intervening", "two", "read", "succeeded"),
		observation("duplicate", "one", "write", "succeeded"),
	}
	got := checkDecision(t, history, nil)
	if !hasSignal(got, "repeated_effect_intent") || !got.Stop {
		t.Fatalf("duplicate completed effect lost: %+v", got)
	}
}

func TestPendingProposalDoesNotClaimAnEffectOccurred(t *testing.T) {
	intent := Intent{ToolID: "crm", Effect: "write", ArgumentsJSON: `{"id":1}`}
	fingerprint, err := IntentFingerprint(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	history := []Observation{{ID: "pending_proposal", IntentFingerprint: fingerprint, Effect: "write", Outcome: "proposed"}}
	got := checkDecision(t, history, &intent)
	if hasSignal(got, "repeated_effect_intent") {
		t.Fatal("pending proposal was falsely represented as a completed business effect")
	}
	// The coordinator must separately reject duplicate proposals in a single
	// automatic batch before preparation/dispatch. This pure guard must not
	// manufacture a successful outcome to justify that distinct batch rule.
	if got.Stop {
		t.Fatalf("two unexecuted proposals incorrectly claimed to meet default loop threshold: %+v", got)
	}
}

func TestReadRepeatsBoundedWhilePaginationContinues(t *testing.T) {
	history := []Observation{observation("one", "page1", "read", "succeeded"), observation("two", "page1", "read", "succeeded")}
	if got := checkDecision(t, history, nil); got.Stop {
		t.Fatalf("two read polls stopped too early: %+v", got)
	}
	history = append(history, observation("three", "page1", "read", "succeeded"))
	if got := checkDecision(t, history, nil); !got.Stop || !hasSignal(got, "repeated_intent") || hasSignal(got, "repeated_effect_intent") {
		t.Fatalf("read loop not bounded: %+v", got)
	}
	pages := []Observation{}
	for i := 0; i < 32; i++ {
		pages = append(pages, observation(fmt.Sprintf("page%d", i), fmt.Sprintf("cursor%d", i), "read", "succeeded"))
	}
	if got := checkDecision(t, pages, nil); got.Stop {
		t.Fatalf("normal pagination stopped: %+v", got)
	}
}

func TestPendingThirdReadBlockedBeforeDispatch(t *testing.T) {
	proposed := Intent{ToolID: "search", Effect: "read", ArgumentsJSON: `{"q":"same"}`}
	fp, err := IntentFingerprint(context.Background(), proposed)
	if err != nil {
		t.Fatal(err)
	}
	history := []Observation{
		{ID: "call_1", IntentFingerprint: fp, Effect: "read", Outcome: "succeeded"},
		{ID: "call_2", IntentFingerprint: fp, Effect: "read", Outcome: "succeeded"},
	}
	got := checkDecision(t, history, &proposed)
	if !got.Stop || !hasSignal(got, "repeated_intent") {
		t.Fatalf("third identical read not stopped: %+v", got)
	}
}

func TestAlternatingCycleAndNewErrors(t *testing.T) {
	for _, outcomes := range [][]string{{"proposed", "proposed", "proposed", "proposed"}, {"succeeded", "succeeded", "succeeded", "succeeded"}, {"rejected", "rejected", "rejected", "rejected"}} {
		history := []Observation{}
		for i, identity := range []string{"A", "B", "A", "B"} {
			history = append(history, observation(fmt.Sprintf("step%d", i), identity, "read", outcomes[i]))
		}
		got := checkDecision(t, history, nil)
		if !got.Stop || !hasSignal(got, "alternating_intents") {
			t.Fatalf("ABAB cycle continued: %+v", got)
		}
		// A genuinely new request/error does not match that alternating suffix.
		history[3].IntentFingerprint = digest("fixed new error")
		if got := checkDecision(t, history, nil); got.Stop {
			t.Fatalf("new error correction stopped: %+v", got)
		}
	}
}

func TestFailedOutcomeSignalAndChangedFailureEvidence(t *testing.T) {
	history := []Observation{observation("attempt1", "same", "read", "rejected"), observation("attempt2", "same", "read", "rejected")}
	history[0].OutputDigest = digest("same failure")
	history[1].OutputDigest = digest("same failure")
	if got := checkDecision(t, history, nil); !got.Stop || !hasSignal(got, "repeated_failed_outcome") {
		t.Fatalf("same failure continued: %+v", got)
	}
	history[1].OutputDigest = digest("new error")
	if got := checkDecision(t, history, nil); got.Stop {
		t.Fatalf("new error falsely treated as same failure: %+v", got)
	}
	// Pure repeated failed status remains a conservative signal when a tool
	// provides no richer result digest; it is not proof of a semantic loop.
	history[0].OutputDigest = ""
	history[1].OutputDigest = ""
	if got := checkDecision(t, history, nil); !hasSignal(got, "repeated_failed_outcome") {
		t.Fatal("missing bounded failure fallback")
	}
}

func TestIdenticalUnverifiedOutputDoesNotProveTruth(t *testing.T) {
	history := []Observation{}
	for i := 0; i < 3; i++ {
		history = append(history, Observation{ID: fmt.Sprintf("output%d", i), Outcome: "output", OutputDigest: digest("I am finished"), UnverifiedOutput: true})
	}
	got := checkDecision(t, history, nil)
	if !got.Stop || !hasSignal(got, "identical_unverified_output") {
		t.Fatalf("unverified repetition continued: %+v", got)
	}
	// A changed statement alone is not a verification claim, but the identical
	// output detector must not pretend to semantically classify different text.
	history[2].OutputDigest = digest("I am finished with different words")
	if got := checkDecision(t, history, nil); got.Stop {
		t.Fatalf("detector claims semantic understanding: %+v", got)
	}
	// Independent evidence breaks the explicitly unverified output suffix.
	history[2].OutputDigest = history[1].OutputDigest
	history[2].UnverifiedOutput = false
	if got := checkDecision(t, history, nil); got.Stop {
		t.Fatalf("independent evidence ignored: %+v", got)
	}
}

func TestIndependentProgressResetsOnlySoftSignals(t *testing.T) {
	history := []Observation{observation("one", "same", "read", "succeeded"), observation("two", "same", "read", "succeeded"), observation("three", "same", "read", "succeeded")}
	history[0].ProgressDigest = digest("baseline")
	history[1].ProgressDigest = digest("baseline")
	history[2].ProgressDigest = digest("baseline")
	if got := checkDecision(t, history, nil); !got.Stop {
		t.Fatal("unchanged evidence incorrectly counted as progress")
	}
	history[2].ProgressDigest = digest("new independently tested state")
	if got := checkDecision(t, history, nil); got.Stop {
		t.Fatalf("independent progress did not reset soft suffix: %+v", got)
	}
	for i := range history {
		history[i].Effect = "write"
	}
	if got := checkDecision(t, history, nil); !got.Stop || !hasSignal(got, "repeated_effect_intent") {
		t.Fatal("progress reset hard side-effect guard")
	}
}

func TestBoundedHistoryPoliciesAndMalformedEvidenceFailClosed(t *testing.T) {
	policy := DefaultPolicy()
	if _, err := Evaluate(context.Background(), policy, make([]Observation, MaxHistory+1), nil); !errors.Is(err, ErrComplexity) {
		t.Fatalf("history overflow: %v", err)
	}
	for _, bad := range []Policy{{}, {RepeatedIntentLimit: 17, RepeatedFailureLimit: 2, RepeatedOutputLimit: 3, AlternationRepeats: 2}, {RepeatedIntentLimit: 3, RepeatedFailureLimit: 1, RepeatedOutputLimit: 3, AlternationRepeats: 2}} {
		if _, err := Evaluate(context.Background(), bad, nil, nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("unbounded/invalid policy: %v", err)
		}
	}
	valid := observation("one", "A", "read", "succeeded")
	badCases := []Observation{
		{}, {ID: "x", Outcome: "output"}, {ID: "x", Outcome: "output", OutputDigest: strings.Repeat("A", 64)},
		{ID: " x", IntentFingerprint: digest("A"), Effect: "read", Outcome: "succeeded"},
		{ID: "x", IntentFingerprint: digest("A"), Effect: "read", Outcome: "claimed_succeeded"},
		{ID: "x", IntentFingerprint: digest("A"), Effect: "write", Outcome: "succeeded", UnverifiedOutput: true, OutputDigest: digest("x")},
	}
	for i, bad := range badCases {
		if _, err := Evaluate(context.Background(), policy, []Observation{bad}, nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad evidence %d: %v", i, err)
		}
	}
	if _, err := Evaluate(context.Background(), policy, []Observation{valid, valid}, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate evidence ID: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Evaluate(ctx, policy, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestDecisionDeterministicAndDoesNotMutateHistory(t *testing.T) {
	history := []Observation{observation("one", "A", "write", "succeeded"), observation("two", "B", "read", "unknown"), observation("three", "A", "write", "succeeded")}
	before := append([]Observation(nil), history...)
	one := checkDecision(t, history, nil)
	for i := 0; i < 100; i++ {
		if got := checkDecision(t, history, nil); !reflect.DeepEqual(got, one) {
			t.Fatal("decision ordering depends on map order")
		}
	}
	if !reflect.DeepEqual(history, before) {
		t.Fatal("guard mutated caller history")
	}
	for _, signal := range one.Signals {
		for _, id := range signal.EvidenceIDs {
			if id == "" {
				t.Fatal("unattributable evidence")
			}
		}
	}
}
