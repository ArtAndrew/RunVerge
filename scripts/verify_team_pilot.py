#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Two human identities on an installed disposable server, with fixture agents.

Invoked only by verify_native_bundle --team-pilot. No saved installation, model
key, external service, identity provider or email invitation is used.
"""
import datetime
import hashlib
import json
import secrets
import time
from urllib.error import HTTPError


def require(condition, message):
    if not condition:
        raise RuntimeError('Team pilot: ' + message)


def denied(call, status):
    try:
        call()
    except HTTPError as error:
        require(error.code == status, 'unexpected refusal status')
        error.close()
    else:
        raise RuntimeError('Team pilot: unauthorized request succeeded')


class TeamPilotProbe:
    real_model_called = False

    def __init__(self):
        self.snapshots = {}
        self.identity = None

    def snapshot(self, request, route):
        return {suffix: request(route + suffix) for suffix in
                ('', '/candidates', '/artifacts', '/decisions', '/costs', '/runs', '/children')}

    def await_candidate(self, request, task, worker):
        route = '/api/v1/tasks/' + task['id']
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            task = request(route)
            if task['status'] == 'waiting_input' and task.get('candidate_id'):
                candidates = request(route + '/candidates')
                selected = [item for item in candidates if item['id'] == task['candidate_id']]
                require(len(selected) == 1, 'exact aggregate candidate absent')
                candidate = selected[0]
                artifacts = [item for item in request(route + '/artifacts')
                             if item['id'] == candidate['artifact_id'] and item['version'] == candidate['artifact_version']]
                require(len(artifacts) == 1, 'exact aggregate artifact absent')
                content = artifacts[0]['content']
                require(hashlib.sha256(content.encode()).hexdigest() == candidate['content_digest'], 'aggregate digest mismatch')
                manifest = json.loads(content)
                require(manifest['pattern'] == 'delegate_verify' and manifest['complete'] and
                        manifest['all_fixture'] and manifest['verification'] == 'awaiting_authorized_decision',
                        'aggregate implied acceptance or real-model quality')
                require(len(manifest['members']) == 2 and all(item['status'] == 'candidate' for item in manifest['members']),
                        'two saved stages missing')
                require(task['spent_microusd'] == '2000' and task['reserved_microusd'] == '0',
                        'shared synthetic accounting mismatch')
                return task, candidate
            require(task['status'] not in ('blocked', 'failed', 'cancelled', 'succeeded') and worker.poll() is None,
                    'team stopped before human review')
            time.sleep(0.1)
        raise RuntimeError('Team pilot: aggregate timeout')

    def decision(self, task, candidate):
        return {'expected_version': task['version'], 'candidate_id': candidate['id'],
                'artifact_id': candidate['artifact_id'], 'artifact_version': candidate['artifact_version'],
                'content_digest': candidate['content_digest'], 'decision': 'accept',
                'reason': 'Scripted owner review of the exact synthetic team result.'}

    def revoked_checks(self, request):
        expires = datetime.datetime.fromisoformat(self.identity['expires_at'].replace('Z', '+00:00'))
        require(expires > datetime.datetime.now(datetime.timezone.utc), 'credential expired before revocation check')
        denied(lambda: request('/api/v1/me', identity=self.identity), 401)
        denied(lambda: request(self.member_route, identity=self.identity), 401)
        # A previously accepted command and key must not bypass current auth.
        denied(lambda: request('/api/v1/tasks', self.task_input, identity=self.identity,
                               idempotency_key=self.task_key), 401)
        # Also reject a new, structurally valid command, not only a cache replay.
        denied(lambda: request('/api/v1/tasks', self.task_input, identity=self.identity), 401)
        require(request('/api/v1/me')['principal_id'] == self.owner_id, 'owner control unavailable')

    def __call__(self, context):
        self.phase = 'member_setup'
        try:
            return self.run(context)
        except HTTPError as error:
            status = error.code
            error.close()
            raise RuntimeError('Team pilot: HTTP refusal during ' + self.phase + ' (status ' + str(status) + ')') from None

    def run(self, context):
        request = context['request']
        owner = context['credentials']
        self.owner_id = owner['principal_id']
        member = request('/api/v1/members', {'display_name': 'Synthetic pilot contributor', 'role': 'member',
                                            'reason': 'Disposable two-person package check.'})
        credential_file = context['private'] / 'pilot-member.json'
        context['record']('issue_separate_pilot_member_credential', [context['binary'], 'issue-token',
                          '--organization', owner['organization_id'], '--principal', member['principal_id'],
                          '--out', credential_file, '--ttl', '1h'])
        self.identity = json.loads(credential_file.read_text())
        context['register_sensitive'](self.identity['token'])
        require(self.identity['principal_id'] == member['principal_id'] != self.owner_id and
                self.identity['organization_id'] == owner['organization_id'], 'separate identity mismatch')
        me = request('/api/v1/me', identity=self.identity)
        require(me['role'] == 'member' and me['principal_kind'] == 'human', 'member identity not active')
        denied(lambda: request('/api/v1/members', identity=self.identity), 403)

        self.phase = 'publish_agents'
        agents = []
        for name in ('Draft fixture', 'Review fixture'):
            draft = request('/api/v1/agents', {'name': name, 'instructions': 'Explicit synthetic echo; no factual verification.',
                            'model': 'fixture:echo', 'tools': [], 'budget_microusd': 10000})
            agents.append(request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']}))
        definition = {'pattern': 'delegate_verify', 'join_policy': 'all', 'max_depth': 1, 'max_children': 2,
                      'max_parallel': 1, 'members': [
                          {'id': 'draft', 'agent_id': agents[0]['id'], 'agent_version': agents[0]['version']},
                          {'id': 'review', 'agent_id': agents[1]['id'], 'agent_version': agents[1]['version']}]}
        draft = request('/api/v1/agents', {'name': 'Synthetic pilot team', 'instructions': 'Collect two stages for human review.',
                        'model': 'team:coordinator', 'tools': [], 'budget_microusd': 10000, 'team': definition})
        team = request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']})
        self.task_input = {'agent_id': team['id'], 'goal': 'Synthetic pilot: draft and review the supplied Atlas trial facts.',
                           'budget_microusd': 10000, 'max_runs': 2, 'execution_timeout_seconds': 300}
        self.phase = 'contributor_team'
        self.task_key = secrets.token_hex(16)
        task = request('/api/v1/tasks', self.task_input, identity=self.identity, idempotency_key=self.task_key)
        require(task['owner_id'] == self.identity['principal_id'], 'task not owned by contributor')
        self.member_route = '/api/v1/tasks/' + task['id']
        task, candidate = self.await_candidate(request, task, context['worker'])
        before_costs = request(self.member_route + '/costs')
        self.phase = 'owner_review'
        accepted = request(self.member_route + '/review', self.decision(task, candidate))
        receipts = request(self.member_route + '/decisions')
        exact = [item for item in receipts if item['id'] == accepted.get('accepted_decision_id')]
        require(accepted['status'] == 'succeeded' and len(exact) == 1, 'owner acceptance missing')
        receipt = exact[0]
        require(receipt['principal_id'] == self.owner_id and receipt['method'] == 'authorized_user_decision' and
                receipt['candidate_id'] == candidate['id'] and receipt['artifact_id'] == candidate['artifact_id'] and
                receipt['artifact_version'] == candidate['artifact_version'] and receipt['content_digest'] == candidate['content_digest'],
                'receipt does not identify owner and exact saved result')
        require(request(self.member_route + '/costs') == before_costs, 'review altered shared accounting')
        require(request(self.member_route, identity=self.identity)['accepted_decision_id'] == receipt['id'],
                'contributor cannot see owner decision')

        # The inverse operation must fail: member cannot review another creator.
        self.phase = 'inverse_review_denial'
        other = request('/api/v1/tasks', self.task_input)
        other, other_candidate = self.await_candidate(request, other, context['worker'])
        other_route = '/api/v1/tasks/' + other['id']
        before = self.snapshot(request, other_route)
        denied(lambda: request(other_route + '/review', self.decision(other, other_candidate), identity=self.identity), 403)
        require(self.snapshot(request, other_route) == before, 'denied review changed result or accounting')
        request(other_route + '/review', self.decision(other, other_candidate))

        # Stop before dispatch so a root cancellation can be checked without
        # racing a deliberately fast fixture. The normal restart starts a worker.
        self.phase = 'root_cancellation'
        context['stop_child'](context['worker'])
        cancelled = request('/api/v1/tasks', self.task_input, identity=self.identity)
        cancel_route = '/api/v1/tasks/' + cancelled['id']
        request(cancel_route + '/cancel', {})
        children = request(cancel_route + '/children')
        require(request(cancel_route)['status'] == 'cancelled' and len(children) == 2 and
                all(child['status'] == 'cancelled' for child in children) and
                request(cancel_route)['spent_microusd'] == '0', 'root stop did not close the unstarted team')

        self.snapshots = {route: self.snapshot(request, route) for route in (self.member_route, other_route, cancel_route)}
        self.task_ids = sorted(item['id'] for item in request('/api/v1/tasks'))
        self.phase = 'credential_revocation'
        current = request('/api/v1/members/' + member['principal_id'])
        request('/api/v1/members/' + member['principal_id'] + '/revoke-credentials',
                {'expected_version': current['version'], 'reason': 'End disposable contributor session.'})
        self.revoked_checks(request)
        after = request('/api/v1/members/' + member['principal_id'])
        require(after['active'] and after['role'] == 'member', 'credential revocation changed membership')
        self.verify_snapshots(request)
        return {'scope': 'installed_two_human_identities_with_fixture_team', 'passed': True,
                'separate_contributor_login': True, 'owner_accepts_contributor_exact_aggregate': True,
                'member_cannot_review_owner_result': True, 'review_preserves_accounting': True,
                'root_cancel_closes_unstarted_children': True, 'revoked_read_new_write_and_cached_replay_denied': True,
                'real_model_called': False, 'independent_human_usability_proven': False,
                'enforced_separation_of_duties': False, 'private_tasks_within_organization': False,
                'sso_or_remote_deployment_proven': False}

    def verify_snapshots(self, request):
        require(sorted(item['id'] for item in request('/api/v1/tasks')) == self.task_ids,
                'denied request or restart created a task')
        for route, expected in self.snapshots.items():
            require(self.snapshot(request, route) == expected, 'restart changed task, output, receipt or ledger')
        return True

    def verify_restart(self, request):
        self.revoked_checks(request)
        return self.verify_snapshots(request)
