// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"context"
	"github.com/jackc/pgx/v5"
)

func bindPreparedModelProposalTx(ctx context.Context, tx pgx.Tx, org string, candidate domain.TaskCandidate, in domain.PrepareToolExecution, arguments string, connection domain.ToolConnection) error {
	if candidate.Source != "model_tools" {
		if in.ModelProposalID != "" {
			return domain.Err("conflict", "Запрос инструмента относится к другому исходному результату", 409)
		}
		return nil
	}
	if !validText(in.ModelProposalID, 128) || in.OccurrenceID != "model:"+in.ModelProposalID {
		return domain.Err("invalid_request", "Нужен точный запрос модели и его постоянный идентификатор действия", 400)
	}
	var matches bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_tool_proposals p WHERE p.organization_id=$1 AND p.id=$2 AND p.candidate_id=$3 AND p.task_id=$4 AND p.run_id=$5 AND p.operation_id=$6
 AND p.tool_id=$7 AND p.tool_version=$8 AND p.tool_profile_digest=$9 AND p.arguments_json=$10 AND p.arguments_digest=$11)`, org, in.ModelProposalID, candidate.ID, candidate.TaskID, candidate.RunID, candidate.OperationID, in.ToolID, in.ToolVersion, domain.ToolConnectionDigest(connection), arguments, Hash([]byte(arguments))).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return domain.Err("conflict", "Аргументы или инструмент отличаются от сохранённого запроса модели", 409)
	}
	return nil
}
