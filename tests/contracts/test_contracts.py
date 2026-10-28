# SPDX-License-Identifier: Elastic-2.0
from __future__ import annotations

from copy import deepcopy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import tomllib
import unittest

from support import HERE, ROOT, Draft202012Validator, dto_validator, read_json, references, resolve_pointer, response_validator

VERSION = "0.14.0"
MAX_MONEY = 1_000_000_000_000
PUBLIC_ROUTES = {
    ("/capabilities", "get"),
	("/configuration/catalog", "get"), ("/configuration/destinations", "get"), ("/configuration/resources", "get"),
	("/configuration/grants", "get"), ("/configuration/grants/{grant_id}", "get"), ("/configuration/grants", "post"), ("/configuration/grants/{grant_id}/revoke", "post"),
	("/configuration/plans", "get"), ("/configuration/plans/{plan_id}", "get"), ("/configuration/plans", "post"),
	("/configuration/plans/{plan_id}/validate", "post"), ("/configuration/plans/{plan_id}/approve", "post"), ("/configuration/plans/{plan_id}/apply", "post"), ("/configuration/plans/{plan_id}/verify", "post"),
	("/configuration/verifications", "get"), ("/configuration/verifications/{verification_id}", "get"), ("/configuration/audit", "get"),
    ("/compatibility", "get"), ("/tasks/{id}/event-snapshot", "get"),
    ("/tasks/{id}/event-feed", "get"), ("/tasks/{id}/event-stream", "get"),
    ("/login-policy", "get"), ("/login-policy/update", "post"),
    ("/service-accounts", "get"), ("/service-accounts", "post"), ("/service-accounts/{principal_id}", "get"),
    ("/service-accounts/{principal_id}/update", "post"), ("/service-accounts/{principal_id}/keys", "get"),
    ("/service-accounts/{principal_id}/keys/{key_id}", "get"), ("/service-accounts/{principal_id}/keys/{key_id}/revoke", "post"),
    ("/security-audit", "get"),
    ("/members/capabilities", "get"), ("/members", "get"), ("/members/{principal_id}", "get"), ("/members", "post"),
    ("/members/{principal_id}/update", "post"), ("/members/{principal_id}/revoke-credentials", "post"), ("/membership-audit", "get"),
    ("/tasks/{id}/automation", "get"), ("/tasks/{id}/automation", "post"),
    ("/tasks/{id}/automation/revoke", "post"), ("/tasks/{id}/progress-signals", "get"),
    ("/tasks/{id}/model-tool-proposals", "get"), ("/tasks/{id}/model-tool-proposals/{proposal_id}", "get"), ("/tasks/{id}/continue-model", "post"),
    ("/tools", "get"), ("/tasks/{id}/tool-executions", "get"), ("/tasks/{id}/tool-executions", "post"),
    ("/tasks/{id}/tool-executions/{execution_id}", "get"),
    ("/tasks/{id}/tool-executions/{execution_id}/decide", "post"),
    ("/tasks/{id}/tool-executions/{execution_id}/dispatch", "post"),
    ("/tasks/{id}/tool-executions/{execution_id}/reconcile", "post"),
    ("/me", "get"),
    ("/models", "get"), ("/agents", "post"), ("/agents", "get"), ("/agents/{id}/publish", "post"),
    ("/tasks", "post"), ("/tasks", "get"), ("/tasks/{id}", "get"),
    ("/tasks/{id}/cancel", "post"), ("/tasks/{id}/events", "get"),
    ("/tasks/{id}/artifacts", "get"), ("/tasks/{id}/runs", "get"),
    ("/tasks/{id}/operations", "get"), ("/tasks/{id}/actions", "get"),
    ("/tasks/{id}/approvals", "get"), ("/tasks/{id}/costs", "get"),
    ("/tasks/{id}/review", "post"), ("/tasks/{id}/retry", "post"),
    ("/tasks/{id}/artifacts/{artifact_id}", "get"),
    ("/tasks/{id}/materials", "get"), ("/tasks/{id}/materials", "post"),
    ("/tasks/{id}/materials/{material_id}", "get"),
    ("/tasks/{id}/materials/{material_id}/revisions", "get"), ("/tasks/{id}/materials/{material_id}/revisions", "post"),
    ("/tasks/{id}/materials/{material_id}/revisions/{version}", "get"),
    ("/tasks/{id}/materials/{material_id}/publish", "post"),
    ("/tasks/{id}/candidates", "get"), ("/tasks/{id}/decisions", "get"), ("/tasks/{id}/children", "get"),
}
MUTATION_DTOS = {
	"ConfigurationDestinationRef", "CreateConfigurationGrant", "RevokeConfigurationGrant", "ConfigurationOperationInput", "CreateConfigurationPlan", "ValidateConfigurationPlan", "ApproveConfigurationPlan", "ApplyConfigurationPlan", "StartConfigurationVerification",
    "UpdateOrganizationLoginPolicy", "CreateServiceAccount", "UpdateServiceAccount", "RevokeServiceKey",
    "CreateOrganizationMember", "UpdateOrganizationMember", "RevokeMemberCredentials",
    "CreateTaskAutomation", "RevokeTaskAutomation", "AutomationToolPin",
    "ContinueModelRequest", "ModelToolResultBinding",
    "PrepareToolExecution", "DecideToolExecution", "DispatchToolExecution", "ReconcileToolExecution",
    "CreateAgent", "CreateTask", "PublishAgent", "EmptyCommand", "ApprovalDecision",
    "ReserveCostRequest", "CostEvidence", "CostAdjustment", "AgentDefinition", "TeamDefinition",
    "ReviewTaskRequest", "RetryTaskRequest", "CreateMaterial", "ReviseMaterial", "PublishMaterial",
}
STATE_DTOS = {"tool_execution": "ToolExecution", "task": "Task", "run": "Run", "operation": "Operation", "action": "Action", "approval": "Approval", "reservation": "Reservation"}


class ContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.domain = read_json(ROOT / "contracts/domain.schema.json")
        cls.openapi = read_json(ROOT / "contracts/openapi.json")
        cls.states = read_json(ROOT / "contracts/state-machines.json")
        cls.permissions = read_json(ROOT / "contracts/permissions.json")
        cls.errors = read_json(ROOT / "contracts/errors.json")
        cls.fixtures = read_json(HERE / "fixtures/valid-dtos.json")["examples"]

    def test_schema_documents_and_references_are_valid(self):
        self.assertEqual(self.domain["$schema"], "https://json-schema.org/draft/2020-12/schema")
        Draft202012Validator.check_schema(self.domain)
        for name, schema in self.openapi["components"]["schemas"].items():
            with self.subTest(schema=name):
                Draft202012Validator.check_schema(schema)
        for document in (self.domain, self.openapi):
            for reference in references(document):
                with self.subTest(reference=reference):
                    resolve_pointer(document, reference)
        self.assertEqual(self.domain["$defs"], self.openapi["components"]["schemas"])

    def test_revision_is_consistent(self):
        self.assertEqual(self.domain["$id"], f"urn:agent-platform:domain:{VERSION}")
        self.assertEqual(self.openapi["info"]["version"], VERSION)
        self.assertEqual(self.states["contract_version"], VERSION)
        self.assertEqual(self.permissions["contract_version"], VERSION)
        self.assertEqual(self.errors["contract_version"], VERSION)
        self.assertEqual(read_json(ROOT / "packages/typescript-sdk/package.json")["version"], VERSION)
        with (ROOT / "packages/python-sdk/pyproject.toml").open("rb") as source:
            self.assertEqual(tomllib.load(source)["project"]["version"], VERSION)
        self.assertIn("packages/typescript-sdk/src/types", (ROOT / "web/src/api.ts").read_text())

    def test_independent_good_examples_cover_every_dto(self):
        self.assertEqual(set(self.fixtures), set(self.domain["$defs"]))
        for name, value in self.fixtures.items():
            with self.subTest(dto=name):
                dto_validator(self.domain, name).validate(value)

    def test_versioned_bad_dto_examples(self):
        document = read_json(HERE / "fixtures/invalid-dtos.json")
        self.assertEqual(document["contract_version"], VERSION)
        for example in document["examples"]:
            with self.subTest(example=example["name"]):
                self.assertEqual(dto_validator(self.domain, example["schema"]).is_valid(example["value"]), example["valid"])

    def test_shared_event_conformance_covers_all_registered_kinds(self):
        corpus = read_json(ROOT / "contracts/event-conformance.json")
        registry = read_json(ROOT / "contracts/event-payloads.json")
        self.assertEqual(corpus["contract_version"], VERSION)
        self.assertEqual(registry["api_version"], VERSION)
        self.assertEqual(corpus["license"], "Apache-2.0")
        self.assertEqual(registry["license"], "Apache-2.0")
        self.assertEqual(len(registry["types"]), 47)
        covered = set()
        for case in corpus["cases"]:
            with self.subTest(case=case["name"]):
                self.assertEqual(dto_validator(self.domain,"Event").is_valid(case["event"]),case["valid"])
                if case["valid"] and case["event"]["payload_version"] == 1:
                    covered.add(case["event"]["type"])
        self.assertEqual(covered,set(registry["types"]))

    def test_required_fields_and_nulls(self):
        for name, schema in self.domain["$defs"].items():
            good = self.fixtures[name]
            for field in schema.get("required", []):
                for mode in ("missing", "null"):
                    with self.subTest(dto=name, field=field, mode=mode):
                        changed = deepcopy(good)
                        if mode == "missing":
                            del changed[field]
                        else:
                            changed[field] = None
                        self.assertFalse(dto_validator(self.domain, name).is_valid(changed))

    def test_unknown_mutation_fields_rejected_and_response_extensions_allowed(self):
        for name in MUTATION_DTOS:
            with self.subTest(dto=name):
                value = {**self.fixtures[name], "invented_capability": True}
                self.assertFalse(dto_validator(self.domain, name).is_valid(value))
        for name in ("Task", "Agent", "Run", "Operation", "CostReport"):
            with self.subTest(response=name):
                dto_validator(self.domain, name).validate({**self.fixtures[name], "future_extension": {}})

    def test_money_bounds_and_types(self):
        fields = {
            "CreateTask": ("budget_microusd", 1), "CreateAgent": ("budget_microusd", 1),
            "AgentDefinition": ("budget_microusd", 1), "ReserveCostRequest": ("amount_microusd", 0),
            "AvailableModel": ("max_charge_microusd", 1),
        }
        for name, (field, minimum) in fields.items():
            self.assertIn(name, self.domain["$defs"], f"Canonical schema is missing {name}")
            for value, valid in [(minimum, True), (MAX_MONEY, True), (minimum - 1, False), (MAX_MONEY + 1, False), (0.5, False), (True, False), ("1000", False)]:
                with self.subTest(dto=name, field=field, value=value):
                    self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: value}), valid)
        factual = {
            "Task": ["reserved_microusd", "spent_microusd"],
            "CostReport": ["reserved_microusd", "spent_microusd", "unknown_microusd"],
            "CostEvidence": ["actual_microusd"], "Reservation": ["actual_microusd"],
            "FinancialIncident": ["amount_microusd"], "CostQuarantine": ["actual_microusd"],
            "LedgerEntry": ["delta_reserved_microusd", "delta_spent_microusd"],
        }
        invalid = [0, 1000, 1.5, True, None, "", "-0", "+1", "01", "1.0", "1e6", " 1", "1 ", "0\n", "١", "9223372036854775808", "-9223372036854775809"]
        for name, fields in factual.items():
            for field in fields:
                schema = self.domain["$defs"][name]["properties"][field]
                self.assertEqual(schema["type"], "string")
                self.assertEqual(schema["x-integer-encoding"], "decimal-int64")
                values = [(v, False) for v in invalid]
                values += [(v, True) for v in ["0", "1", str(MAX_MONEY + 1), "9007199254740993", "9223372036854775807"]]
                values += [("-1", name == "LedgerEntry"), ("-9223372036854775808", name == "LedgerEntry")]
                for value, valid in values:
                    with self.subTest(dto=name, field=field, value=value):
                        self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: value}), valid)
        for value, valid in [("0", True), (str(MAX_MONEY), True), (str(MAX_MONEY + 1), False), (1000, False), ("-1", False)]:
            self.assertEqual(dto_validator(self.domain, "Reservation").is_valid({**self.fixtures["Reservation"], "amount_microusd": value}), valid)

    def test_decimal_range_patterns_cover_int64_without_float_rounding(self):
        import random
        rng = random.Random(301)
        maximum = (1 << 63) - 1
        samples = [0, maximum, maximum - 1, 1 << 53, (1 << 53) + 1]
        samples += [10**n + delta for n in range(1, 19) for delta in (-1, 0, 1)]
        samples += [rng.randrange(maximum + 1) for _ in range(100)]
        for number in samples:
            dto_validator(self.domain, "CostEvidence").validate({**self.fixtures["CostEvidence"], "actual_microusd": str(number)})
            if number:
                dto_validator(self.domain, "LedgerEntry").validate({**self.fixtures["LedgerEntry"], "delta_spent_microusd": str(-number)})
        for number in [maximum + i for i in range(1, 30)] + [10**19, 10**20]:
            self.assertFalse(dto_validator(self.domain, "CostEvidence").is_valid({**self.fixtures["CostEvidence"], "actual_microusd": str(number)}))

    def test_quarantine_shape_and_semantics_are_distinct(self):
        for field, invalid in [("status", "invented"), ("reason", "invented"), ("requested_kind", "refund"), ("base_revision", 0), ("revision", 0)]:
            self.assertFalse(dto_validator(self.domain, "CostQuarantine").is_valid({**self.fixtures["CostQuarantine"], field: invalid}))
        for field, invalid in [("quarantines", None), ("pending_quarantine_count", -1), ("pending_quarantine_count", True), ("incomplete", "false")]:
            self.assertFalse(dto_validator(self.domain, "CostReport").is_valid({**self.fixtures["CostReport"], field: invalid}))
        for status in ("settled", "quarantined"):
            dto_validator(self.domain, "AccountingOutcome").validate({**self.fixtures["AccountingOutcome"], "status": status})
        self.assertFalse(dto_validator(self.domain, "AccountingOutcome").is_valid({**self.fixtures["AccountingOutcome"], "status": "retry"}))
        # Contextual evidence/reservation/account binding and when incomplete is
        # true are storage invariants, not implied by this structural schema.

    def test_status_enums_and_forbidden_transitions(self):
        transitions = self.states["transitions"]
        self.assertEqual(set(transitions), set(STATE_DTOS))
        for kind, name in STATE_DTOS.items():
            states = transitions[kind]
            self.assertEqual(set(states), set(self.domain["$defs"][name]["properties"]["status"]["enum"]))
            for state, targets in states.items():
                self.assertEqual(len(targets), len(set(targets)))
                self.assertTrue(set(targets) <= set(states))
                dto_validator(self.domain, name).validate({**self.fixtures[name], "status": state})
            self.assertFalse(dto_validator(self.domain, name).is_valid({**self.fixtures[name], "status": "invented"}))
        forbidden = [("task", "succeeded", "queued"), ("task", "cancelled", "running"), ("run", "failed", "running"), ("operation", "succeeded", "leased"), ("action", "unknown", "sent"), ("reservation", "unknown", "reserved"), ("approval", "approved", "pending")]
        for kind, before, after in forbidden:
            with self.subTest(kind=kind, before=before, after=after):
                self.assertNotIn(after, transitions[kind][before])

    def test_structural_team_bounds_and_semantic_examples(self):
        for field, invalid in [("max_depth", 0), ("max_depth", 17), ("max_children", 0), ("max_children", 33), ("pattern", "invented"), ("join_policy", "any")]:
            with self.subTest(field=field, invalid=invalid):
                value = {**self.fixtures["TeamDefinition"], field: invalid}
                self.assertFalse(dto_validator(self.domain, "TeamDefinition").is_valid(value))
                self.assertFalse(dto_validator(self.domain, "AgentDefinition").is_valid({**self.fixtures["AgentDefinition"], "team": value}))
        for count in (0, 33):
            value = {**self.fixtures["TeamDefinition"], "members": [{"id": f"m{i}", "agent_id": "agent_fixture", "agent_version": 1} for i in range(count)]}
            self.assertFalse(dto_validator(self.domain, "TeamDefinition").is_valid(value))
        for example in read_json(HERE / "fixtures/semantic-examples.json")["examples"]:
            with self.subTest(semantic_case=example["name"]):
                self.assertEqual(dto_validator(self.domain, example["schema"]).is_valid(example["value"]), example["schema_valid"])
                self.assertFalse(example["semantic_valid"])

    def test_review_retry_and_additive_execution_metadata(self):
        for field, maximum in [("max_runs", 10), ("execution_timeout_seconds", 2592000)]:
            for value, valid in [(1, True), (maximum, True), (0, False), (maximum + 1, False), (True, False)]:
                self.assertEqual(dto_validator(self.domain, "CreateTask").is_valid({**self.fixtures["CreateTask"], field: value}), valid)
        for decision in ("accept", "reject", "request_changes"):
            value = {**self.fixtures["ReviewTaskRequest"], "decision": decision}
            dto_validator(self.domain, "ReviewTaskRequest").validate(value)
            value.pop("reason")
            self.assertEqual(dto_validator(self.domain, "ReviewTaskRequest").is_valid(value), decision == "accept")
        for source in ("fixture", "model", "team", "material"):
            dto_validator(self.domain, "TaskCandidate").validate({**self.fixtures["TaskCandidate"], "source": source})
        for field in ("expected_version", "artifact_version"):
            self.assertFalse(dto_validator(self.domain, "ReviewTaskRequest").is_valid({**self.fixtures["ReviewTaskRequest"], field: 0}))
        # Durable idempotency caches from 0.3 can lack new metadata. Fresh GET
        # supplies it; schema compatibility never authorizes guessing its value.
        cached = deepcopy(self.fixtures["Task"])
        for field in ("current_run_id", "candidate_id", "attempt_count", "max_runs", "execution_deadline", "accepted_decision_id"):
            cached.pop(field, None)
        dto_validator(self.domain, "Task").validate(cached)
        run = deepcopy(self.fixtures["Run"])
        run.pop("attempt")
        dto_validator(self.domain, "Run").validate(run)
        for name in ("TaskCandidate", "TaskDecision", "ReviewTaskRequest"):
            for invalid in ("a" * 63, "A" * 64, "a" * 64 + "\n"):
                self.assertFalse(dto_validator(self.domain, name).is_valid({**self.fixtures[name], "content_digest": invalid}))
        # Cross-task/run ownership, artifact bytes, stale versions, consumed
        # child decisions, root-only retry, budget/deadline checks are enforced
        # transactionally by storage, not by accepting a structurally valid DTO.

    def test_material_content_presence_versions_and_draft_publication_distinction(self):
        for name in ("CreateMaterial", "ReviseMaterial", "MaterialContent"):
            self.assertIn("content", self.domain["$defs"][name]["required"])
            for content, valid in [("", True), ("{unfinished", True), ("a" * 262144, True), ("a" * 262145, False), (None, False), (False, False)]:
                self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], "content": content}), valid)
        for name, field in [("Material", "head_version"), ("MaterialRevision", "version"), ("ReviseMaterial", "expected_version"), ("PublishMaterial", "expected_material_version")]:
            for value, valid in [(1, True), (128, True), (0, False), (129, False), (True, False), (1.5, False)]:
                self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: value}), valid)
        for name in ("CreateMaterial", "Material"):
            for media, valid in [("text/plain", True), ("text/markdown", True), ("application/json", True), ("text/html", False)]:
                self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], "media_type": media}), valid)
        for name, field in [("Material", "head_content_digest"), ("MaterialRevision", "content_digest"), ("PublishMaterial", "content_digest")]:
            for invalid in ("a" * 63, "A" * 64, "a" * 64 + "\n"):
                self.assertFalse(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: invalid}))
        self.assertFalse(dto_validator(self.domain, "MaterialRevision").is_valid({**self.fixtures["MaterialRevision"], "base_version": -1}))
        self.assertFalse(dto_validator(self.domain, "ReviseMaterial").is_valid({**self.fixtures["ReviseMaterial"], "reason": " "}))
        # Publication has no inline content. The server checks nonblank/valid
        # JSON stored bytes, current parent, digest, task/run state and closure.
        self.assertNotIn("content", self.domain["$defs"]["PublishMaterial"]["properties"])
        for name in ("TaskCandidate",):
            for field in ("parent_candidate_id", "review_basis_candidate_id", "material_id", "material_version", "published_by"):
                self.assertIn(field, self.domain["$defs"][name]["properties"])

    def test_cost_evidence_and_categories(self):
        evidence = self.fixtures["CostEvidence"]
        for field, invalid in [("final", False), ("currency", "EUR"), ("payload_digest", "A" * 64), ("payload_digest", "a" * 63), ("source", ""), ("source", "x" * 129), ("reference", "x" * 257), ("reason", "x" * 257)]:
            with self.subTest(field=field, invalid=invalid):
                self.assertFalse(dto_validator(self.domain, "CostEvidence").is_valid({**evidence, field: invalid}))
        for name, field, invalid in [("ReserveCostRequest", "category", "fixture_legacy"), ("ReserveCostRequest", "category", "invented"), ("Reservation", "category", "invented"), ("ReserveCostRequest", "provider", "x" * 129), ("ReserveCostRequest", "price_version", "x" * 129), ("ReserveCostRequest", "charge_key", ""), ("ReserveCostRequest", "intent_hash", "bad")]:
            with self.subTest(dto=name, field=field):
                self.assertFalse(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: invalid}))
        dto_validator(self.domain, "Reservation").validate({**self.fixtures["Reservation"], "category": "fixture_legacy"})

    def test_dates_and_counters(self):
        for name in ("Task", "Agent", "Run", "Operation", "Event", "Approval"):
            with self.subTest(dto=name):
                self.assertFalse(dto_validator(self.domain, name).is_valid({**self.fixtures[name], "created_at": "not-a-timestamp"}))
        for name, field, value in [("Event", "sequence", 0), ("Operation", "attempts", -1), ("Operation", "fencing_token", -1), ("Operation", "max_attempts", 0), ("Run", "agent_version", 0), ("Task", "depth", -1), ("Task", "depth", 17)]:
            with self.subTest(dto=name, field=field):
                self.assertFalse(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: value}))

    def test_model_catalog_has_only_safe_documented_fields_and_bounds(self):
        self.assertIn("AvailableModel", self.domain["$defs"])
        schema = self.domain["$defs"]["AvailableModel"]
        self.assertEqual(set(schema["properties"]), {
            "accounting_mode", "max_input_tokens", "max_output_bytes", "timeout_milliseconds",
            "id", "model", "version", "price_version", "max_charge_microusd",
            "max_input_bytes", "max_output_tokens", "bound_assumptions", "created_at",
        })
        for field in ("version", "max_input_bytes", "max_output_tokens"):
            self.assertFalse(dto_validator(self.domain, "AvailableModel").is_valid({**self.fixtures["AvailableModel"], field: 0}))
        self.assertIn("model_connection_version", self.domain["$defs"]["Task"]["properties"])
        self.assertFalse(dto_validator(self.domain, "Task").is_valid({**self.fixtures["Task"], "model_connection_version": 0}))
        # Extension-tolerant response schemas do not guarantee that a server
        # omitted secrets; actual response DTO/handler tests remain necessary.

    def test_http_surface_and_envelopes(self):
        actual = {(route, method) for route, path in self.openapi["paths"].items() for method in path if method in {"get", "post", "put", "patch", "delete"}}
        self.assertEqual(actual, PUBLIC_ROUTES)
        costs = self.openapi["paths"]["/tasks/{id}/costs"]["get"]
        self.assertIn("quarantine_cursor", {p["name"] for p in costs["parameters"]})
        self.assertEqual(self.openapi["openapi"], "3.1.0")
        self.assertEqual(self.openapi["security"], [{"bearerAuth": []}])
        for route, method in actual:
            with self.subTest(route=route, method=method):
                operation = self.openapi["paths"][route][method]
                params = {(p["name"], p["in"]): p for p in operation["parameters"]}
                if route == "/compatibility":
                    self.assertEqual(operation["security"], [])
                    self.assertNotIn(("X-Organization-ID", "header"), params)
                else:
                    self.assertTrue(params[("X-Organization-ID", "header")]["required"])
                self.assertEqual(params[("X-Platform-API-Version", "header")]["required"], route != "/compatibility")
                self.assertEqual(params[("X-Platform-API-Version", "header")]["schema"]["const"], VERSION)
                if method == "post":
                    self.assertTrue(params[("Idempotency-Key", "header")]["required"])
                for code in (400, 401, 403, 404, 409, 500, 503):
                    response_validator(self.openapi, route, method, code).validate({"error": self.fixtures["Error"]})
                code = 201 if (route, method) in {("/configuration/grants", "post"),("/configuration/plans", "post"),("/configuration/plans/{plan_id}/approve", "post"),("/configuration/plans/{plan_id}/verify", "post"),("/service-accounts", "post"),("/members", "post"),("/tasks/{id}/automation", "post"),("/tasks/{id}/tool-executions", "post"),("/agents", "post"), ("/tasks", "post"), ("/tasks/{id}/materials", "post"), ("/tasks/{id}/materials/{material_id}/revisions", "post")} else 200
                if route.endswith("/event-stream"):
                    self.assertIn("text/event-stream",operation["responses"]["200"]["content"])
                else:
                    self.assertFalse(response_validator(self.openapi, route, method, code).is_valid({}))

    def test_permission_boundaries(self):
        roles = self.permissions["roles"]
        self.assertEqual(set(roles), {"viewer", "member", "admin", "owner"})
        self.assertEqual(set(roles["viewer"]), {"tasks.read", "agents.read", "costs.read"})
        self.assertNotIn("tasks.cancel_any", roles["member"])
        self.assertIn("tasks.cancel_own", roles["member"])
        self.assertIn("tasks.cancel_any", roles["admin"])
        self.assertNotIn("organization.manage", roles["admin"])
        self.assertIn("organization.manage", roles["owner"])
        for role in ("viewer", "member"):
            self.assertNotIn("tasks.automate_any", roles[role])
            self.assertNotIn("actions.approve_any", roles[role])
            self.assertNotIn("actions.reconcile_any", roles[role])
        for role in ("admin", "owner"):
            self.assertIn("tasks.automate_any", roles[role])
            self.assertIn("actions.approve_any", roles[role])
            self.assertIn("actions.reconcile_any", roles[role])
        self.assertIn("actions.prepare_own", roles["member"])
        self.assertIn("actions.dispatch_own", roles["member"])
        for role, permissions in roles.items():
            with self.subTest(role=role):
                self.assertEqual(len(permissions), len(set(permissions)))
                self.assertFalse(any(p.startswith("costs.") and p != "costs.read" for p in permissions))

    def test_error_catalog_and_unknown_codes(self):
        expected = {"invalid_request": 400, "unauthenticated": 401, "forbidden": 403,
                    "not_found": 404, "conflict": 409, "incompatible_client": 409, "budget_exceeded": 409,
                    "invalid_transition": 409, "stale_lease": 409,
                    "unavailable": 503, "internal_error": 500}
        self.assertEqual({code: entry["http_status"] for code, entry in self.errors["errors"].items()}, expected)
        self.assertEqual(set(self.domain["$defs"]["Error"]["properties"]["code"]["enum"]), set(expected))
        for code, entry in self.errors["errors"].items():
            self.assertTrue(entry["meaning"].strip())
            dto_validator(self.domain, "Error").validate({**self.fixtures["Error"], "code": code})
        self.assertFalse(dto_validator(self.domain, "Error").is_valid({**self.fixtures["Error"], "code": "invented_error"}))

    def test_utf8_byte_annotations_are_explicit_not_silent_validators(self):
        fields = [("CreateAgent", "name", 120), ("CreateAgent", "instructions", 32768),
                  ("CreateTask", "goal", 16384), ("ReserveCostRequest", "provider", 128),
                  ("ReserveCostRequest", "price_version", 128), ("CostEvidence", "source", 128),
                  ("CostEvidence", "reference", 256), ("CostEvidence", "reason", 256),
                  ("ReviewTaskRequest", "reason", 4096), ("RetryTaskRequest", "reason", 4096),
                  ("RetryTaskRequest", "feedback", 8192),
                  ("CreateMaterial", "name", 120), ("CreateMaterial", "content", 262144), ("CreateMaterial", "reason", 4096),
                  ("ReviseMaterial", "content", 262144), ("ReviseMaterial", "reason", 4096), ("MaterialContent", "content", 262144)]
        for name, field, limit in fields:
            with self.subTest(dto=name, field=field):
                self.assertEqual(self.domain["$defs"][name]["properties"][field]["x-max-utf8-bytes"], limit)
        multibyte_name = "я" * 120
        self.assertGreater(len(multibyte_name.encode("utf-8")), 120)
        # A standard validator counts code points. Server semantic validation
        # must separately reject this name because its UTF-8 byte count is 240.
        dto_validator(self.domain, "CreateAgent").validate({**self.fixtures["CreateAgent"], "name": multibyte_name})

    def test_tool_policy_bounds_and_exact_input_annotations(self):
        for name in ("ToolInfo", "ToolExecution"):
            for value, valid in [("0", True), (str(MAX_MONEY), True), (str(MAX_MONEY + 1), False), (1000, False), ("01", False), ("-1", False), ("1\n", False)]:
                with self.subTest(dto=name, value=value):
                    self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], "max_charge_microusd": value}), valid)
        for name, field, bound in [("ToolInfo", "input_schema_json", 8192), ("PrepareToolExecution", "arguments_json", 32768), ("DecideToolExecution", "reason", 4096), ("ReconcileToolExecution", "reason", 4096), ("ReconcileToolExecution", "effect_reference", 256), ("ReconcileToolExecution", "result", 262144), ("Approval", "reason", 4096)]:
            self.assertEqual(self.domain["$defs"][name]["properties"][field]["x-max-utf8-bytes"], bound)
        # JSON Schema annotations do not parse an embedded JSON string or enforce
        # duplicate keys, Unicode bytes, tool-specific schema or capability checks.
        self.assertTrue(self.domain["$defs"]["PrepareToolExecution"]["properties"]["arguments_json"]["x-content-json-object"])
        dto_validator(self.domain, "PrepareToolExecution").validate({**self.fixtures["PrepareToolExecution"], "arguments_json": "[]"})
        dto_validator(self.domain, "ReconcileToolExecution").validate({**self.fixtures["ReconcileToolExecution"], "result": ""})
        metadata = deepcopy(self.fixtures["ToolExecution"])
        del metadata["arguments_json"]
        dto_validator(self.domain, "ToolExecution").validate(metadata)
        for command in ("approve", "deny", "dispatch", "reconcile"):
            dto_validator(self.domain, "ToolExecutionView").validate({**self.fixtures["ToolExecutionView"], "available_commands": [command]})
        self.assertFalse(dto_validator(self.domain, "ToolExecutionView").is_valid({**self.fixtures["ToolExecutionView"], "available_commands": ["retry_unknown"]}))

    def test_automation_consent_and_monotonic_usage_wire_bounds(self):
        for name, field, minimum, maximum in [
            ("CreateTaskAutomation", "max_model_turns", 1, 31),
            ("TaskAutomationGrant", "max_model_turns", 1, 31),
            ("CreateTaskAutomation", "max_tool_executions", 1, 32),
            ("TaskAutomationGrant", "max_tool_executions", 1, 32),
            ("CreateTaskAutomation", "expires_in_seconds", 1, 86400),
            ("AutomationUsage", "model_turns", 0, 31),
            ("AutomationUsage", "tool_executions", 0, 32),
        ]:
            for value, valid in [(minimum, True), (maximum, True), (minimum - 1, False), (maximum + 1, False), (True, False), (1.5, False)]:
                with self.subTest(dto=name, field=field, value=value):
                    self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: value}), valid)
        for name, field, minimum in [("CreateTaskAutomation", "max_charge_microusd", 1), ("TaskAutomationGrant", "max_charge_microusd", 1), ("AutomationUsage", "allocated_microusd", 0)]:
            for value, valid in [(str(minimum), True), (str(MAX_MONEY), True), (str(minimum - 1), False), (str(MAX_MONEY + 1), False), (1000, False), ("01", False), ("+1", False), ("1.0", False), ("1e3", False), (" 1", False), ("1\n", False)]:
                with self.subTest(dto=name, field=field, value=value):
                    self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], field: value}), valid)
        for name in ("CreateTaskAutomation", "TaskAutomationGrant"):
            for mode in ("read_only", "prepare_and_continue"):
                dto_validator(self.domain, name).validate({**self.fixtures[name], "mode": mode})
            for count, valid in [(0, False), (1, True), (16, True), (17, False)]:
                pins = [{**self.fixtures["AutomationToolPin"], "tool_id": f"tool_{i}"} for i in range(count)]
                self.assertEqual(dto_validator(self.domain, name).is_valid({**self.fixtures[name], "tool_pins": pins}), valid)
        for status in ("active", "revoked", "expired", "stopped", "completed"):
            dto_validator(self.domain, "TaskAutomationGrant").validate({**self.fixtures["TaskAutomationGrant"], "status": status})
        for command in ("create", "revoke"):
            dto_validator(self.domain, "TaskAutomationState").validate({**self.fixtures["TaskAutomationState"], "available_commands": [command]})
        for field, bad in [("tool_id", " "), ("tool_id", "x" * 129), ("tool_version", 0), ("tool_profile_digest", "A" * 64)]:
            self.assertFalse(dto_validator(self.domain, "AutomationToolPin").is_valid({**self.fixtures["AutomationToolPin"], field: bad}))
        for count, valid in [(0, True), (512, True), (513, False)]:
            self.assertEqual(dto_validator(self.domain, "ProgressSignal").is_valid({**self.fixtures["ProgressSignal"], "evidence_ids": [f"execution_{i}" for i in range(count)]}), valid)
        # Structural shape never proves that pins belong to a task, cumulative
        # usage cannot reset, a role remains active, or a receipt permits the
        # next model. Real storage/worker negative tests cover those obligations.
        self.assertFalse(dto_validator(self.domain, "CreateTaskAutomation").is_valid({**self.fixtures["CreateTaskAutomation"], "acknowledge_cost": False}))

    def test_sdk_license_boundary(self):
        # Guardrails plus source inventory, not a mathematical proof of originality.
        for package in (ROOT / "packages/python-sdk", ROOT / "packages/typescript-sdk"):
            self.assertIn("Apache License", (package / "LICENSE").read_text())
            sources = [p for p in (package / "src").rglob("*") if p.is_file() and p.suffix in {".py", ".ts", ".go"}]
            self.assertTrue(sources)
            for path in sources:
                with self.subTest(path=str(path.relative_to(ROOT))):
                    body = path.read_text()
                    self.assertIn("SPDX-License-Identifier: Apache-2.0", body)
                    self.assertNotIn("SPDX-License-Identifier: Elastic-2.0", body)
                    self.assertNotEqual(path.suffix, ".go")
                    for server_only in ("pgxpool", "DATABASE_URL", "internal/storage", "internal/modelgateway", "CREATE TABLE", "BEGIN TRANSACTION"):
                        self.assertNotIn(server_only, body)

    def test_generated_contracts_are_current(self):
        # Build from canonical Go DTOs, execute in an isolated output directory.
        # Never regenerate the checked-in contracts to make the comparison pass.
        sys.path.insert(0, str(ROOT))
        from scripts.dev import environment
        scratch = HERE / ".scratch"
        scratch.mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="drift-", dir=scratch) as temporary:
            temp = Path(temporary)
            (temp / "contracts").mkdir()
            binary = temp / "contract-generator"
            subprocess.run(["go", "build", "-mod=readonly", "-o", str(binary), "./cmd/contracts"], cwd=ROOT, env=environment(), check=True, capture_output=True)
            subprocess.run([str(binary)], cwd=temp, env=environment(), check=True, capture_output=True)
            for filename in ("domain.schema.json", "openapi.json", "state-machines.json", "permissions.json", "errors.json", "event-payloads.json", "event-conformance.json", "capability-catalog.json"):
                with self.subTest(file=filename):
                    self.assertEqual((ROOT / "contracts" / filename).read_bytes(), (temp / "contracts" / filename).read_bytes(), f"Regenerate {filename} from canonical DTOs; do not edit JSON manually")



            for relative in ("internal/capabilities/catalog.json", "tests/contracts/fixtures/product-capabilities.json"):
                with self.subTest(file=relative):
                    self.assertEqual((ROOT / relative).read_bytes(), (temp / relative).read_bytes())

    def test_product_catalogue_matches_all_operations_and_has_no_authority(self):
        catalogue = read_json(ROOT / "contracts/capability-catalog.json")
        observation = read_json(HERE / "fixtures/product-capabilities.json")
        self.assertEqual((ROOT / "contracts/capability-catalog.json").read_bytes(), (ROOT / "internal/capabilities/catalog.json").read_bytes())
        expected = {(method.upper(), "/api/v1" + path): spec["operationId"] for path, methods in self.openapi["paths"].items() for method, spec in methods.items() if method in {"get", "post"}}
        actual = {(op["method"], op["path"]): op["id"] for op in catalogue["operations"]}
        self.assertEqual(actual, expected)
        self.assertEqual(len(actual), len(catalogue["operations"]))
        self.assertEqual(catalogue["api_contract"], VERSION)
        self.assertEqual(len(catalogue["roles"]), 6)
        self.assertEqual(len(catalogue["onboarding"]), 6)
        self.assertEqual({p["id"]: p["status"] for p in catalogue["profiles"]}["provider"], "not_implemented")
        dto_validator(self.domain, "ProductCapabilities").validate(observation)
        for changed in ({"read_only": False}, {"catalog_version": "999"}, {"assessment": "execution_permission"}, {"api_contract": "0.12.0"}):
            self.assertFalse(dto_validator(self.domain, "ProductCapabilities").is_valid({**observation, **changed}))
        schemas = self.domain["$defs"]
        for op in catalogue["operations"]:
            self.assertIn(op["output_schema"], schemas)
            if op["input_schema"]:
                self.assertIn(op["input_schema"], schemas)
            self.assertEqual({b["surface"] for b in op["bindings"]}, {"http", "cli", "python", "typescript", "mcp", "workspace"})
            self.assertNotIn("can_execute", op)
        for item in catalogue["operations"] + catalogue["profiles"] + catalogue["onboarding"]:
            for ref in item["documentation"]:
                self.assertTrue((ROOT / ref).is_file(), ref)


if __name__ == "__main__":
    unittest.main(verbosity=2)
