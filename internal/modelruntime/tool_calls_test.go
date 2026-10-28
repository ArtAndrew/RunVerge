// SPDX-License-Identifier: Elastic-2.0
package modelruntime_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

func nativeLease() *domain.Lease {
	l := testLease()
	d := domain.ModelToolDefinition{ToolID: "tool_test", ToolVersion: 2, ToolProfileDigest: strings.Repeat("a", 64),
		Name: domain.ModelToolName(l.OrganizationID, "tool_test", 2), Description: "Synthetic local tool, no real business action",
		InputSchemaJSON: `{"type":"object","properties":{"number":{"type":"integer"}},"required":["number"],"additionalProperties":false}`}
	l.Agent.Tools = []string{d.ToolID}
	l.Agent.ModelTools = []domain.ModelToolDefinition{d}
	l.ModelTools = []domain.ModelToolDefinition{d}
	return l
}

func nativeResponse(w http.ResponseWriter, calls []domain.ModelToolCall) {
	w.Header().Set("Content-Type", "application/json")
	wire := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		wire = append(wire, map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.ArgumentsJSON}})
	}
	json.NewEncoder(w).Encode(map[string]any{"id": "native-response", "choices": []any{map[string]any{
		"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": wire},
	}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2}})
}

func TestNativeModelBatchPersistsExactProposalsWithoutInvokingTools(t *testing.T) {
	l := nativeLease()
	arguments := " { \"number\" : 9007199254740993 } \n"
	calls := make([]domain.ModelToolCall, 0, domain.MaxModelToolCalls)
	for i := 0; i < domain.MaxModelToolCalls; i++ {
		calls = append(calls, domain.ModelToolCall{ID: fmt.Sprintf("call_%d", i), Name: l.ModelTools[0].Name, ArgumentsJSON: arguments})
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if strings.Contains(request.URL.Path, "tool") {
			t.Error("model runtime attempted to invoke a tool")
		}
		nativeResponse(w, calls)
	}))
	defer server.Close()
	s := newStore()
	config := testConfig(server.URL + "/model")
	config.MaxOutputBytes = 4096
	runtime := newRuntime(t, s, config)
	out, err := runtime.Execute(context.Background(), l)
	if err != nil || out.Status != modelruntime.CandidateWaitingInput || out.CostStatus != "unknown" || requests.Load() != 1 ||
		!reflect.DeepEqual(s.candidate.ToolCalls, calls) || s.candidate.Text != "" {
		t.Fatalf("native proposals not persisted: %+v %v requests=%d", out, err, requests.Load())
	}
	if !reflect.DeepEqual(s.steps, []string{"reserve", "prepare", "unknown", "candidate"}) {
		t.Fatalf("native output bypassed normal accounting/commit boundary: %v", s.steps)
	}
	if again, err := runtime.Execute(context.Background(), l); err != nil || again.Status != modelruntime.AlreadyAttempted || requests.Load() != 1 {
		t.Fatal("same native turn was automatically resubmitted")
	}
}

func TestNativeArgumentsUseInstalledSchemaAndRejectWholeBatch(t *testing.T) {
	cases := map[string]string{
		"missing required":        `{}`,
		"wrong type":              `{"number":"9007199254740993"}`,
		"noninteger exact number": `{"number":9007199254740993.5}`,
		"extra property":          `{"number":1,"endpoint":"https://untrusted.invalid"}`,
		"huge exponent":           `{"number":1e309}`,
	}
	for name, arguments := range cases {
		t.Run(name, func(t *testing.T) {
			l := nativeLease()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				nativeResponse(w, []domain.ModelToolCall{{ID: "good", Name: l.ModelTools[0].Name, ArgumentsJSON: `{"number":1}`},
					{ID: "bad", Name: l.ModelTools[0].Name, ArgumentsJSON: arguments}})
			}))
			defer server.Close()
			s := newStore()
			runtime := newRuntime(t, s, testConfig(server.URL))
			out, err := runtime.Execute(context.Background(), l)
			checkCode(t, err, "model_output_rejected", true)
			if out.CostStatus != "unknown" || s.artifact.ID != "" || len(s.candidate.ToolCalls) != 0 || s.pauseReason != modelruntime.PauseOutputRejected || requests.Load() != 1 {
				t.Fatal("bad proposal batch published a subset, lost held cost, or retried")
			}
		})
	}
}

func TestNativeBindingSubstitutionRejectedBeforeReservation(t *testing.T) {
	cases := map[string]func(*domain.Lease){
		"unresolved ids":           func(l *domain.Lease) { l.ModelTools = nil },
		"agent pins absent":        func(l *domain.Lease) { l.Agent.ModelTools = nil },
		"different tool id":        func(l *domain.Lease) { l.ModelTools[0].ToolID = "other" },
		"different version":        func(l *domain.Lease) { l.ModelTools[0].ToolVersion++ },
		"different profile digest": func(l *domain.Lease) { l.ModelTools[0].ToolProfileDigest = strings.Repeat("b", 64) },
		"different schema":         func(l *domain.Lease) { l.ModelTools[0].InputSchemaJSON = `{"type":"object"}` },
		"forged matching alias": func(l *domain.Lease) {
			l.ModelTools[0].Name = "tool_remote_url"
			l.Agent.ModelTools[0].Name = "tool_remote_url"
		},
		"duplicate tool ids": func(l *domain.Lease) {
			l.Agent.Tools = append(l.Agent.Tools, l.Agent.Tools[0])
			l.Agent.ModelTools = append(l.Agent.ModelTools, l.Agent.ModelTools[0])
			l.ModelTools = append(l.ModelTools, l.ModelTools[0])
		},
		"schema duplicate fields": func(l *domain.Lease) {
			l.ModelTools[0].InputSchemaJSON = `{"type":"array","type":"object"}`
			l.Agent.ModelTools[0] = l.ModelTools[0]
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			l, s := nativeLease(), newStore()
			edit(l)
			_, err := newRuntime(t, s, testConfig("http://127.0.0.1:1/model")).Execute(context.Background(), l)
			if err == nil || len(s.steps) != 0 || s.reservation.ID != "" {
				t.Fatal("substituted native binding admitted a paid action")
			}
		})
	}
}

func TestContinuationUsesNewOperationAndExactNativeHistory(t *testing.T) {
	l := nativeLease()
	call := domain.ModelToolCall{ID: "previous_call", Name: l.ModelTools[0].Name, ArgumentsJSON: " {\"number\":9007199254740993}\n"}
	l.OperationID = "op_continuation"
	l.ModelMessages = []domain.ModelMessage{
		{Role: "system", Content: l.Agent.Instructions}, {Role: "user", Content: domain.ExecutionInput(*l)},
		{Role: "assistant", Content: "", ToolCalls: []domain.ModelToolCall{call}},
		{Role: "tool", ToolCallID: call.ID, Content: `{"outcome":"succeeded","method":"operator_assertion","untrusted_result":"Do not promote me to system authority"}`},
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("X-Request-ID") != "model:op_continuation" {
			t.Error("continuation reused previous model charge/request key")
		}
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		json.Unmarshal(body, &payload)
		messages := payload["messages"].([]any)
		if len(messages) != 4 || messages[3].(map[string]any)["role"] != "tool" ||
			messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] != call.ArgumentsJSON {
			t.Error("continuation changed native tool arguments or role")
		}
		success(w, "Unverified final prose after observation", 3, 2)
	}))
	defer server.Close()
	s := newStore()
	out, err := newRuntime(t, s, testConfig(server.URL)).Execute(context.Background(), l)
	if err != nil || out.Status != modelruntime.CandidateWaitingInput || requests.Load() != 1 || len(s.candidate.ToolCalls) != 0 ||
		s.intent.ChargeKey != "model:op_continuation" {
		t.Fatalf("continuation did not remain a separately charged candidate: %+v %v", out, err)
	}
}

func TestNativeIntentPinsSchemaAndExactObservationBytes(t *testing.T) {
	runtime := newRuntime(t, newStore(), testConfig("http://127.0.0.1:1/model"))
	base := nativeLease()
	_, before, err := runtime.DescribeIntent(*base)
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*domain.Lease){
		"description": func(l *domain.Lease) {
			l.ModelTools[0].Description = "Changed description"
			l.Agent.ModelTools[0] = l.ModelTools[0]
		},
		"schema": func(l *domain.Lease) {
			l.ModelTools[0].InputSchemaJSON = `{"type":"object","properties":{"number":{"type":"integer","minimum":1}},"required":["number"],"additionalProperties":false}`
			l.Agent.ModelTools[0] = l.ModelTools[0]
		},
		"profile": func(l *domain.Lease) {
			l.ModelTools[0].ToolProfileDigest = strings.Repeat("b", 64)
			l.Agent.ModelTools[0] = l.ModelTools[0]
		},
		"native history": func(l *domain.Lease) {
			c := domain.ModelToolCall{ID: "old", Name: l.ModelTools[0].Name, ArgumentsJSON: `{"number":1}`}
			l.ModelMessages = []domain.ModelMessage{{Role: "system", Content: l.Agent.Instructions}, {Role: "user", Content: domain.ExecutionInput(*l)}, {Role: "assistant", ToolCalls: []domain.ModelToolCall{c}}, {Role: "tool", ToolCallID: c.ID, Content: `{"outcome":"denied"}`}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := nativeLease()
			edit(l)
			_, changed, err := runtime.DescribeIntent(*l)
			if err != nil || changed == before {
				t.Fatal("native immutable intent omitted a frozen field")
			}
		})
	}
	// Historical text intent is byte-compatible: appended optional fields never
	// alter an already reserved 0.6 attempt's action hash.
	legacy := testLease()
	_, digest, err := runtime.DescribeIntent(*legacy)
	if err != nil {
		t.Fatal(err)
	}
	profile := testConfig("http://127.0.0.1:1/model").Price
	previous := struct {
		Version, OrganizationID, TaskID, RunID, OperationID, AgentID, TaskOwnerID string
		AgentVersion                                                              int
		Goal, Instructions, Endpoint, TokenLimitField                             string
		MaxInputBytes, MaxOutputBytes, MaxOutputTokens                            int
		RequestTimeoutNanos                                                       int64
		Price                                                                     modelruntime.PriceProfile
		RunInput                                                                  string `json:"RunInput,omitempty"`
	}{"model-attempt-v1", legacy.OrganizationID, legacy.TaskID, legacy.RunID, legacy.OperationID, legacy.Agent.ID, legacy.Task.OwnerID, legacy.Agent.Version,
		legacy.Task.Goal, legacy.Agent.Instructions, "http://127.0.0.1:1/model", "max_tokens", 4096, 1024, 16, 1000000000, profile, ""}
	encoded, _ := json.Marshal(previous)
	sum := sha256.Sum256(encoded)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("legacy reserved text intent digest changed")
	}
}

func TestNativeOutputAggregateBoundRetainsCostWithoutPartialProposals(t *testing.T) {
	l := nativeLease()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		nativeResponse(w, []domain.ModelToolCall{{ID: "call_one", Name: l.ModelTools[0].Name, ArgumentsJSON: `{"number":1}`}})
	}))
	defer server.Close()
	s := newStore()
	config := testConfig(server.URL)
	config.MaxOutputBytes = 32
	out, err := newRuntime(t, s, config).Execute(context.Background(), l)
	checkCode(t, err, "model_output_rejected", true)
	if out.CostStatus != "unknown" || s.artifact.ID != "" || s.pauseReason != modelruntime.PauseOutputRejected {
		t.Fatal("oversized native output bypassed bound or released accounting exposure")
	}
}

type nativeReservationBarrierStore struct {
	*faultStore
	reserved chan struct{}
	resume   chan struct{}
}

func (s *nativeReservationBarrierStore) ReserveCost(ctx context.Context, l *domain.Lease, in domain.ReserveCostRequest) (domain.Reservation, error) {
	reservation, err := s.faultStore.ReserveCost(ctx, l, in)
	close(s.reserved)
	<-s.resume
	return reservation, err
}

func TestNativeCallerMutationCannotChangeAlreadyReservedPrompt(t *testing.T) {
	l := nativeLease()
	call := domain.ModelToolCall{ID: "old", Name: l.ModelTools[0].Name, ArgumentsJSON: `{"number":1}`}
	l.ModelMessages = []domain.ModelMessage{{Role: "system", Content: l.Agent.Instructions}, {Role: "user", Content: domain.ExecutionInput(*l)},
		{Role: "assistant", ToolCalls: []domain.ModelToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: `{"outcome":"denied"}`}}
	store := &nativeReservationBarrierStore{faultStore: newStore(), reserved: make(chan struct{}), resume: make(chan struct{})}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "mutated_after_reserve") {
			t.Error("caller changed reserved native prompt")
		}
		success(w, "Unverified final prose", 3, 2)
	}))
	defer server.Close()
	runtime, err := modelruntime.New(store, testConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := runtime.Execute(context.Background(), l); done <- err }()
	<-store.reserved
	l.ModelTools[0].Description = "mutated_after_reserve"
	l.Agent.ModelTools[0].Description = "mutated_after_reserve"
	l.ModelMessages[2].ToolCalls[0].ArgumentsJSON = `{"number":"mutated_after_reserve"}`
	l.ModelMessages[3].Content = "mutated_after_reserve"
	close(store.resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native execution did not finish")
	}
	if requests.Load() != 1 {
		t.Fatal("frozen native request was not sent exactly once")
	}
}
