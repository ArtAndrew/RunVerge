// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"reflect"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type recoveryTeamResult struct {
	source                      domain.RunSource
	run, operation, kind, basis string
}

// This reader deliberately has no current task/run/status or authority predicate.
// A historical selection is meaningful after a retry, cancellation or edit.
func recoveryReadTeamResult(ctx context.Context, tx pgx.Tx, schema, org, id string) (recoveryTeamResult, error) {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	var r recoveryTeamResult
	err := tx.QueryRow(ctx, `SELECT c.task_id,c.id,c.artifact_id,c.artifact_version,c.content_digest,a.content,c.run_id,c.operation_id,c.source,COALESCE(c.review_basis_candidate_id,'')
 FROM `+q("task_candidates")+` c JOIN `+q("artifacts")+` a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.version=c.artifact_version AND a.task_id=c.task_id
 WHERE c.organization_id=$1 AND c.id=$2`, org, id).Scan(&r.source.TaskID, &r.source.CandidateID, &r.source.ArtifactID, &r.source.ArtifactVersion, &r.source.ContentDigest, &r.source.Content, &r.run, &r.operation, &r.kind, &r.basis)
	if err != nil || len(r.source.Content) > 262144 || Hash([]byte(r.source.Content)) != r.source.ContentDigest || r.kind == "model_tools" {
		return r, recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	return r, nil
}

func recoveryTeamBasis(ctx context.Context, tx pgx.Tx, schema, org string, result recoveryTeamResult) (recoveryTeamResult, error) {
	if result.kind != "material" {
		return result, nil
	}
	basis, err := recoveryReadTeamResult(ctx, tx, schema, org, result.basis)
	if err != nil || basis.kind == "material" || basis.source.TaskID != result.source.TaskID || basis.run != result.run || basis.operation != result.operation {
		return basis, recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	return basis, nil
}

// Called only after the staged plan/member rows and their foreign keys pass.
// It reconstructs inputs and manifests from retained run-specific evidence;
// it never executes a coordinator, grants authority or contacts a provider.
func validateRecoveryTeamResults(ctx context.Context, tx pgx.Tx, schema string, executions []recoveryTeamExecution) error {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	var validBindings bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM `+q("task_candidates")+` c WHERE c.source='team' AND NOT EXISTS(SELECT 1 FROM `+q("team_executions")+` e WHERE e.organization_id=c.organization_id AND e.task_id=c.task_id AND e.run_id=c.run_id))`).Scan(&validBindings); err != nil || !validBindings {
		return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	for _, e := range executions {
		var node compiledTeamNode
		if decodeRecoveryHistoryJSON(e.raw, &node, 16<<20) != nil || node.Definition == nil {
			return ErrRecoveryTeamHistory
		}
		var parentInput, parentGoal, parentPrincipal string
		if tx.QueryRow(ctx, `SELECT r.input_context,t.goal,r.principal_id FROM `+q("runs")+` r JOIN `+q("tasks")+` t ON t.organization_id=r.organization_id AND t.id=r.task_id WHERE r.organization_id=$1 AND r.id=$2 AND r.task_id=$3`, e.org, e.run, e.task).Scan(&parentInput, &parentGoal, &parentPrincipal) != nil {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
		originalGoal := parentGoal
		if parentInput != "" {
			var previous domain.RunInput
			if decodeRecoveryHistoryJSON([]byte(parentInput), &previous, domain.MaxRunInputBytes) != nil {
				return ErrRecoveryTeamHistory
			}
			originalGoal = previous.OriginalGoal
		}
		rows, err := tx.Query(ctx, `SELECT member_id,child_task_id,COALESCE(child_run_id,''),status,COALESCE(selected_candidate_id,''),COALESCE(selected_artifact_id,''),COALESCE(selected_artifact_version,0),COALESCE(selected_content_digest,'') FROM `+q("team_members")+` WHERE organization_id=$1 AND parent_run_id=$2 ORDER BY ordinal`, e.org, e.run)
		if err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
		var slots []teamSlot
		for rows.Next() {
			var slot teamSlot
			var selected domain.RunSource
			if rows.Scan(&slot.ID, &slot.ChildTaskID, &slot.ChildRunID, &slot.Status, &selected.CandidateID, &selected.ArtifactID, &selected.ArtifactVersion, &selected.ContentDigest) != nil {
				rows.Close()
				return ErrRecoveryTeamHistory
			}
			if selected.CandidateID != "" {
				selected.TaskID = slot.ChildTaskID
				slot.Source = &selected
			}
			slots = append(slots, slot)
			if len(slots) > 32 {
				rows.Close()
				return ErrRecoveryTeamLimit
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(slots) != len(node.Members) {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
		manifest := teamManifest{Version: 1, Pattern: node.Definition.Pattern, JoinPolicy: node.Definition.JoinPolicy, Complete: true, AllFixture: teamAllFixture(node), Verification: "awaiting_authorized_decision", Members: []teamManifestMember{}}
		for i := range slots {
			slot := &slots[i]
			if (slot.Status == "candidate") != (slot.Source != nil) {
				return ErrRecoveryTeamHistory
			}
			if slot.Source != nil {
				exact, err := recoveryReadTeamResult(ctx, tx, schema, e.org, slot.Source.CandidateID)
				if err != nil || exact.run != slot.ChildRunID || exact.source.TaskID != slot.ChildTaskID || exact.source.ArtifactID != slot.Source.ArtifactID || exact.source.ArtifactVersion != slot.Source.ArtifactVersion || exact.source.ContentDigest != slot.Source.ContentDigest {
					return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
				}
				slot.Source = &exact.source
			}
			if slot.ChildRunID != "" {
				var childGoal, actualInput, actualDigest, actualSource, principal, feedback string
				var attempt int
				if tx.QueryRow(ctx, `SELECT t.goal,r.input_context,r.input_digest,COALESCE(r.source_candidate_id,''),r.principal_id,r.feedback,r.attempt FROM `+q("runs")+` r JOIN `+q("tasks")+` t ON t.organization_id=r.organization_id AND t.id=r.task_id WHERE r.organization_id=$1 AND r.id=$2 AND r.task_id=$3`, e.org, slot.ChildRunID, slot.ChildTaskID).Scan(&childGoal, &actualInput, &actualDigest, &actualSource, &principal, &feedback, &attempt) != nil {
					return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
				}
				input := domain.RunInput{OriginalGoal: originalGoal, LocalGoal: childGoal, Sources: []domain.RunSource{}}
				if parentInput != "" {
					input.ParentContext = json.RawMessage(parentInput)
				}
				sourceID := ""
				if node.Definition.Pattern != "parallel" && i > 0 {
					previous := slots[i-1]
					if previous.Source == nil {
						return ErrRecoveryTeamHistory
					}
					input.Sources = []domain.RunSource{*previous.Source}
					sourceID = previous.Source.CandidateID
				}
				body, digest, err := BuildRunInput(input)
				if err != nil || body != actualInput || digest != actualDigest || sourceID != actualSource || principal != parentPrincipal || feedback != "" || attempt != 1 {
					return ErrRecoveryTeamHistory
				}
			}
			if slot.Status != "candidate" {
				manifest.Complete = false
			}
			manifest.Members = append(manifest.Members, teamManifestMember{ID: slot.ID, TaskID: slot.ChildTaskID, RunID: slot.ChildRunID, Status: slot.Status, Result: slot.Source})
		}
		// A cancelled/blocked execution may retain a previously emitted manifest.
		// Compare the frozen members, never current task statuses or latest runs.
		rows, err = tx.Query(ctx, `SELECT a.content FROM `+q("task_candidates")+` c JOIN `+q("artifacts")+` a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.version=c.artifact_version WHERE c.organization_id=$1 AND c.task_id=$2 AND c.run_id=$3 AND c.source='team'`, e.org, e.task, e.run)
		if err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
		count := 0
		valid := true
		ready := 0
		for _, member := range manifest.Members {
			if member.Status == "candidate" {
				ready++
			}
		}
		for rows.Next() {
			var raw string
			var stored teamManifest
			count++
			if rows.Scan(&raw) != nil || count > 1 || ready == 0 || (manifest.JoinPolicy == "all" && !manifest.Complete) || decodeRecoveryHistoryJSON([]byte(raw), &stored, 262144) != nil || !reflect.DeepEqual(stored, manifest) {
				valid = false
				break
			}
			for _, m := range stored.Members {
				if m.Status != "candidate" && m.Status != "failed" && m.Status != "cancelled" && m.Status != "skipped" {
					valid = false
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if !valid || err != nil || (e.status == "candidate" && count != 1) {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
	}
	return recoveryCheckInheritedSelection(ctx, tx, schema)
}

func recoveryCheckInheritedSelection(ctx context.Context, tx pgx.Tx, schema string) error {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	rows, err := tx.Query(ctx, `SELECT d.organization_id,d.task_id,d.run_id,d.candidate_id,d.artifact_id,d.artifact_version,d.content_digest,parent.task_id,parent.run_id,parent.candidate_id FROM `+q("task_decisions")+` d JOIN `+q("task_decisions")+` parent ON parent.organization_id=d.organization_id AND parent.id=d.parent_decision_id WHERE d.method='inherited_authorized_user_decision'`)
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	type inherited struct {
		org, task, run, candidate, artifact, digest, parentTask, parentRun, parentCandidate string
		version                                                                             int
	}
	var decisions []inherited
	for rows.Next() {
		var d inherited
		if rows.Scan(&d.org, &d.task, &d.run, &d.candidate, &d.artifact, &d.version, &d.digest, &d.parentTask, &d.parentRun, &d.parentCandidate) != nil {
			rows.Close()
			return ErrRecoveryTeamHistory
		}
		decisions = append(decisions, d)
		if len(decisions) > 200000 {
			rows.Close()
			return ErrRecoveryTeamLimit
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	work := 0
	for _, d := range decisions {
		parent, err := recoveryReadTeamResult(ctx, tx, schema, d.org, d.parentCandidate)
		if err != nil || parent.source.TaskID != d.parentTask || parent.run != d.parentRun {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
		visited := map[string]bool{}
		found := false
		var walk func(recoveryTeamResult, int) error
		walk = func(result recoveryTeamResult, depth int) error {
			work++
			if work > 1000000 {
				return ErrRecoveryTeamLimit
			}
			if depth > domain.MaxTaskDepth || len(visited) >= domain.MaxTreeTasks || visited[result.source.TaskID] {
				return ErrRecoveryTeamHistory
			}
			visited[result.source.TaskID] = true
			basis, err := recoveryTeamBasis(ctx, tx, schema, d.org, result)
			if err != nil {
				return err
			}
			if basis.kind != "team" {
				if depth == 0 {
					return ErrRecoveryTeamHistory
				}
				return nil
			}
			var manifest teamManifest
			if decodeRecoveryHistoryJSON([]byte(basis.source.Content), &manifest, 262144) != nil {
				return ErrRecoveryTeamHistory
			}
			for _, member := range manifest.Members {
				if member.Result == nil {
					continue
				}
				selected, err := recoveryReadTeamResult(ctx, tx, schema, d.org, member.Result.CandidateID)
				if err != nil || !reflect.DeepEqual(selected.source, *member.Result) || selected.run != member.RunID {
					return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
				}
				if selected.source.TaskID == d.task && selected.run == d.run && selected.source.CandidateID == d.candidate && selected.source.ArtifactID == d.artifact && selected.source.ArtifactVersion == d.version && selected.source.ContentDigest == d.digest {
					found = true
				}
				if err = walk(selected, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if err = walk(parent, 0); err != nil {
			return err
		}
		if !found {
			return ErrRecoveryTeamHistory
		}
	}
	return nil
}
