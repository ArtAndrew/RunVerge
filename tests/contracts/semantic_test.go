// SPDX-License-Identifier: Elastic-2.0
package contracts_test

import (
	"encoding/json"
	"os"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

// The same values pass structural JSON Schema validation but must fail the
// canonical domain validator. A schema pass never authorizes a team to run.
func TestTeamSemanticFixtures(t *testing.T) {
	data, err := os.ReadFile("fixtures/semantic-examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Examples []struct {
			Name          string                `json:"name"`
			Schema        string                `json:"schema"`
			Value         domain.TeamDefinition `json:"value"`
			SemanticValid bool                  `json:"semantic_valid"`
		} `json:"examples"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Examples) < 2 {
		t.Fatal("semantic fixtures missing")
	}
	for _, example := range document.Examples {
		if example.Schema != "TeamDefinition" {
			continue // Review/retry context is enforced by storage, not ValidateTeam.
		}
		t.Run(example.Name, func(t *testing.T) {
			if got := domain.ValidateTeam(example.Value) == nil; got != example.SemanticValid {
				t.Fatal("domain semantic outcome disagrees with independent fixture")
			}
		})
	}
}
