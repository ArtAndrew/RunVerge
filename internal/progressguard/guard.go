// SPDX-License-Identifier: Elastic-2.0
package progressguard

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxHistory = 512

type Intent struct {
	ToolID        string
	Effect        string // read, write, destructive; not a claim of success.
	ArgumentsJSON string
}

// Observation is a caller-authorized projection of one logical proposal,
// execution or model output, in causal order. An execution appears once with
// its current durable outcome; do not pass every lifecycle event as a new
// logical action. Stable IDs must not change on Retry or grant renewal.
//
// ProgressDigest is optional independently checked, cumulative progress state.
// A newly generated ID, model assertion, plan, elapsed time or merely repeated
// receipt is not new progress. Changed progress only resets soft suffix checks;
// it never permits an unknown effect or a repeated successful business intent.
type Observation struct {
	ID                string
	IntentFingerprint string
	Effect            string
	Outcome           string // proposed, succeeded, rejected, unknown, output.
	OutputDigest      string
	ProgressDigest    string
	UnverifiedOutput  bool
}

type Policy struct {
	RepeatedIntentLimit  int
	RepeatedFailureLimit int
	RepeatedOutputLimit  int
	AlternationRepeats   int
}

func DefaultPolicy() Policy {
	return Policy{
		RepeatedIntentLimit:  3,
		RepeatedFailureLimit: 2,
		RepeatedOutputLimit:  3,
		AlternationRepeats:   2,
	}
}

type Signal struct {
	Code        string
	Fingerprint string
	Count       int
	EvidenceIDs []string
}

type Decision struct {
	Stop                      bool
	ProposedIntentFingerprint string
	Signals                   []Signal
}

// Evaluate returns deterministic reasons to stop automatic work for human
// review. Signals indicate repetition or uncertainty, not truth or a proven
// loop. It never changes a goal, retries an action, escalates a model, clears a
// budget, or creates evidence. The caller must persist the selected decision
// append-only in the same transaction that denies continuation/dispatch.
//
// The complete task/root history must be supplied, including prior runs and
// retries. Overflow is an error; truncating old unknowns or repeated effects to
// make a request fit would invalidate the guard. Error also means no automatic
// dispatch. Proposed is optional: it checks an intent before a new effect.
func Evaluate(ctx context.Context, policy Policy, history []Observation, proposed *Intent) (Decision, error) {
	decision := Decision{Signals: []Signal{}}
	if err := ctx.Err(); err != nil {
		return decision, err
	}
	if !validPolicy(policy) {
		return decision, ErrInvalidInput
	}
	if len(history) > MaxHistory {
		return decision, ErrComplexity
	}
	seenIDs := make(map[string]bool, len(history))
	for _, observation := range history {
		if err := ctx.Err(); err != nil {
			return decision, err
		}
		if !validObservation(observation) || seenIDs[observation.ID] {
			return decision, ErrInvalidInput
		}
		seenIDs[observation.ID] = true
	}
	if proposed != nil {
		fingerprint, err := IntentFingerprint(ctx, *proposed)
		if err != nil {
			return decision, err
		}
		decision.ProposedIntentFingerprint = fingerprint
	}
	add := func(signal Signal) {
		decision.Signals = append(decision.Signals, signal)
		decision.Stop = true
	}

	// Uncertainty is not evidence of failure. In particular, a later grant,
	// new model plan or different proposed call does not authorize a resend.
	unknown := Signal{Code: "unknown_effect"}
	for _, observation := range history {
		if observation.Outcome == "unknown" {
			unknown.Count++
			unknown.EvidenceIDs = append(unknown.EvidenceIDs, observation.ID)
		}
	}
	if unknown.Count > 0 {
		add(unknown)
	}

	// This check deliberately spans progress epochs, runs and profile versions.
	// Stable tool identity and normalized arguments bind the business intent.
	seenEffects := map[string][]string{}
	for _, observation := range history {
		if observation.Outcome == "succeeded" && observation.Effect != "read" {
			seenEffects[observation.IntentFingerprint] = append(seenEffects[observation.IntentFingerprint], observation.ID)
		}
	}
	if prior := seenEffects[decision.ProposedIntentFingerprint]; proposed != nil && len(prior) > 0 {
		add(Signal{Code: "repeated_effect_intent", Fingerprint: decision.ProposedIntentFingerprint, Count: len(prior) + 1, EvidenceIDs: cloneIDs(prior)})
	} else {
		// Deterministically report the earliest already duplicated effect when
		// checking a completed batch before another model continuation.
		for _, observation := range history {
			prior := seenEffects[observation.IntentFingerprint]
			if len(prior) > 1 {
				add(Signal{Code: "repeated_effect_intent", Fingerprint: observation.IntentFingerprint, Count: len(prior), EvidenceIDs: cloneIDs(prior)})
				break
			}
		}
	}

	// Soft detectors inspect a suffix since the most recent independent
	// progress change. A fresh ID or changed text alone does not reset it.
	start := 0
	lastProgress := ""
	for index, observation := range history {
		if observation.ProgressDigest != "" && observation.ProgressDigest != lastProgress {
			start = index
			lastProgress = observation.ProgressDigest
		}
	}
	intents := make([]Observation, 0, len(history)-start+1)
	outputs := make([]Observation, 0, len(history)-start)
	for _, observation := range history[start:] {
		if observation.IntentFingerprint != "" {
			intents = append(intents, observation)
		}
		if observation.Outcome == "output" {
			outputs = append(outputs, observation)
		}
	}
	if proposed != nil {
		intents = append(intents, Observation{IntentFingerprint: decision.ProposedIntentFingerprint, Effect: proposed.Effect, Outcome: "proposed"})
	}
	if len(intents) > 0 {
		last := intents[len(intents)-1]
		matched := 0
		for index := len(intents) - 1; index >= 0 && intents[index].IntentFingerprint == last.IntentFingerprint; index-- {
			matched++
		}
		if matched >= policy.RepeatedIntentLimit {
			add(suffixSignal("repeated_intent", last.IntentFingerprint, intents[len(intents)-matched:]))
		}
		// Output/failure digest distinguishes newly rejected errors. A model
		// repeating the same failed request with only a new call ID does not.
		if last.Outcome == "rejected" {
			matched = 0
			for index := len(intents) - 1; index >= 0; index-- {
				item := intents[index]
				if item.Outcome != "rejected" || item.IntentFingerprint != last.IntentFingerprint || item.OutputDigest != last.OutputDigest {
					break
				}
				matched++
			}
			if matched >= policy.RepeatedFailureLimit {
				add(suffixSignal("repeated_failed_outcome", last.IntentFingerprint, intents[len(intents)-matched:]))
			}
		}
		needed := policy.AlternationRepeats * 2
		if len(intents) >= needed {
			window := intents[len(intents)-needed:]
			a, b := window[0].IntentFingerprint, window[1].IntentFingerprint
			alternates := a != b
			for index, item := range window {
				want := a
				if index%2 == 1 {
					want = b
				}
				alternates = alternates && item.IntentFingerprint == want
			}
			if alternates {
				add(suffixSignal("alternating_intents", "", window))
			}
		}
	}
	if len(outputs) > 0 {
		last := outputs[len(outputs)-1]
		matched := 0
		for index := len(outputs) - 1; index >= 0; index-- {
			item := outputs[index]
			if !item.UnverifiedOutput || item.OutputDigest != last.OutputDigest {
				break
			}
			matched++
		}
		if matched >= policy.RepeatedOutputLimit {
			add(suffixSignal("identical_unverified_output", last.OutputDigest, outputs[len(outputs)-matched:]))
		}
	}
	return decision, nil
}

func suffixSignal(code, fingerprint string, observations []Observation) Signal {
	signal := Signal{Code: code, Fingerprint: fingerprint, Count: len(observations), EvidenceIDs: []string{}}
	for _, item := range observations {
		if item.ID != "" { // The proposed intent has not yet become an observation.
			signal.EvidenceIDs = append(signal.EvidenceIDs, item.ID)
		}
	}
	return signal
}

func cloneIDs(ids []string) []string { return append([]string{}, ids...) }

func validPolicy(policy Policy) bool {
	for _, limit := range []int{policy.RepeatedIntentLimit, policy.RepeatedFailureLimit, policy.RepeatedOutputLimit, policy.AlternationRepeats} {
		if limit < 2 || limit > 16 {
			return false
		}
	}
	return true
}

func validObservation(item Observation) bool {
	if !validID(item.ID) || !validOptionalDigest(item.OutputDigest) || !validOptionalDigest(item.ProgressDigest) {
		return false
	}
	if item.UnverifiedOutput && item.OutputDigest == "" {
		return false
	}
	if item.Outcome == "output" {
		return item.IntentFingerprint == "" && item.Effect == "" && item.OutputDigest != ""
	}
	if !validDigest(item.IntentFingerprint) || !validEffect(item.Effect) {
		return false
	}
	switch item.Outcome {
	case "proposed", "succeeded", "rejected", "unknown":
		return !item.UnverifiedOutput
	default:
		return false
	}
}

func validEffect(effect string) bool {
	return effect == "read" || effect == "write" || effect == "destructive"
}

func validOptionalDigest(value string) bool { return value == "" || validDigest(value) }

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
