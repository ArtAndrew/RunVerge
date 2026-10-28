"""Build and inspect both local SDK distributions without publishing or network."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import zipfile
from email.parser import BytesParser

ROOT = Path(__file__).resolve().parents[2]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node", required=True, type=Path)
    parser.add_argument("--npm-cli", required=True, type=Path)
    parser.add_argument("--report", type=Path, default=ROOT / "tests/sdk/package-report-0.13.json")
    args = parser.parse_args()
    if args.report.exists(): parser.error("Use a new report filename to preserve earlier package evidence")
    scratch_root = ROOT.parents[1] / "work" / "sdk-package-checks"
    scratch_root.mkdir(parents=True, exist_ok=True)
    scratch = Path(tempfile.mkdtemp(prefix="v013-", dir=scratch_root))
    env = os.environ.copy()
    env["PATH"] = str(args.node.parent) + os.pathsep + env.get("PATH", "")
    env["npm_config_cache"] = str(scratch / "npm-cache")
    commands = []

    def run(command, cwd):
        commands.append([str(v) for v in command])
        result = subprocess.run(command, cwd=cwd, env=env, text=True, capture_output=True, timeout=120)
        if result.returncode:
            raise RuntimeError(f"Package check command failed ({result.returncode}): {result.stdout}\n{result.stderr}")
        return result

    def inspect_name(name):
        path = Path(name)
        assert not path.is_absolute() and ".." not in path.parts, "unsafe archive member"
        assert not {"internal", ".local", "migrations", "node_modules", "tests"}.intersection(path.parts), "server/private/test code in SDK distribution"

    def inspect_source(name, contents):
        inspect_name(name)
        if name.endswith((".py", ".ts", ".js", ".json", ".md", "LICENSE")):
            assert b"Elastic-2.0" not in contents, "server license leaked into SDK distribution"

    python_dir = ROOT / "packages/python-sdk"
    wheel_dir = scratch / "wheel"
    wheel_dir.mkdir()
    run([sys.executable, "-c", "from setuptools.build_meta import build_wheel; import sys; build_wheel(sys.argv[1])", str(wheel_dir)], python_dir)
    wheels = list(wheel_dir.glob("*.whl"))
    assert len(wheels) == 1 and wheels[0].name == "agent_platform_sdk-0.14.0-py3-none-any.whl"
    python_install = scratch / "python-installed"
    with zipfile.ZipFile(wheels[0]) as archive:
        wheel_files = sorted(archive.namelist())
        for name in wheel_files:
            inspect_source(name, archive.read(name))
        assert "agent_platform/py.typed" in wheel_files
        assert "agent_platform/money.py" in wheel_files
        assert "agent_platform/capabilities.py" in wheel_files
        assert "agent_platform/capability_types.py" in wheel_files
        metadata_name = next(v for v in wheel_files if v.endswith(".dist-info/METADATA"))
        metadata = BytesParser().parsebytes(archive.read(metadata_name))
        assert metadata["Version"] == "0.14.0" and metadata["Requires-Python"] == ">=3.10"
        assert metadata.get_all("Requires-Dist", []) == []
        assert metadata["License"] == "Apache-2.0"
        assert any(v.endswith("/LICENSE") for v in wheel_files)
        archive.extractall(python_install)
    python_check = '''import sys
sys.path.insert(0, sys.argv[1])
from agent_platform import Client, Money, parse_micro_usd, AvailableModel, CostQuarantine, AccountingOutcome, TaskCandidate, TaskDecision, TeamDefinition, Material, MaterialRevision, MaterialContent, ToolInfo, ToolExecution, ToolExecutionView, ToolOutcomeReceipt, ModelToolProposal, ModelToolResultBinding, TaskAutomationGrant, TaskAutomationState, ProgressSignal, OrganizationMember, MembershipCapabilities, MembershipAuditEntry, OrganizationLoginPolicy, ServiceAccount, ServiceKey, OrganizationSecurityAuditEntry, ProductCapabilities, CapabilityOperation, CapabilityProfile, CapabilityOnboarding, CapabilityStep
assert parse_micro_usd("9223372036854775807") == 9223372036854775807
assert parse_micro_usd("-9223372036854775808", signed=True) == -9223372036854775808
for value in ("01", "-0", "1\\n", "9223372036854775808", 1):
    try: parse_micro_usd(value)
    except (ValueError, TypeError): pass
    else: raise AssertionError("malformed money accepted")
for name in ("capabilities", "get_current_identity", "create_task", "get_task", "list_tasks", "cancel_task", "list_agents", "create_agent", "publish_agent", "list_models", "events", "artifacts", "runs", "operations", "actions", "approvals", "costs", "review_task", "retry_task", "candidates", "decisions", "children", "materials", "get_material", "material_revisions", "get_material_version", "create_material", "revise_material", "publish_material", "get_artifact", "tools", "tool_executions", "get_tool_execution", "prepare_tool_execution", "decide_tool_execution", "dispatch_tool_execution", "reconcile_tool_execution", "model_proposals", "get_model_proposal", "continue_model", "automation", "create_automation", "revoke_automation", "progress_signals", "membership_capabilities", "list_members", "get_member", "create_member", "update_member", "revoke_member_credentials", "list_membership_audit", "get_login_policy", "update_login_policy", "list_service_accounts", "get_service_account", "create_service_account", "update_service_account", "list_service_keys", "get_service_key", "revoke_service_key", "list_security_audit", "get_task_event_snapshot", "task_event_feed", "stream_task_events"):
    assert callable(getattr(Client, name))
assert not hasattr(Client, "settle_cost")
'''
    run([sys.executable, "-I", "-c", python_check, str(python_install)], scratch)

    typescript_dir = ROOT / "packages/typescript-sdk"
    packed = run([str(args.node), str(args.npm_cli), "pack", "--json", "--offline", "--pack-destination", str(scratch)], typescript_dir)
    pack_meta = json.loads(packed.stdout)
    assert len(pack_meta) == 1 and pack_meta[0]["version"] == "0.14.0"
    tar_path = scratch / pack_meta[0]["filename"]
    consumer = scratch / "consumer"
    package_install = consumer / "node_modules/@agent-platform/sdk"
    package_install.mkdir(parents=True)
    with tarfile.open(tar_path, "r:gz") as archive:
        ts_files = sorted(v.name for v in archive.getmembers())
        for member in archive.getmembers():
            inspect_name(member.name)
            assert member.name.startswith("package/") and member.isfile(), "unexpected nonfile tar entry"
            data = archive.extractfile(member).read()
            inspect_source(member.name, data)
            destination = package_install / member.name[len("package/"):]
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(data)
    package = json.loads((package_install / "package.json").read_text())
    assert package["version"] == "0.14.0" and package["license"] == "Apache-2.0"
    assert not package.get("dependencies")
    for file in ("dist/index.js", "dist/index.d.ts", "dist/money.js", "dist/money.d.ts", "LICENSE"):
        assert (package_install / file).is_file(), f"missing {file}"
    node_check = '''import assert from 'node:assert/strict';
import {Client, parseMicroUSD} from '@agent-platform/sdk';
assert.equal(parseMicroUSD('9223372036854775807'),9223372036854775807n);
assert.equal(parseMicroUSD('-9223372036854775808',true),-9223372036854775808n);
for(const value of ['01','-0','1\\n','9223372036854775808',1]) assert.throws(()=>parseMicroUSD(value));
for(const name of ['capabilities','getCurrentIdentity','createTask','getTask','listTasks','cancelTask','listAgents','createAgent','publishAgent','models','events','artifacts','runs','operations','actions','approvals','costs','reviewTask','retryTask','candidates','decisions','children','materials','getMaterial','materialRevisions','getMaterialVersion','createMaterial','reviseMaterial','publishMaterial','getArtifact','tools','toolExecutions','getToolExecution','prepareToolExecution','decideToolExecution','dispatchToolExecution','reconcileToolExecution','modelProposals','getModelProposal','continueModel','automation','createAutomation','revokeAutomation','progressSignals','membershipCapabilities','listMembers','getMember','createMember','updateMember','revokeMemberCredentials','listMembershipAudit','getLoginPolicy','updateLoginPolicy','listServiceAccounts','getServiceAccount','createServiceAccount','updateServiceAccount','listServiceKeys','getServiceKey','revokeServiceKey','listSecurityAudit','getTaskEventSnapshot','taskEventFeed','streamTaskEvents']) assert.equal(typeof Client.prototype[name],'function');
assert.equal(Client.prototype.settleCost,undefined);
'''
    run([str(args.node), "--input-type=module", "-e", node_check], consumer)
    declaration_test = consumer / "consumer.mts"
    declaration_test.write_text('''import {parseMicroUSD} from "@agent-platform/sdk";
import type {ProductCapabilities,CapabilityOperation,CapabilityProfile,CapabilityOnboarding,CapabilityStep,Money,Task,AvailableModel,CostQuarantine,AccountingOutcome,ReviewTaskInput,RetryTaskInput,TaskCandidate,TaskDecision,TeamDefinition,Material,MaterialRevision,MaterialContent,PublishMaterialInput,ToolInfo,ToolExecutionView,DispatchToolExecutionInput,ModelToolProposal,ModelToolResultBinding,ContinueModelInput,TaskAutomationGrant,TaskAutomationState,ProgressSignal,CreateTaskAutomationInput,OrganizationMember,MembershipCapabilities,MembershipAuditEntry,UpdateMemberInput} from "@agent-platform/sdk";
const exact: Money = "9223372036854775807";
const value: bigint = parseMicroUSD(exact);
// @ts-expect-error Financial wire facts require canonical strings.
const lossy: Money = 9007199254740993;
declare const task: Task; declare const model: AvailableModel;
declare const quarantine: CostQuarantine; declare const outcome: AccountingOutcome;
const spent: Money = task.spent_microusd;
const retry: RetryTaskInput = {expectedVersion:1,reason:"Revise",acknowledgeNewCost:true};
// @ts-expect-error Explicit cost acknowledgment is mandatory.
const missing: RetryTaskInput = {expectedVersion:1,reason:"Revise"};
declare const candidate: TaskCandidate; declare const decision: TaskDecision;
declare const team: TeamDefinition; declare const review: ReviewTaskInput;
declare const material: Material; declare const revision: MaterialRevision; declare const content: MaterialContent;
const publication: PublishMaterialInput = {expectedTaskVersion:1,expectedMaterialVersion:2,contentDigest:"a".repeat(64),parentCandidateId:"original"};
declare const tool: ToolInfo; declare const executionView: ToolExecutionView;
const bound: Money = tool.max_charge_microusd;
// @ts-expect-error Tool dispatch must acknowledge its potential cost explicitly.
const noToolAck: DispatchToolExecutionInput = {expectedTaskVersion:1,expectedExecutionVersion:1,expectedApprovalVersion:1,intentHash:"a".repeat(64)};
declare const proposal: ModelToolProposal; declare const terminal: ModelToolResultBinding; declare const next: ContinueModelInput;
declare const automation: TaskAutomationState; declare const grant: TaskAutomationGrant; declare const signal: ProgressSignal;
const delegate: CreateTaskAutomationInput = {expectedTaskVersion:1,mode:"read_only",toolPins:[{tool_id:"tool",tool_version:1,tool_profile_digest:"a".repeat(64)}],maxModelTurns:4,maxToolExecutions:8,maxChargeMicrousd:"1000",expiresInSeconds:900,acknowledgeCost:true,reason:"Bounded read"};
// @ts-expect-error Delegation money remains a canonical string.
const numericDelegate: CreateTaskAutomationInput = {...delegate,maxChargeMicrousd:1000};
declare const member: OrganizationMember; declare const caps: MembershipCapabilities; declare const audit: MembershipAuditEntry;
const memberUpdate: UpdateMemberInput={expectedVersion:member.version,displayName:member.display_name,role:"viewer",active:false,reason:"Disable"};
// @ts-expect-error Explicit member status is mandatory.
const missingMemberStatus: UpdateMemberInput={expectedVersion:1,displayName:"Local",role:"member",reason:"Review"};
void [member,caps,audit,memberUpdate,missingMemberStatus,automation,grant,signal,delegate,numericDelegate,proposal,terminal,next,value,lossy,spent,model,quarantine,outcome,retry,missing,candidate,decision,team,review,material,revision,content,publication,bound,executionView,noToolAck];
''')
    run([str(args.node), str(typescript_dir / "node_modules/typescript/bin/tsc"), "--noEmit", "--strict", "--exactOptionalPropertyTypes", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", str(declaration_test)], consumer)
    report = {
        "contract_version": "0.14.0", "result": "pass", "published": False, "network": "npm pack --offline; no registry upload/install",
        "python": {"filename": wheels[0].name, "sha256": hashlib.sha256(wheels[0].read_bytes()).hexdigest(), "files": wheel_files, "runtime_dependencies": [], "py_typed": True, "isolated_import": "pass"},
        "typescript": {"filename": tar_path.name, "sha256": hashlib.sha256(tar_path.read_bytes()).hexdigest(), "files": ts_files, "runtime_dependencies": [], "isolated_runtime_import": "pass", "isolated_declaration_consumer": "pass"},
        "license_boundary": "archive paths and source markers checked; finite guards, not exhaustive provenance audit",
        "commands": commands,
    }
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"result": "pass", "version": "0.14.0", "python_files": len(wheel_files), "typescript_files": len(ts_files), "published": False}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
