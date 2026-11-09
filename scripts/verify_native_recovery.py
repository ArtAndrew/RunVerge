# SPDX-License-Identifier: Elastic-2.0
"""Held recovery drill for the installed native binary and its owned cluster."""
import json
import os
from dataclasses import dataclass


def require(condition, message):
    if not condition:
        raise RuntimeError('Installed held recovery: ' + message)


def private_json(path, value):
    with path.open('x') as output:
        os.fchmod(output.fileno(), 0o600)
        json.dump(value, output)
    return path


@dataclass
class RestoredTarget:
    label: str
    dsn: str
    inspection: tuple
    imported: dict
    read: tuple
    result: dict
    list_tasks: tuple
    listing: dict


class HeldRecoveryProbe:
    real_model_called = False
    held_recovery = True

    def __call__(self, env):
        self.record = env['record']
        self.binary = env['binary']
        self.host_env = {key: value for key, value in env['env'].items() if key != 'DATABASE_URL'}
        private = env['private']
        staging = private / 'recovery-staging'
        staging.mkdir(mode=0o700)
        keys = private / 'recovery-keys.json'
        source_config = private_json(private / 'source.json', {'database_url': env['env']['DATABASE_URL']})
        require(env['maintenance_dsn'] != env['env']['DATABASE_URL'], 'source and target connection match')
        admin = private_json(private / 'maintenance.json', {'database_url': env['maintenance_dsn']})
        require((self.binary.parent.parent / 'RECOVERY-OPERATOR.md').is_file(), 'operator guide missing')
        task = env['fixture_task']
        candidates = env['request']('/api/v1/tasks/' + task['id'] + '/candidates')
        require(len(candidates) == 1, 'source candidate missing')
        candidate = candidates[0]
        artifacts = [a for a in env['fixture_artifacts']
                     if a['id'] == candidate['artifact_id'] and a['version'] == candidate['artifact_version']]
        require(len(artifacts) == 1, 'source artifact missing')
        org = env['credentials']['organization_id']
        self.command('recovery_generate_keys', 'backup-keygen', '--out', keys)
        self.targets = []
        selector = ('--database-config', source_config)
        for label in ('original', 'repeat'):
            bundle = private / ('backup-' + label)
            material = ('--keys-file', keys, '--staging-parent', staging)
            backup = self.command(label + '_backup', 'backup-create', *selector, *material, '--out-dir', bundle)
            if label == 'original':
                env['stop_source_after_backup']()
            archive = ('--backup-dir', bundle, *material)
            selected = (*archive, '--expected-receipt-sha256', backup['receipt_sha256'])
            verified = self.command(label + '_verify_archive', 'archive-verify', *selected)
            require(verified['manifest'] == backup['manifest'], 'archive verification differs')
            plan_file = private / (label + '-plan.json')
            plan = self.command(label + '_plan_target', 'provision-plan', *selected,
                                '--admin-config', admin, '--out', plan_file)
            prepared = self.command(label + '_provision_target', 'provision-apply', '--intent-file', plan_file,
                                    '--expected-database', plan['database'], '--confirm-create-target')
            require(prepared['state'] == 'empty_held', 'new target was not held')
            target_file = str(plan_file) + '.target.json'
            target = ('--target-config', target_file)
            lineage = archive if label == 'repeat' else ()
            imported = self.command(label + '_import', 'archive-import', *target, *archive)
            require(imported['state'] == 'committed_held', 'import not committed held')
            inspection = ('import-inspect', *target, *lineage)
            require(self.command(label + '_inspect', *inspection) == imported, 'import inspection differs')
            scope = private_json(private / (label + '-scopes.json'), {
                'scopes': [{'subject': 'native-drill-reader', 'organization_id': org}],
            })
            authority = ('--authority-dir', private / (label + '-authority'))
            self.command(label + '_init_current_reader', 'access-init', *authority, *target, *lineage,
                         '--scope-file', scope, '--operator-reference', 'native-drill:fixture-operator',
                         '--confirm-current-authority')
            token = private / (label + '-token')
            self.command(label + '_issue_reader', 'issue-read-token', *authority, '--subject', 'native-drill-reader',
                         '--organization-id', org, '--lifetime', '10m', '--out', token)
            env['register_sensitive'](token.read_text().strip())
            read = ('read-result', *authority, *target, '--token-file', token,
                    '--organization-id', org, '--candidate-id', candidate['id'])
            result = self.command(label + '_read_result', *read)
            require(result['content'] == artifacts[0]['content'] and result['content_digest'] == candidate['content_digest']
                    and result['notice']['execution_held'], 'restored result or hold differs')
            list_tasks = ('list-tasks', *authority, *target, '--token-file', token, '--organization-id', org)
            listing = self.command(label + '_read_task', *list_tasks)
            expected = [t for t in listing['tasks'] if t['id'] == task['id']]
            require(len(expected) == 1 and expected[0]['spent_microusd'] == task['spent_microusd']
                    and expected[0]['reserved_microusd'] == task['reserved_microusd'], 'restored cost differs')
            with open(target_file) as stream:
                dsn = json.load(stream)['database_url']
            env['register_sensitive'](dsn)
            self.targets.append(RestoredTarget(label, dsn, inspection, imported, read, result, list_tasks, listing))
            selector = target
        require(not any(staging.iterdir()), 'temporary archive staging remains')
        self.assert_held('before_restart')
        return {'scope': 'installed_held_restore_two_generations', 'restored_targets': len(self.targets),
                'exact_result_and_cost': True, 'execution_held': True,
                'separate_postgres_cluster': True, 'source_offline_during_recovery': True}

    def command(self, step, *args):
        return json.loads(self.record(step, [self.binary, 'recovery', *args], self.host_env))

    def assert_held(self, phase):
        for target in self.targets:
            for command in ('serve', 'worker'):
                stderr = self.record(target.label + '_' + command + '_denied_' + phase, [self.binary, command],
                                     dict(self.host_env, DATABASE_URL=target.dsn), expected_exit=1)
                require('Recovery target is held; ordinary access and execution are disabled' in stderr,
                        'runtime failed for a reason other than the hold')

    def verify_restart(self, _request):
        for target in self.targets:
            require(self.command(target.label + '_inspect_after_restart', *target.inspection) == target.imported,
                    'restart changed import receipt')
            require(self.command(target.label + '_read_after_restart', *target.read) == target.result,
                    'restart changed restored result')
            require(self.command(target.label + '_list_after_restart', *target.list_tasks) == target.listing,
                    'restart changed restored tasks or costs')
        self.assert_held('after_restart')
        return True
