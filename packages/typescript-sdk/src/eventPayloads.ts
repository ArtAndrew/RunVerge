// SPDX-License-Identifier: Apache-2.0
// Generated from contracts/event-payloads.json; run tests/sdk/generate_event_payloads.py.
export const EVENT_PAYLOAD_REGISTRY = {
  "api_version": "0.14.0",
  "registry_version": 1,
  "license": "Apache-2.0",
  "types": {
    "action.dispatched": {
      "fields": {
        "action_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "dispatch_not_after": {
          "kind": "timestamp",
          "required": true
        },
        "fencing_token": {
          "kind": "decimal",
          "required": true,
          "minimum": 1,
          "maximum_decimal": "9223372036854775807"
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "automation.completed": {
      "fields": {
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "grant_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "signal_count": {
          "kind": "integer",
          "required": true,
          "maximum": 2147483647
        },
        "snapshot_digest": {
          "kind": "digest",
          "required": true
        },
        "task_acceptance_changed": {
          "kind": "boolean",
          "required": true,
          "const": false
        }
      }
    },
    "automation.expired": {
      "fields": {
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "grant_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "signal_count": {
          "kind": "integer",
          "required": true,
          "maximum": 2147483647
        },
        "snapshot_digest": {
          "kind": "digest",
          "required": true
        },
        "task_acceptance_changed": {
          "kind": "boolean",
          "required": true,
          "const": false
        }
      }
    },
    "automation.granted": {
      "fields": {
        "acceptance_automated": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "expires_at": {
          "kind": "timestamp",
          "required": true
        },
        "grant_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "independent_of_login_session": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "max_charge_microusd": {
          "kind": "decimal",
          "required": true,
          "minimum": 1,
          "maximum_decimal": "1000000000000"
        },
        "max_model_turns": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "max_tool_executions": {
          "kind": "integer",
          "required": true,
          "maximum": 2147483647
        },
        "mode": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "automation.revoked": {
      "fields": {
        "grant_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "past_effects_preserved": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        }
      }
    },
    "automation.stopped": {
      "fields": {
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "grant_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "signal_count": {
          "kind": "integer",
          "required": true,
          "maximum": 2147483647
        },
        "snapshot_digest": {
          "kind": "digest",
          "required": true
        },
        "task_acceptance_changed": {
          "kind": "boolean",
          "required": true,
          "const": false
        }
      }
    },
    "cost.adjustment": {
      "fields": {
        "actual_microusd": {
          "kind": "decimal",
          "required": true,
          "maximum_decimal": "9223372036854775807"
        },
        "incident": {
          "kind": "boolean",
          "required": true
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "cost.fixture_reconciled": {
      "fields": {
        "actual_microusd": {
          "kind": "decimal",
          "required": true,
          "maximum_decimal": "9223372036854775807"
        },
        "basis": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "fixture_per_logical_operation"
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "synthetic_cost": {
          "kind": "boolean",
          "required": true,
          "const": true
        }
      }
    },
    "cost.quarantine_resolution": {
      "fields": {
        "actual_microusd": {
          "kind": "decimal",
          "required": true,
          "maximum_decimal": "9223372036854775807"
        },
        "incident": {
          "kind": "boolean",
          "required": true
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "cost.quarantined": {
      "fields": {
        "actual_microusd": {
          "kind": "decimal",
          "required": true,
          "maximum_decimal": "9223372036854775807"
        },
        "evidence_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "quarantine_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "projection_overflow"
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "cost.released": {
      "fields": {
        "basis": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "not_dispatched"
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "cost.reserved": {
      "fields": {
        "amount_microusd": {
          "kind": "decimal",
          "required": true,
          "maximum_decimal": "9223372036854775807"
        },
        "category": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "price_version": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "cost.settle": {
      "fields": {
        "actual_microusd": {
          "kind": "decimal",
          "required": true,
          "maximum_decimal": "9223372036854775807"
        },
        "incident": {
          "kind": "boolean",
          "required": true
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "cost.settled": {
      "fields": {
        "actual_microusd": {
          "kind": "decimal",
          "required": true,
          "maximum_decimal": "9223372036854775807"
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "synthetic_cost": {
          "kind": "boolean",
          "required": true,
          "const": true
        }
      }
    },
    "cost.unknown": {
      "fields": {
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "material.created": {
      "fields": {
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "costs_unchanged": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "draft_only": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "material_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647,
          "const": 1
        }
      }
    },
    "material.published": {
      "fields": {
        "artifact_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "artifact_version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647,
          "const": 1
        },
        "candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "costs_unchanged": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "material_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "material_version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "new_run_created": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "parent_candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128,
          "allow_empty": true
        },
        "published_by": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "review_basis_candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128,
          "allow_empty": true
        }
      }
    },
    "material.revised": {
      "fields": {
        "base_version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "costs_unchanged": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "draft_only": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "material_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "merge_base_version": {
          "kind": "integer",
          "required": false,
          "minimum": 1,
          "maximum": 2147483647
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        }
      }
    },
    "model.candidate_created": {
      "fields": {
        "artifact_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "cost_status": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "operation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "proposal_count": {
          "kind": "integer",
          "required": true,
          "maximum": 2147483647
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "source": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "verified": {
          "kind": "boolean",
          "required": true,
          "const": false
        }
      }
    },
    "model.continuation_queued": {
      "fields": {
        "acknowledged_new_cost": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "messages_digest": {
          "kind": "digest",
          "required": true
        },
        "model_turn": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "operation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "prior_liabilities_preserved": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "source_candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128,
          "allow_empty": true
        },
        "source_operation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "operation.claimed": {
      "fields": {
        "adapter": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "enum": [
            "fixture",
            "model"
          ]
        },
        "attempt": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "fencing_token": {
          "kind": "decimal",
          "required": true,
          "minimum": 1,
          "maximum_decimal": "9223372036854775807"
        },
        "model_connection_id": {
          "kind": "string",
          "required": false,
          "max_bytes": 128
        },
        "model_connection_version": {
          "kind": "integer",
          "required": false,
          "minimum": 1,
          "maximum": 2147483647
        },
        "operation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "synthetic_cost": {
          "kind": "boolean",
          "required": false,
          "const": true
        }
      },
      "variants": [
        {
          "field": "adapter",
          "value": "fixture",
          "required": [
            "synthetic_cost"
          ],
          "forbidden": [
            "model_connection_id",
            "model_connection_version"
          ]
        },
        {
          "field": "adapter",
          "value": "model",
          "required": [
            "model_connection_id",
            "model_connection_version"
          ],
          "forbidden": [
            "synthetic_cost"
          ]
        }
      ]
    },
    "task.blocked": {
      "fields": {
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        }
      }
    },
    "task.cancelled": {
      "fields": {
        "liabilities_preserved": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "allow_empty": true
        }
      }
    },
    "task.created": {
      "fields": {
        "agent_version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "task.decision_inherited": {
      "fields": {
        "candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "decision": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "enum": [
            "accept",
            "reject",
            "request_changes"
          ]
        },
        "decision_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "method": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "enum": [
            "inherited_authorized_user_decision"
          ]
        },
        "parent_decision_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "task.delegated": {
      "fields": {
        "agent_version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "member_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "parent_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "root_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "state": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "dependency_pending"
        }
      }
    },
    "task.new_attempt": {
      "fields": {
        "acknowledged_new_cost": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "attempt": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "input_digest": {
          "kind": "digest",
          "required": true
        },
        "operation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "prior_liabilities_preserved": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "task.result_decided": {
      "fields": {
        "candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "costs_unchanged": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "decision": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "enum": [
            "accept",
            "reject",
            "request_changes"
          ]
        },
        "decision_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "method": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "enum": [
            "authorized_user_decision"
          ]
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "task.succeeded": {
      "fields": {
        "adapter": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "fixture:echo"
        },
        "verification": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "exact_goal_echo"
        }
      }
    },
    "team.blocked": {
      "fields": {
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "team.candidate_created": {
      "fields": {
        "all_fixture": {
          "kind": "boolean",
          "required": true
        },
        "artifact_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "complete": {
          "kind": "boolean",
          "required": true
        },
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "synthetic_cost": {
          "kind": "boolean",
          "required": true
        },
        "verified": {
          "kind": "boolean",
          "required": true,
          "const": false
        }
      }
    },
    "team.initialized": {
      "fields": {
        "all_fixture": {
          "kind": "boolean",
          "required": true
        },
        "join_policy": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "enum": [
            "all",
            "allow_partial"
          ]
        },
        "pattern": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "enum": [
            "sequence",
            "parallel",
            "delegate_verify"
          ]
        },
        "plan_digest": {
          "kind": "digest",
          "required": true
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "team.member_candidate": {
      "fields": {
        "acceptance": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "not_implied"
        },
        "candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "child_task_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "member_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "team.member_started": {
      "fields": {
        "input_digest": {
          "kind": "digest",
          "required": true
        },
        "member_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "operation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "parent_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "parent_run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "run_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "source_candidate_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128,
          "allow_empty": true
        }
      }
    },
    "tool.approval_decided": {
      "fields": {
        "approval_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "decision": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "method": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "tool.cancelled": {
      "fields": {
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "liabilities_preserved": {
          "kind": "boolean",
          "required": true
        },
        "outcome": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        }
      }
    },
    "tool.claimed": {
      "fields": {
        "attempt": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647
        },
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "fencing_token": {
          "kind": "decimal",
          "required": true,
          "minimum": 1,
          "maximum_decimal": "9223372036854775807"
        }
      }
    },
    "tool.cost_released": {
      "fields": {
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "proof": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "pending_action_never_dispatched"
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "tool.dispatch_queued": {
      "fields": {
        "cost_acknowledged": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "intent_hash": {
          "kind": "digest",
          "required": true
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "tool.dispatched": {
      "fields": {
        "action_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "dispatch_not_after": {
          "kind": "timestamp",
          "required": true
        },
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "fencing_token": {
          "kind": "decimal",
          "required": true,
          "minimum": 1,
          "maximum_decimal": "9223372036854775807"
        },
        "intent_hash": {
          "kind": "digest",
          "required": true
        }
      }
    },
    "tool.effect_observed": {
      "fields": {
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "financial_settlement": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "method": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "registered_tool_response"
        },
        "outcome": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "receipt_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "tool.operator_reconciled": {
      "fields": {
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "financial_settlement": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "method": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096,
          "const": "operator_assertion"
        },
        "outcome": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "principal_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "receipt_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "tool.outcome_conflict_overflow": {
      "fields": {
        "additional_payload_retained": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "receipt_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "requires_operator_review": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "retained_conflicts": {
          "kind": "integer",
          "required": true,
          "maximum": 2147483647,
          "const": 8
        }
      }
    },
    "tool.outcome_conflicted": {
      "fields": {
        "artifact_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "artifact_version": {
          "kind": "integer",
          "required": true,
          "minimum": 1,
          "maximum": 2147483647,
          "const": 1
        },
        "conflict_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "content_digest": {
          "kind": "digest",
          "required": true
        },
        "effect_reference": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "evidence_digest": {
          "kind": "digest",
          "required": true
        },
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "existing_receipt_unchanged": {
          "kind": "boolean",
          "required": true,
          "const": true
        },
        "financial_settlement": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "observed_outcome": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        },
        "receipt_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "tool.outcome_unknown": {
      "fields": {
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "financial_settlement": {
          "kind": "boolean",
          "required": false,
          "const": false
        },
        "reason": {
          "kind": "string",
          "required": false,
          "max_bytes": 4096
        },
        "resend_allowed": {
          "kind": "boolean",
          "required": true,
          "const": false
        }
      }
    },
    "tool.prepared": {
      "fields": {
        "action_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "approval_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "intent_hash": {
          "kind": "digest",
          "required": true
        },
        "reservation_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        }
      }
    },
    "tool.stopped_unsent": {
      "fields": {
        "execution_id": {
          "kind": "string",
          "required": true,
          "max_bytes": 128
        },
        "external_call": {
          "kind": "boolean",
          "required": true,
          "const": false
        },
        "reason": {
          "kind": "string",
          "required": true,
          "max_bytes": 4096
        }
      }
    }
  }
} as const;

export interface EventPayloadMap {
  "action.dispatched": { "action_id": string; "dispatch_not_after": string; "fencing_token": string; "reservation_id": string };
  "automation.completed": { "external_call": false; "grant_id": string; "reason": string; "signal_count": number; "snapshot_digest": string; "task_acceptance_changed": false };
  "automation.expired": { "external_call": false; "grant_id": string; "reason": string; "signal_count": number; "snapshot_digest": string; "task_acceptance_changed": false };
  "automation.granted": { "acceptance_automated": false; "expires_at": string; "grant_id": string; "independent_of_login_session": true; "max_charge_microusd": string; "max_model_turns": number; "max_tool_executions": number; "mode": string; "principal_id": string };
  "automation.revoked": { "grant_id": string; "past_effects_preserved": true; "principal_id": string; "reason": string };
  "automation.stopped": { "external_call": false; "grant_id": string; "reason": string; "signal_count": number; "snapshot_digest": string; "task_acceptance_changed": false };
  "cost.adjustment": { "actual_microusd": string; "incident": boolean; "reservation_id": string };
  "cost.fixture_reconciled": { "actual_microusd": string; "basis": "fixture_per_logical_operation"; "reservation_id": string; "synthetic_cost": true };
  "cost.quarantine_resolution": { "actual_microusd": string; "incident": boolean; "reservation_id": string };
  "cost.quarantined": { "actual_microusd": string; "evidence_id": string; "quarantine_id": string; "reason": "projection_overflow"; "reservation_id": string };
  "cost.released": { "basis": "not_dispatched"; "reservation_id": string };
  "cost.reserved": { "amount_microusd": string; "category": string; "price_version": string; "reservation_id": string };
  "cost.settle": { "actual_microusd": string; "incident": boolean; "reservation_id": string };
  "cost.settled": { "actual_microusd": string; "reservation_id": string; "synthetic_cost": true };
  "cost.unknown": { "reason": string; "reservation_id": string };
  "material.created": { "content_digest": string; "costs_unchanged": true; "draft_only": true; "material_id": string; "principal_id": string; "version": 1 };
  "material.published": { "artifact_id": string; "artifact_version": 1; "candidate_id": string; "content_digest": string; "costs_unchanged": true; "material_id": string; "material_version": number; "new_run_created": false; "parent_candidate_id": string; "published_by": string; "review_basis_candidate_id": string };
  "material.revised": { "base_version": number; "content_digest": string; "costs_unchanged": true; "draft_only": true; "material_id": string; "merge_base_version"?: number; "principal_id": string; "version": number };
  "model.candidate_created": { "artifact_id": string; "content_digest": string; "cost_status": string; "operation_id": string; "proposal_count": number; "reservation_id": string; "source": string; "verified": false };
  "model.continuation_queued": { "acknowledged_new_cost": true; "messages_digest": string; "model_turn": number; "operation_id": string; "prior_liabilities_preserved": true; "reason": string; "run_id": string; "source_candidate_id": string; "source_operation_id": string };
  "operation.claimed": { "adapter": "fixture" | "model"; "attempt": number; "fencing_token": string; "model_connection_id"?: string; "model_connection_version"?: number; "operation_id": string; "synthetic_cost"?: true } & ({ "adapter": "fixture"; "synthetic_cost": string; "model_connection_id"?: never; "model_connection_version"?: never } | { "adapter": "model"; "model_connection_id": string; "model_connection_version": string; "synthetic_cost"?: never });
  "task.blocked": { "reason": string };
  "task.cancelled": { "liabilities_preserved": true; "reason": string };
  "task.created": { "agent_version": number; "run_id": string };
  "task.decision_inherited": { "candidate_id": string; "decision": "accept" | "reject" | "request_changes"; "decision_id": string; "method": "inherited_authorized_user_decision"; "parent_decision_id": string };
  "task.delegated": { "agent_version": number; "member_id": string; "parent_id": string; "root_id": string; "state": "dependency_pending" };
  "task.new_attempt": { "acknowledged_new_cost": true; "attempt": number; "input_digest": string; "operation_id": string; "prior_liabilities_preserved": true; "reason": string; "run_id": string };
  "task.result_decided": { "candidate_id": string; "content_digest": string; "costs_unchanged": true; "decision": "accept" | "reject" | "request_changes"; "decision_id": string; "method": "authorized_user_decision"; "principal_id": string; "run_id": string };
  "task.succeeded": { "adapter": "fixture:echo"; "verification": "exact_goal_echo" };
  "team.blocked": { "reason": string; "run_id": string };
  "team.candidate_created": { "all_fixture": boolean; "artifact_id": string; "candidate_id": string; "complete": boolean; "content_digest": string; "run_id": string; "synthetic_cost": boolean; "verified": false };
  "team.initialized": { "all_fixture": boolean; "join_policy": "all" | "allow_partial"; "pattern": "sequence" | "parallel" | "delegate_verify"; "plan_digest": string; "run_id": string };
  "team.member_candidate": { "acceptance": "not_implied"; "candidate_id": string; "child_task_id": string; "content_digest": string; "member_id": string; "run_id": string };
  "team.member_started": { "input_digest": string; "member_id": string; "operation_id": string; "parent_id": string; "parent_run_id": string; "run_id": string; "source_candidate_id": string };
  "tool.approval_decided": { "approval_id": string; "decision": string; "execution_id": string; "external_call": false; "method": string; "principal_id": string };
  "tool.cancelled": { "execution_id": string; "liabilities_preserved": boolean; "outcome": string };
  "tool.claimed": { "attempt": number; "execution_id": string; "fencing_token": string };
  "tool.cost_released": { "execution_id": string; "proof": "pending_action_never_dispatched"; "reservation_id": string };
  "tool.dispatch_queued": { "cost_acknowledged": true; "execution_id": string; "external_call": false; "intent_hash": string; "principal_id": string };
  "tool.dispatched": { "action_id": string; "dispatch_not_after": string; "execution_id": string; "fencing_token": string; "intent_hash": string };
  "tool.effect_observed": { "execution_id": string; "financial_settlement": false; "method": "registered_tool_response"; "outcome": string; "receipt_id": string };
  "tool.operator_reconciled": { "execution_id": string; "external_call": false; "financial_settlement": false; "method": "operator_assertion"; "outcome": string; "principal_id": string; "receipt_id": string };
  "tool.outcome_conflict_overflow": { "additional_payload_retained": false; "execution_id": string; "receipt_id": string; "requires_operator_review": true; "retained_conflicts": 8 };
  "tool.outcome_conflicted": { "artifact_id": string; "artifact_version": 1; "conflict_id": string; "content_digest": string; "effect_reference": string; "evidence_digest": string; "execution_id": string; "existing_receipt_unchanged": true; "financial_settlement": false; "observed_outcome": string; "receipt_id": string };
  "tool.outcome_unknown": { "execution_id": string; "financial_settlement"?: false; "reason"?: string; "resend_allowed": false };
  "tool.prepared": { "action_id": string; "approval_id": string; "execution_id": string; "external_call": false; "intent_hash": string; "reservation_id": string };
  "tool.stopped_unsent": { "execution_id": string; "external_call": false; "reason": string };
}
