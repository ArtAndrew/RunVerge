// SPDX-License-Identifier: Elastic-2.0
package modelruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestHistoricalIntentSnapshotAuthorWireCompatibility(t *testing.T) {
	s := IntentSnapshot{Version: "model-attempt-v1", OrganizationID: "org", TaskID: "task", RunID: "run", OperationID: "op", AgentID: "agent", TaskOwnerID: "owner", AgentVersion: 1, Goal: "<verify>", Instructions: "instructions", Endpoint: "https://model.example/v1", TokenLimitField: "max_tokens", MaxInputBytes: 123, MaxOutputBytes: 456, MaxOutputTokens: 7, RequestTimeoutNanos: 8000000, Price: PriceProfile{ProviderAccount: "p", Version: "v", Model: "m", BoundAssumptions: "b", MaxChargeMicroUSD: 9, MaxInputTokens: 10, FixedMicroUSD: 1}}
	// Literal pre-extraction v1 representation. This detects changed tags,
	// field order, HTML escaping and omission independently of the new encoder.
	wire := `{"Version":"model-attempt-v1","OrganizationID":"org","TaskID":"task","RunID":"run","OperationID":"op","AgentID":"agent","TaskOwnerID":"owner","AgentVersion":1,"Goal":"\u003cverify\u003e","Instructions":"instructions","Endpoint":"https://model.example/v1","TokenLimitField":"max_tokens","MaxInputBytes":123,"MaxOutputBytes":456,"MaxOutputTokens":7,"RequestTimeoutNanos":8000000,"Price":{"ProviderAccount":"p","Version":"v","Model":"m","BoundAssumptions":"b","MaxChargeMicroUSD":9,"MaxInputTokens":10,"InputMicroUSDPerMillion":0,"OutputMicroUSDPerMillion":0,"FixedMicroUSD":1}}`
	assert := func(wire string) {
		t.Helper()
		want := sha256.Sum256([]byte(wire))
		got, err := HistoricalIntentDigest(s)
		if err != nil || got != hex.EncodeToString(want[:]) {
			t.Fatal("historical v1 digest changed", err)
		}
	}
	assert(wire)
	s.Price.AccountingMode = "metered_api"
	assert(strings.Replace(wire, `"Price":{`, `"Price":{"accounting_mode":"metered_api",`, 1))
	s.Version = "unknown-v2"
	if _, err := HistoricalIntentDigest(s); err == nil {
		t.Fatal("unknown intent protocol accepted")
	}
}
