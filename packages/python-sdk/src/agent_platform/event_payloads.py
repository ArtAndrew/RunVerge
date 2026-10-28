# SPDX-License-Identifier: Apache-2.0
"""Generated public protocol declarations; see tests/sdk/generate_event_payloads.py."""
from typing import Literal, TypedDict

EVENT_PAYLOAD_REGISTRY = {'api_version': '0.14.0',
 'registry_version': 1,
 'license': 'Apache-2.0',
 'types': {'action.dispatched': {'fields': {'action_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128},
                                            'dispatch_not_after': {'kind': 'timestamp',
                                                                   'required': True},
                                            'fencing_token': {'kind': 'decimal',
                                                              'required': True,
                                                              'minimum': 1,
                                                              'maximum_decimal': '9223372036854775807'},
                                            'reservation_id': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 128}}},
           'automation.completed': {'fields': {'external_call': {'kind': 'boolean',
                                                                 'required': True,
                                                                 'const': False},
                                               'grant_id': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 128},
                                               'reason': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 4096},
                                               'signal_count': {'kind': 'integer',
                                                                'required': True,
                                                                'maximum': 2147483647},
                                               'snapshot_digest': {'kind': 'digest',
                                                                   'required': True},
                                               'task_acceptance_changed': {'kind': 'boolean',
                                                                           'required': True,
                                                                           'const': False}}},
           'automation.expired': {'fields': {'external_call': {'kind': 'boolean',
                                                               'required': True,
                                                               'const': False},
                                             'grant_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128},
                                             'reason': {'kind': 'string',
                                                        'required': True,
                                                        'max_bytes': 4096},
                                             'signal_count': {'kind': 'integer',
                                                              'required': True,
                                                              'maximum': 2147483647},
                                             'snapshot_digest': {'kind': 'digest',
                                                                 'required': True},
                                             'task_acceptance_changed': {'kind': 'boolean',
                                                                         'required': True,
                                                                         'const': False}}},
           'automation.granted': {'fields': {'acceptance_automated': {'kind': 'boolean',
                                                                      'required': True,
                                                                      'const': False},
                                             'expires_at': {'kind': 'timestamp', 'required': True},
                                             'grant_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128},
                                             'independent_of_login_session': {'kind': 'boolean',
                                                                              'required': True,
                                                                              'const': True},
                                             'max_charge_microusd': {'kind': 'decimal',
                                                                     'required': True,
                                                                     'minimum': 1,
                                                                     'maximum_decimal': '1000000000000'},
                                             'max_model_turns': {'kind': 'integer',
                                                                 'required': True,
                                                                 'minimum': 1,
                                                                 'maximum': 2147483647},
                                             'max_tool_executions': {'kind': 'integer',
                                                                     'required': True,
                                                                     'maximum': 2147483647},
                                             'mode': {'kind': 'string',
                                                      'required': True,
                                                      'max_bytes': 4096},
                                             'principal_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128}}},
           'automation.revoked': {'fields': {'grant_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128},
                                             'past_effects_preserved': {'kind': 'boolean',
                                                                        'required': True,
                                                                        'const': True},
                                             'principal_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128},
                                             'reason': {'kind': 'string',
                                                        'required': True,
                                                        'max_bytes': 4096}}},
           'automation.stopped': {'fields': {'external_call': {'kind': 'boolean',
                                                               'required': True,
                                                               'const': False},
                                             'grant_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128},
                                             'reason': {'kind': 'string',
                                                        'required': True,
                                                        'max_bytes': 4096},
                                             'signal_count': {'kind': 'integer',
                                                              'required': True,
                                                              'maximum': 2147483647},
                                             'snapshot_digest': {'kind': 'digest',
                                                                 'required': True},
                                             'task_acceptance_changed': {'kind': 'boolean',
                                                                         'required': True,
                                                                         'const': False}}},
           'cost.adjustment': {'fields': {'actual_microusd': {'kind': 'decimal',
                                                              'required': True,
                                                              'maximum_decimal': '9223372036854775807'},
                                          'incident': {'kind': 'boolean', 'required': True},
                                          'reservation_id': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 128}}},
           'cost.fixture_reconciled': {'fields': {'actual_microusd': {'kind': 'decimal',
                                                                      'required': True,
                                                                      'maximum_decimal': '9223372036854775807'},
                                                  'basis': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 4096,
                                                            'const': 'fixture_per_logical_operation'},
                                                  'reservation_id': {'kind': 'string',
                                                                     'required': True,
                                                                     'max_bytes': 128},
                                                  'synthetic_cost': {'kind': 'boolean',
                                                                     'required': True,
                                                                     'const': True}}},
           'cost.quarantine_resolution': {'fields': {'actual_microusd': {'kind': 'decimal',
                                                                         'required': True,
                                                                         'maximum_decimal': '9223372036854775807'},
                                                     'incident': {'kind': 'boolean',
                                                                  'required': True},
                                                     'reservation_id': {'kind': 'string',
                                                                        'required': True,
                                                                        'max_bytes': 128}}},
           'cost.quarantined': {'fields': {'actual_microusd': {'kind': 'decimal',
                                                               'required': True,
                                                               'maximum_decimal': '9223372036854775807'},
                                           'evidence_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128},
                                           'quarantine_id': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 128},
                                           'reason': {'kind': 'string',
                                                      'required': True,
                                                      'max_bytes': 4096,
                                                      'const': 'projection_overflow'},
                                           'reservation_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128}}},
           'cost.released': {'fields': {'basis': {'kind': 'string',
                                                  'required': True,
                                                  'max_bytes': 4096,
                                                  'const': 'not_dispatched'},
                                        'reservation_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128}}},
           'cost.reserved': {'fields': {'amount_microusd': {'kind': 'decimal',
                                                            'required': True,
                                                            'maximum_decimal': '9223372036854775807'},
                                        'category': {'kind': 'string',
                                                     'required': True,
                                                     'max_bytes': 4096},
                                        'price_version': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 4096},
                                        'reservation_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128}}},
           'cost.settle': {'fields': {'actual_microusd': {'kind': 'decimal',
                                                          'required': True,
                                                          'maximum_decimal': '9223372036854775807'},
                                      'incident': {'kind': 'boolean', 'required': True},
                                      'reservation_id': {'kind': 'string',
                                                         'required': True,
                                                         'max_bytes': 128}}},
           'cost.settled': {'fields': {'actual_microusd': {'kind': 'decimal',
                                                           'required': True,
                                                           'maximum_decimal': '9223372036854775807'},
                                       'reservation_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128},
                                       'synthetic_cost': {'kind': 'boolean',
                                                          'required': True,
                                                          'const': True}}},
           'cost.unknown': {'fields': {'reason': {'kind': 'string',
                                                  'required': True,
                                                  'max_bytes': 4096},
                                       'reservation_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128}}},
           'material.created': {'fields': {'content_digest': {'kind': 'digest', 'required': True},
                                           'costs_unchanged': {'kind': 'boolean',
                                                               'required': True,
                                                               'const': True},
                                           'draft_only': {'kind': 'boolean',
                                                          'required': True,
                                                          'const': True},
                                           'material_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128},
                                           'principal_id': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 128},
                                           'version': {'kind': 'integer',
                                                       'required': True,
                                                       'minimum': 1,
                                                       'maximum': 2147483647,
                                                       'const': 1}}},
           'material.published': {'fields': {'artifact_id': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 128},
                                             'artifact_version': {'kind': 'integer',
                                                                  'required': True,
                                                                  'minimum': 1,
                                                                  'maximum': 2147483647,
                                                                  'const': 1},
                                             'candidate_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128},
                                             'content_digest': {'kind': 'digest', 'required': True},
                                             'costs_unchanged': {'kind': 'boolean',
                                                                 'required': True,
                                                                 'const': True},
                                             'material_id': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 128},
                                             'material_version': {'kind': 'integer',
                                                                  'required': True,
                                                                  'minimum': 1,
                                                                  'maximum': 2147483647},
                                             'new_run_created': {'kind': 'boolean',
                                                                 'required': True,
                                                                 'const': False},
                                             'parent_candidate_id': {'kind': 'string',
                                                                     'required': True,
                                                                     'max_bytes': 128,
                                                                     'allow_empty': True},
                                             'published_by': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128},
                                             'review_basis_candidate_id': {'kind': 'string',
                                                                           'required': True,
                                                                           'max_bytes': 128,
                                                                           'allow_empty': True}}},
           'material.revised': {'fields': {'base_version': {'kind': 'integer',
                                                            'required': True,
                                                            'minimum': 1,
                                                            'maximum': 2147483647},
                                           'content_digest': {'kind': 'digest', 'required': True},
                                           'costs_unchanged': {'kind': 'boolean',
                                                               'required': True,
                                                               'const': True},
                                           'draft_only': {'kind': 'boolean',
                                                          'required': True,
                                                          'const': True},
                                           'material_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128},
                                           'merge_base_version': {'kind': 'integer',
                                                                  'required': False,
                                                                  'minimum': 1,
                                                                  'maximum': 2147483647},
                                           'principal_id': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 128},
                                           'version': {'kind': 'integer',
                                                       'required': True,
                                                       'minimum': 1,
                                                       'maximum': 2147483647}}},
           'model.candidate_created': {'fields': {'artifact_id': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 128},
                                                  'content_digest': {'kind': 'digest',
                                                                     'required': True},
                                                  'cost_status': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 4096},
                                                  'operation_id': {'kind': 'string',
                                                                   'required': True,
                                                                   'max_bytes': 128},
                                                  'proposal_count': {'kind': 'integer',
                                                                     'required': True,
                                                                     'maximum': 2147483647},
                                                  'reservation_id': {'kind': 'string',
                                                                     'required': True,
                                                                     'max_bytes': 128},
                                                  'source': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 4096},
                                                  'verified': {'kind': 'boolean',
                                                               'required': True,
                                                               'const': False}}},
           'model.continuation_queued': {'fields': {'acknowledged_new_cost': {'kind': 'boolean',
                                                                              'required': True,
                                                                              'const': True},
                                                    'messages_digest': {'kind': 'digest',
                                                                        'required': True},
                                                    'model_turn': {'kind': 'integer',
                                                                   'required': True,
                                                                   'minimum': 1,
                                                                   'maximum': 2147483647},
                                                    'operation_id': {'kind': 'string',
                                                                     'required': True,
                                                                     'max_bytes': 128},
                                                    'prior_liabilities_preserved': {'kind': 'boolean',
                                                                                    'required': True,
                                                                                    'const': True},
                                                    'reason': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 4096},
                                                    'run_id': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 128},
                                                    'source_candidate_id': {'kind': 'string',
                                                                            'required': True,
                                                                            'max_bytes': 128,
                                                                            'allow_empty': True},
                                                    'source_operation_id': {'kind': 'string',
                                                                            'required': True,
                                                                            'max_bytes': 128}}},
           'operation.claimed': {'fields': {'adapter': {'kind': 'string',
                                                        'required': True,
                                                        'max_bytes': 4096,
                                                        'enum': ['fixture', 'model']},
                                            'attempt': {'kind': 'integer',
                                                        'required': True,
                                                        'minimum': 1,
                                                        'maximum': 2147483647},
                                            'fencing_token': {'kind': 'decimal',
                                                              'required': True,
                                                              'minimum': 1,
                                                              'maximum_decimal': '9223372036854775807'},
                                            'model_connection_id': {'kind': 'string',
                                                                    'required': False,
                                                                    'max_bytes': 128},
                                            'model_connection_version': {'kind': 'integer',
                                                                         'required': False,
                                                                         'minimum': 1,
                                                                         'maximum': 2147483647},
                                            'operation_id': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 128},
                                            'synthetic_cost': {'kind': 'boolean',
                                                               'required': False,
                                                               'const': True}},
                                 'variants': [{'field': 'adapter',
                                               'value': 'fixture',
                                               'required': ['synthetic_cost'],
                                               'forbidden': ['model_connection_id',
                                                             'model_connection_version']},
                                              {'field': 'adapter',
                                               'value': 'model',
                                               'required': ['model_connection_id',
                                                            'model_connection_version'],
                                               'forbidden': ['synthetic_cost']}]},
           'task.blocked': {'fields': {'reason': {'kind': 'string',
                                                  'required': True,
                                                  'max_bytes': 4096}}},
           'task.cancelled': {'fields': {'liabilities_preserved': {'kind': 'boolean',
                                                                   'required': True,
                                                                   'const': True},
                                         'reason': {'kind': 'string',
                                                    'required': True,
                                                    'max_bytes': 4096,
                                                    'allow_empty': True}}},
           'task.created': {'fields': {'agent_version': {'kind': 'integer',
                                                         'required': True,
                                                         'minimum': 1,
                                                         'maximum': 2147483647},
                                       'run_id': {'kind': 'string',
                                                  'required': True,
                                                  'max_bytes': 128}}},
           'task.decision_inherited': {'fields': {'candidate_id': {'kind': 'string',
                                                                   'required': True,
                                                                   'max_bytes': 128},
                                                  'decision': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 4096,
                                                               'enum': ['accept',
                                                                        'reject',
                                                                        'request_changes']},
                                                  'decision_id': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 128},
                                                  'method': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 4096,
                                                             'enum': ['inherited_authorized_user_decision']},
                                                  'parent_decision_id': {'kind': 'string',
                                                                         'required': True,
                                                                         'max_bytes': 128}}},
           'task.delegated': {'fields': {'agent_version': {'kind': 'integer',
                                                           'required': True,
                                                           'minimum': 1,
                                                           'maximum': 2147483647},
                                         'member_id': {'kind': 'string',
                                                       'required': True,
                                                       'max_bytes': 128},
                                         'parent_id': {'kind': 'string',
                                                       'required': True,
                                                       'max_bytes': 128},
                                         'root_id': {'kind': 'string',
                                                     'required': True,
                                                     'max_bytes': 128},
                                         'state': {'kind': 'string',
                                                   'required': True,
                                                   'max_bytes': 4096,
                                                   'const': 'dependency_pending'}}},
           'task.new_attempt': {'fields': {'acknowledged_new_cost': {'kind': 'boolean',
                                                                     'required': True,
                                                                     'const': True},
                                           'attempt': {'kind': 'integer',
                                                       'required': True,
                                                       'minimum': 1,
                                                       'maximum': 2147483647},
                                           'input_digest': {'kind': 'digest', 'required': True},
                                           'operation_id': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 128},
                                           'prior_liabilities_preserved': {'kind': 'boolean',
                                                                           'required': True,
                                                                           'const': True},
                                           'reason': {'kind': 'string',
                                                      'required': True,
                                                      'max_bytes': 4096},
                                           'run_id': {'kind': 'string',
                                                      'required': True,
                                                      'max_bytes': 128}}},
           'task.result_decided': {'fields': {'candidate_id': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 128},
                                              'content_digest': {'kind': 'digest',
                                                                 'required': True},
                                              'costs_unchanged': {'kind': 'boolean',
                                                                  'required': True,
                                                                  'const': True},
                                              'decision': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 4096,
                                                           'enum': ['accept',
                                                                    'reject',
                                                                    'request_changes']},
                                              'decision_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128},
                                              'method': {'kind': 'string',
                                                         'required': True,
                                                         'max_bytes': 4096,
                                                         'enum': ['authorized_user_decision']},
                                              'principal_id': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 128},
                                              'run_id': {'kind': 'string',
                                                         'required': True,
                                                         'max_bytes': 128}}},
           'task.succeeded': {'fields': {'adapter': {'kind': 'string',
                                                     'required': True,
                                                     'max_bytes': 4096,
                                                     'const': 'fixture:echo'},
                                         'verification': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 4096,
                                                          'const': 'exact_goal_echo'}}},
           'team.blocked': {'fields': {'reason': {'kind': 'string',
                                                  'required': True,
                                                  'max_bytes': 4096},
                                       'run_id': {'kind': 'string',
                                                  'required': True,
                                                  'max_bytes': 128}}},
           'team.candidate_created': {'fields': {'all_fixture': {'kind': 'boolean',
                                                                 'required': True},
                                                 'artifact_id': {'kind': 'string',
                                                                 'required': True,
                                                                 'max_bytes': 128},
                                                 'candidate_id': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 128},
                                                 'complete': {'kind': 'boolean', 'required': True},
                                                 'content_digest': {'kind': 'digest',
                                                                    'required': True},
                                                 'run_id': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 128},
                                                 'synthetic_cost': {'kind': 'boolean',
                                                                    'required': True},
                                                 'verified': {'kind': 'boolean',
                                                              'required': True,
                                                              'const': False}}},
           'team.initialized': {'fields': {'all_fixture': {'kind': 'boolean', 'required': True},
                                           'join_policy': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 4096,
                                                           'enum': ['all', 'allow_partial']},
                                           'pattern': {'kind': 'string',
                                                       'required': True,
                                                       'max_bytes': 4096,
                                                       'enum': ['sequence',
                                                                'parallel',
                                                                'delegate_verify']},
                                           'plan_digest': {'kind': 'digest', 'required': True},
                                           'run_id': {'kind': 'string',
                                                      'required': True,
                                                      'max_bytes': 128}}},
           'team.member_candidate': {'fields': {'acceptance': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 4096,
                                                               'const': 'not_implied'},
                                                'candidate_id': {'kind': 'string',
                                                                 'required': True,
                                                                 'max_bytes': 128},
                                                'child_task_id': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 128},
                                                'content_digest': {'kind': 'digest',
                                                                   'required': True},
                                                'member_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128},
                                                'run_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128}}},
           'team.member_started': {'fields': {'input_digest': {'kind': 'digest', 'required': True},
                                              'member_id': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 128},
                                              'operation_id': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 128},
                                              'parent_id': {'kind': 'string',
                                                            'required': True,
                                                            'max_bytes': 128},
                                              'parent_run_id': {'kind': 'string',
                                                                'required': True,
                                                                'max_bytes': 128},
                                              'run_id': {'kind': 'string',
                                                         'required': True,
                                                         'max_bytes': 128},
                                              'source_candidate_id': {'kind': 'string',
                                                                      'required': True,
                                                                      'max_bytes': 128,
                                                                      'allow_empty': True}}},
           'tool.approval_decided': {'fields': {'approval_id': {'kind': 'string',
                                                                'required': True,
                                                                'max_bytes': 128},
                                                'decision': {'kind': 'string',
                                                             'required': True,
                                                             'max_bytes': 4096},
                                                'execution_id': {'kind': 'string',
                                                                 'required': True,
                                                                 'max_bytes': 128},
                                                'external_call': {'kind': 'boolean',
                                                                  'required': True,
                                                                  'const': False},
                                                'method': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 4096},
                                                'principal_id': {'kind': 'string',
                                                                 'required': True,
                                                                 'max_bytes': 128}}},
           'tool.cancelled': {'fields': {'execution_id': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 128},
                                         'liabilities_preserved': {'kind': 'boolean',
                                                                   'required': True},
                                         'outcome': {'kind': 'string',
                                                     'required': True,
                                                     'max_bytes': 4096}}},
           'tool.claimed': {'fields': {'attempt': {'kind': 'integer',
                                                   'required': True,
                                                   'minimum': 1,
                                                   'maximum': 2147483647},
                                       'execution_id': {'kind': 'string',
                                                        'required': True,
                                                        'max_bytes': 128},
                                       'fencing_token': {'kind': 'decimal',
                                                         'required': True,
                                                         'minimum': 1,
                                                         'maximum_decimal': '9223372036854775807'}}},
           'tool.cost_released': {'fields': {'execution_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128},
                                             'proof': {'kind': 'string',
                                                       'required': True,
                                                       'max_bytes': 4096,
                                                       'const': 'pending_action_never_dispatched'},
                                             'reservation_id': {'kind': 'string',
                                                                'required': True,
                                                                'max_bytes': 128}}},
           'tool.dispatch_queued': {'fields': {'cost_acknowledged': {'kind': 'boolean',
                                                                     'required': True,
                                                                     'const': True},
                                               'execution_id': {'kind': 'string',
                                                                'required': True,
                                                                'max_bytes': 128},
                                               'external_call': {'kind': 'boolean',
                                                                 'required': True,
                                                                 'const': False},
                                               'intent_hash': {'kind': 'digest', 'required': True},
                                               'principal_id': {'kind': 'string',
                                                                'required': True,
                                                                'max_bytes': 128}}},
           'tool.dispatched': {'fields': {'action_id': {'kind': 'string',
                                                        'required': True,
                                                        'max_bytes': 128},
                                          'dispatch_not_after': {'kind': 'timestamp',
                                                                 'required': True},
                                          'execution_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128},
                                          'fencing_token': {'kind': 'decimal',
                                                            'required': True,
                                                            'minimum': 1,
                                                            'maximum_decimal': '9223372036854775807'},
                                          'intent_hash': {'kind': 'digest', 'required': True}}},
           'tool.effect_observed': {'fields': {'execution_id': {'kind': 'string',
                                                                'required': True,
                                                                'max_bytes': 128},
                                               'financial_settlement': {'kind': 'boolean',
                                                                        'required': True,
                                                                        'const': False},
                                               'method': {'kind': 'string',
                                                          'required': True,
                                                          'max_bytes': 4096,
                                                          'const': 'registered_tool_response'},
                                               'outcome': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 4096},
                                               'receipt_id': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 128}}},
           'tool.operator_reconciled': {'fields': {'execution_id': {'kind': 'string',
                                                                    'required': True,
                                                                    'max_bytes': 128},
                                                   'external_call': {'kind': 'boolean',
                                                                     'required': True,
                                                                     'const': False},
                                                   'financial_settlement': {'kind': 'boolean',
                                                                            'required': True,
                                                                            'const': False},
                                                   'method': {'kind': 'string',
                                                              'required': True,
                                                              'max_bytes': 4096,
                                                              'const': 'operator_assertion'},
                                                   'outcome': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 4096},
                                                   'principal_id': {'kind': 'string',
                                                                    'required': True,
                                                                    'max_bytes': 128},
                                                   'receipt_id': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 128}}},
           'tool.outcome_conflict_overflow': {'fields': {'additional_payload_retained': {'kind': 'boolean',
                                                                                         'required': True,
                                                                                         'const': False},
                                                         'execution_id': {'kind': 'string',
                                                                          'required': True,
                                                                          'max_bytes': 128},
                                                         'receipt_id': {'kind': 'string',
                                                                        'required': True,
                                                                        'max_bytes': 128},
                                                         'requires_operator_review': {'kind': 'boolean',
                                                                                      'required': True,
                                                                                      'const': True},
                                                         'retained_conflicts': {'kind': 'integer',
                                                                                'required': True,
                                                                                'maximum': 2147483647,
                                                                                'const': 8}}},
           'tool.outcome_conflicted': {'fields': {'artifact_id': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 128},
                                                  'artifact_version': {'kind': 'integer',
                                                                       'required': True,
                                                                       'minimum': 1,
                                                                       'maximum': 2147483647,
                                                                       'const': 1},
                                                  'conflict_id': {'kind': 'string',
                                                                  'required': True,
                                                                  'max_bytes': 128},
                                                  'content_digest': {'kind': 'digest',
                                                                     'required': True},
                                                  'effect_reference': {'kind': 'string',
                                                                       'required': True,
                                                                       'max_bytes': 4096},
                                                  'evidence_digest': {'kind': 'digest',
                                                                      'required': True},
                                                  'execution_id': {'kind': 'string',
                                                                   'required': True,
                                                                   'max_bytes': 128},
                                                  'existing_receipt_unchanged': {'kind': 'boolean',
                                                                                 'required': True,
                                                                                 'const': True},
                                                  'financial_settlement': {'kind': 'boolean',
                                                                           'required': True,
                                                                           'const': False},
                                                  'observed_outcome': {'kind': 'string',
                                                                       'required': True,
                                                                       'max_bytes': 4096},
                                                  'receipt_id': {'kind': 'string',
                                                                 'required': True,
                                                                 'max_bytes': 128}}},
           'tool.outcome_unknown': {'fields': {'execution_id': {'kind': 'string',
                                                                'required': True,
                                                                'max_bytes': 128},
                                               'financial_settlement': {'kind': 'boolean',
                                                                        'required': False,
                                                                        'const': False},
                                               'reason': {'kind': 'string',
                                                          'required': False,
                                                          'max_bytes': 4096},
                                               'resend_allowed': {'kind': 'boolean',
                                                                  'required': True,
                                                                  'const': False}}},
           'tool.prepared': {'fields': {'action_id': {'kind': 'string',
                                                      'required': True,
                                                      'max_bytes': 128},
                                        'approval_id': {'kind': 'string',
                                                        'required': True,
                                                        'max_bytes': 128},
                                        'execution_id': {'kind': 'string',
                                                         'required': True,
                                                         'max_bytes': 128},
                                        'external_call': {'kind': 'boolean',
                                                          'required': True,
                                                          'const': False},
                                        'intent_hash': {'kind': 'digest', 'required': True},
                                        'reservation_id': {'kind': 'string',
                                                           'required': True,
                                                           'max_bytes': 128}}},
           'tool.stopped_unsent': {'fields': {'execution_id': {'kind': 'string',
                                                               'required': True,
                                                               'max_bytes': 128},
                                              'external_call': {'kind': 'boolean',
                                                                'required': True,
                                                                'const': False},
                                              'reason': {'kind': 'string',
                                                         'required': True,
                                                         'max_bytes': 4096}}}}}

class EventMetadata(TypedDict):
    id: str
    organization_id: str
    task_id: str
    sequence: str
    created_at: str


class _ActionDispatchedRequired(TypedDict):
    action_id: str
    dispatch_not_after: str
    fencing_token: str
    reservation_id: str


class ActionDispatchedPayload(_ActionDispatchedRequired, total=False):
    pass


class ActionDispatchedEvent(EventMetadata):
    type: Literal['action.dispatched']
    payload_version: Literal[1]
    data: ActionDispatchedPayload


class _AutomationCompletedRequired(TypedDict):
    external_call: Literal[False]
    grant_id: str
    reason: str
    signal_count: int
    snapshot_digest: str
    task_acceptance_changed: Literal[False]


class AutomationCompletedPayload(_AutomationCompletedRequired, total=False):
    pass


class AutomationCompletedEvent(EventMetadata):
    type: Literal['automation.completed']
    payload_version: Literal[1]
    data: AutomationCompletedPayload


class _AutomationExpiredRequired(TypedDict):
    external_call: Literal[False]
    grant_id: str
    reason: str
    signal_count: int
    snapshot_digest: str
    task_acceptance_changed: Literal[False]


class AutomationExpiredPayload(_AutomationExpiredRequired, total=False):
    pass


class AutomationExpiredEvent(EventMetadata):
    type: Literal['automation.expired']
    payload_version: Literal[1]
    data: AutomationExpiredPayload


class _AutomationGrantedRequired(TypedDict):
    acceptance_automated: Literal[False]
    expires_at: str
    grant_id: str
    independent_of_login_session: Literal[True]
    max_charge_microusd: str
    max_model_turns: int
    max_tool_executions: int
    mode: str
    principal_id: str


class AutomationGrantedPayload(_AutomationGrantedRequired, total=False):
    pass


class AutomationGrantedEvent(EventMetadata):
    type: Literal['automation.granted']
    payload_version: Literal[1]
    data: AutomationGrantedPayload


class _AutomationRevokedRequired(TypedDict):
    grant_id: str
    past_effects_preserved: Literal[True]
    principal_id: str
    reason: str


class AutomationRevokedPayload(_AutomationRevokedRequired, total=False):
    pass


class AutomationRevokedEvent(EventMetadata):
    type: Literal['automation.revoked']
    payload_version: Literal[1]
    data: AutomationRevokedPayload


class _AutomationStoppedRequired(TypedDict):
    external_call: Literal[False]
    grant_id: str
    reason: str
    signal_count: int
    snapshot_digest: str
    task_acceptance_changed: Literal[False]


class AutomationStoppedPayload(_AutomationStoppedRequired, total=False):
    pass


class AutomationStoppedEvent(EventMetadata):
    type: Literal['automation.stopped']
    payload_version: Literal[1]
    data: AutomationStoppedPayload


class _CostAdjustmentRequired(TypedDict):
    actual_microusd: str
    incident: bool
    reservation_id: str


class CostAdjustmentPayload(_CostAdjustmentRequired, total=False):
    pass


class CostAdjustmentEvent(EventMetadata):
    type: Literal['cost.adjustment']
    payload_version: Literal[1]
    data: CostAdjustmentPayload


class _CostFixtureReconciledRequired(TypedDict):
    actual_microusd: str
    basis: Literal['fixture_per_logical_operation']
    reservation_id: str
    synthetic_cost: Literal[True]


class CostFixtureReconciledPayload(_CostFixtureReconciledRequired, total=False):
    pass


class CostFixtureReconciledEvent(EventMetadata):
    type: Literal['cost.fixture_reconciled']
    payload_version: Literal[1]
    data: CostFixtureReconciledPayload


class _CostQuarantineResolutionRequired(TypedDict):
    actual_microusd: str
    incident: bool
    reservation_id: str


class CostQuarantineResolutionPayload(_CostQuarantineResolutionRequired, total=False):
    pass


class CostQuarantineResolutionEvent(EventMetadata):
    type: Literal['cost.quarantine_resolution']
    payload_version: Literal[1]
    data: CostQuarantineResolutionPayload


class _CostQuarantinedRequired(TypedDict):
    actual_microusd: str
    evidence_id: str
    quarantine_id: str
    reason: Literal['projection_overflow']
    reservation_id: str


class CostQuarantinedPayload(_CostQuarantinedRequired, total=False):
    pass


class CostQuarantinedEvent(EventMetadata):
    type: Literal['cost.quarantined']
    payload_version: Literal[1]
    data: CostQuarantinedPayload


class _CostReleasedRequired(TypedDict):
    basis: Literal['not_dispatched']
    reservation_id: str


class CostReleasedPayload(_CostReleasedRequired, total=False):
    pass


class CostReleasedEvent(EventMetadata):
    type: Literal['cost.released']
    payload_version: Literal[1]
    data: CostReleasedPayload


class _CostReservedRequired(TypedDict):
    amount_microusd: str
    category: str
    price_version: str
    reservation_id: str


class CostReservedPayload(_CostReservedRequired, total=False):
    pass


class CostReservedEvent(EventMetadata):
    type: Literal['cost.reserved']
    payload_version: Literal[1]
    data: CostReservedPayload


class _CostSettleRequired(TypedDict):
    actual_microusd: str
    incident: bool
    reservation_id: str


class CostSettlePayload(_CostSettleRequired, total=False):
    pass


class CostSettleEvent(EventMetadata):
    type: Literal['cost.settle']
    payload_version: Literal[1]
    data: CostSettlePayload


class _CostSettledRequired(TypedDict):
    actual_microusd: str
    reservation_id: str
    synthetic_cost: Literal[True]


class CostSettledPayload(_CostSettledRequired, total=False):
    pass


class CostSettledEvent(EventMetadata):
    type: Literal['cost.settled']
    payload_version: Literal[1]
    data: CostSettledPayload


class _CostUnknownRequired(TypedDict):
    reason: str
    reservation_id: str


class CostUnknownPayload(_CostUnknownRequired, total=False):
    pass


class CostUnknownEvent(EventMetadata):
    type: Literal['cost.unknown']
    payload_version: Literal[1]
    data: CostUnknownPayload


class _MaterialCreatedRequired(TypedDict):
    content_digest: str
    costs_unchanged: Literal[True]
    draft_only: Literal[True]
    material_id: str
    principal_id: str
    version: Literal[1]


class MaterialCreatedPayload(_MaterialCreatedRequired, total=False):
    pass


class MaterialCreatedEvent(EventMetadata):
    type: Literal['material.created']
    payload_version: Literal[1]
    data: MaterialCreatedPayload


class _MaterialPublishedRequired(TypedDict):
    artifact_id: str
    artifact_version: Literal[1]
    candidate_id: str
    content_digest: str
    costs_unchanged: Literal[True]
    material_id: str
    material_version: int
    new_run_created: Literal[False]
    parent_candidate_id: str
    published_by: str
    review_basis_candidate_id: str


class MaterialPublishedPayload(_MaterialPublishedRequired, total=False):
    pass


class MaterialPublishedEvent(EventMetadata):
    type: Literal['material.published']
    payload_version: Literal[1]
    data: MaterialPublishedPayload


class _MaterialRevisedRequired(TypedDict):
    base_version: int
    content_digest: str
    costs_unchanged: Literal[True]
    draft_only: Literal[True]
    material_id: str
    principal_id: str
    version: int


class MaterialRevisedPayload(_MaterialRevisedRequired, total=False):
    merge_base_version: int


class MaterialRevisedEvent(EventMetadata):
    type: Literal['material.revised']
    payload_version: Literal[1]
    data: MaterialRevisedPayload


class _ModelCandidateCreatedRequired(TypedDict):
    artifact_id: str
    content_digest: str
    cost_status: str
    operation_id: str
    proposal_count: int
    reservation_id: str
    source: str
    verified: Literal[False]


class ModelCandidateCreatedPayload(_ModelCandidateCreatedRequired, total=False):
    pass


class ModelCandidateCreatedEvent(EventMetadata):
    type: Literal['model.candidate_created']
    payload_version: Literal[1]
    data: ModelCandidateCreatedPayload


class _ModelContinuationQueuedRequired(TypedDict):
    acknowledged_new_cost: Literal[True]
    messages_digest: str
    model_turn: int
    operation_id: str
    prior_liabilities_preserved: Literal[True]
    reason: str
    run_id: str
    source_candidate_id: str
    source_operation_id: str


class ModelContinuationQueuedPayload(_ModelContinuationQueuedRequired, total=False):
    pass


class ModelContinuationQueuedEvent(EventMetadata):
    type: Literal['model.continuation_queued']
    payload_version: Literal[1]
    data: ModelContinuationQueuedPayload


class _OperationClaimedRequired(TypedDict):
    adapter: Literal['fixture', 'model']
    attempt: int
    fencing_token: str
    operation_id: str


class OperationClaimedPayload(_OperationClaimedRequired, total=False):
    model_connection_id: str
    model_connection_version: int
    synthetic_cost: Literal[True]


class OperationClaimedEvent(EventMetadata):
    type: Literal['operation.claimed']
    payload_version: Literal[1]
    data: OperationClaimedPayload


class _TaskBlockedRequired(TypedDict):
    reason: str


class TaskBlockedPayload(_TaskBlockedRequired, total=False):
    pass


class TaskBlockedEvent(EventMetadata):
    type: Literal['task.blocked']
    payload_version: Literal[1]
    data: TaskBlockedPayload


class _TaskCancelledRequired(TypedDict):
    liabilities_preserved: Literal[True]
    reason: str


class TaskCancelledPayload(_TaskCancelledRequired, total=False):
    pass


class TaskCancelledEvent(EventMetadata):
    type: Literal['task.cancelled']
    payload_version: Literal[1]
    data: TaskCancelledPayload


class _TaskCreatedRequired(TypedDict):
    agent_version: int
    run_id: str


class TaskCreatedPayload(_TaskCreatedRequired, total=False):
    pass


class TaskCreatedEvent(EventMetadata):
    type: Literal['task.created']
    payload_version: Literal[1]
    data: TaskCreatedPayload


class _TaskDecisionInheritedRequired(TypedDict):
    candidate_id: str
    decision: Literal['accept', 'reject', 'request_changes']
    decision_id: str
    method: Literal['inherited_authorized_user_decision']
    parent_decision_id: str


class TaskDecisionInheritedPayload(_TaskDecisionInheritedRequired, total=False):
    pass


class TaskDecisionInheritedEvent(EventMetadata):
    type: Literal['task.decision_inherited']
    payload_version: Literal[1]
    data: TaskDecisionInheritedPayload


class _TaskDelegatedRequired(TypedDict):
    agent_version: int
    member_id: str
    parent_id: str
    root_id: str
    state: Literal['dependency_pending']


class TaskDelegatedPayload(_TaskDelegatedRequired, total=False):
    pass


class TaskDelegatedEvent(EventMetadata):
    type: Literal['task.delegated']
    payload_version: Literal[1]
    data: TaskDelegatedPayload


class _TaskNewAttemptRequired(TypedDict):
    acknowledged_new_cost: Literal[True]
    attempt: int
    input_digest: str
    operation_id: str
    prior_liabilities_preserved: Literal[True]
    reason: str
    run_id: str


class TaskNewAttemptPayload(_TaskNewAttemptRequired, total=False):
    pass


class TaskNewAttemptEvent(EventMetadata):
    type: Literal['task.new_attempt']
    payload_version: Literal[1]
    data: TaskNewAttemptPayload


class _TaskResultDecidedRequired(TypedDict):
    candidate_id: str
    content_digest: str
    costs_unchanged: Literal[True]
    decision: Literal['accept', 'reject', 'request_changes']
    decision_id: str
    method: Literal['authorized_user_decision']
    principal_id: str
    run_id: str


class TaskResultDecidedPayload(_TaskResultDecidedRequired, total=False):
    pass


class TaskResultDecidedEvent(EventMetadata):
    type: Literal['task.result_decided']
    payload_version: Literal[1]
    data: TaskResultDecidedPayload


class _TaskSucceededRequired(TypedDict):
    adapter: Literal['fixture:echo']
    verification: Literal['exact_goal_echo']


class TaskSucceededPayload(_TaskSucceededRequired, total=False):
    pass


class TaskSucceededEvent(EventMetadata):
    type: Literal['task.succeeded']
    payload_version: Literal[1]
    data: TaskSucceededPayload


class _TeamBlockedRequired(TypedDict):
    reason: str
    run_id: str


class TeamBlockedPayload(_TeamBlockedRequired, total=False):
    pass


class TeamBlockedEvent(EventMetadata):
    type: Literal['team.blocked']
    payload_version: Literal[1]
    data: TeamBlockedPayload


class _TeamCandidateCreatedRequired(TypedDict):
    all_fixture: bool
    artifact_id: str
    candidate_id: str
    complete: bool
    content_digest: str
    run_id: str
    synthetic_cost: bool
    verified: Literal[False]


class TeamCandidateCreatedPayload(_TeamCandidateCreatedRequired, total=False):
    pass


class TeamCandidateCreatedEvent(EventMetadata):
    type: Literal['team.candidate_created']
    payload_version: Literal[1]
    data: TeamCandidateCreatedPayload


class _TeamInitializedRequired(TypedDict):
    all_fixture: bool
    join_policy: Literal['all', 'allow_partial']
    pattern: Literal['sequence', 'parallel', 'delegate_verify']
    plan_digest: str
    run_id: str


class TeamInitializedPayload(_TeamInitializedRequired, total=False):
    pass


class TeamInitializedEvent(EventMetadata):
    type: Literal['team.initialized']
    payload_version: Literal[1]
    data: TeamInitializedPayload


class _TeamMemberCandidateRequired(TypedDict):
    acceptance: Literal['not_implied']
    candidate_id: str
    child_task_id: str
    content_digest: str
    member_id: str
    run_id: str


class TeamMemberCandidatePayload(_TeamMemberCandidateRequired, total=False):
    pass


class TeamMemberCandidateEvent(EventMetadata):
    type: Literal['team.member_candidate']
    payload_version: Literal[1]
    data: TeamMemberCandidatePayload


class _TeamMemberStartedRequired(TypedDict):
    input_digest: str
    member_id: str
    operation_id: str
    parent_id: str
    parent_run_id: str
    run_id: str
    source_candidate_id: str


class TeamMemberStartedPayload(_TeamMemberStartedRequired, total=False):
    pass


class TeamMemberStartedEvent(EventMetadata):
    type: Literal['team.member_started']
    payload_version: Literal[1]
    data: TeamMemberStartedPayload


class _ToolApprovalDecidedRequired(TypedDict):
    approval_id: str
    decision: str
    execution_id: str
    external_call: Literal[False]
    method: str
    principal_id: str


class ToolApprovalDecidedPayload(_ToolApprovalDecidedRequired, total=False):
    pass


class ToolApprovalDecidedEvent(EventMetadata):
    type: Literal['tool.approval_decided']
    payload_version: Literal[1]
    data: ToolApprovalDecidedPayload


class _ToolCancelledRequired(TypedDict):
    execution_id: str
    liabilities_preserved: bool
    outcome: str


class ToolCancelledPayload(_ToolCancelledRequired, total=False):
    pass


class ToolCancelledEvent(EventMetadata):
    type: Literal['tool.cancelled']
    payload_version: Literal[1]
    data: ToolCancelledPayload


class _ToolClaimedRequired(TypedDict):
    attempt: int
    execution_id: str
    fencing_token: str


class ToolClaimedPayload(_ToolClaimedRequired, total=False):
    pass


class ToolClaimedEvent(EventMetadata):
    type: Literal['tool.claimed']
    payload_version: Literal[1]
    data: ToolClaimedPayload


class _ToolCostReleasedRequired(TypedDict):
    execution_id: str
    proof: Literal['pending_action_never_dispatched']
    reservation_id: str


class ToolCostReleasedPayload(_ToolCostReleasedRequired, total=False):
    pass


class ToolCostReleasedEvent(EventMetadata):
    type: Literal['tool.cost_released']
    payload_version: Literal[1]
    data: ToolCostReleasedPayload


class _ToolDispatchQueuedRequired(TypedDict):
    cost_acknowledged: Literal[True]
    execution_id: str
    external_call: Literal[False]
    intent_hash: str
    principal_id: str


class ToolDispatchQueuedPayload(_ToolDispatchQueuedRequired, total=False):
    pass


class ToolDispatchQueuedEvent(EventMetadata):
    type: Literal['tool.dispatch_queued']
    payload_version: Literal[1]
    data: ToolDispatchQueuedPayload


class _ToolDispatchedRequired(TypedDict):
    action_id: str
    dispatch_not_after: str
    execution_id: str
    fencing_token: str
    intent_hash: str


class ToolDispatchedPayload(_ToolDispatchedRequired, total=False):
    pass


class ToolDispatchedEvent(EventMetadata):
    type: Literal['tool.dispatched']
    payload_version: Literal[1]
    data: ToolDispatchedPayload


class _ToolEffectObservedRequired(TypedDict):
    execution_id: str
    financial_settlement: Literal[False]
    method: Literal['registered_tool_response']
    outcome: str
    receipt_id: str


class ToolEffectObservedPayload(_ToolEffectObservedRequired, total=False):
    pass


class ToolEffectObservedEvent(EventMetadata):
    type: Literal['tool.effect_observed']
    payload_version: Literal[1]
    data: ToolEffectObservedPayload


class _ToolOperatorReconciledRequired(TypedDict):
    execution_id: str
    external_call: Literal[False]
    financial_settlement: Literal[False]
    method: Literal['operator_assertion']
    outcome: str
    principal_id: str
    receipt_id: str


class ToolOperatorReconciledPayload(_ToolOperatorReconciledRequired, total=False):
    pass


class ToolOperatorReconciledEvent(EventMetadata):
    type: Literal['tool.operator_reconciled']
    payload_version: Literal[1]
    data: ToolOperatorReconciledPayload


class _ToolOutcomeConflictOverflowRequired(TypedDict):
    additional_payload_retained: Literal[False]
    execution_id: str
    receipt_id: str
    requires_operator_review: Literal[True]
    retained_conflicts: Literal[8]


class ToolOutcomeConflictOverflowPayload(_ToolOutcomeConflictOverflowRequired, total=False):
    pass


class ToolOutcomeConflictOverflowEvent(EventMetadata):
    type: Literal['tool.outcome_conflict_overflow']
    payload_version: Literal[1]
    data: ToolOutcomeConflictOverflowPayload


class _ToolOutcomeConflictedRequired(TypedDict):
    artifact_id: str
    artifact_version: Literal[1]
    conflict_id: str
    content_digest: str
    effect_reference: str
    evidence_digest: str
    execution_id: str
    existing_receipt_unchanged: Literal[True]
    financial_settlement: Literal[False]
    observed_outcome: str
    receipt_id: str


class ToolOutcomeConflictedPayload(_ToolOutcomeConflictedRequired, total=False):
    pass


class ToolOutcomeConflictedEvent(EventMetadata):
    type: Literal['tool.outcome_conflicted']
    payload_version: Literal[1]
    data: ToolOutcomeConflictedPayload


class _ToolOutcomeUnknownRequired(TypedDict):
    execution_id: str
    resend_allowed: Literal[False]


class ToolOutcomeUnknownPayload(_ToolOutcomeUnknownRequired, total=False):
    financial_settlement: Literal[False]
    reason: str


class ToolOutcomeUnknownEvent(EventMetadata):
    type: Literal['tool.outcome_unknown']
    payload_version: Literal[1]
    data: ToolOutcomeUnknownPayload


class _ToolPreparedRequired(TypedDict):
    action_id: str
    approval_id: str
    execution_id: str
    external_call: Literal[False]
    intent_hash: str
    reservation_id: str


class ToolPreparedPayload(_ToolPreparedRequired, total=False):
    pass


class ToolPreparedEvent(EventMetadata):
    type: Literal['tool.prepared']
    payload_version: Literal[1]
    data: ToolPreparedPayload


class _ToolStoppedUnsentRequired(TypedDict):
    execution_id: str
    external_call: Literal[False]
    reason: str


class ToolStoppedUnsentPayload(_ToolStoppedUnsentRequired, total=False):
    pass


class ToolStoppedUnsentEvent(EventMetadata):
    type: Literal['tool.stopped_unsent']
    payload_version: Literal[1]
    data: ToolStoppedUnsentPayload


TypedEvent = ActionDispatchedEvent | AutomationCompletedEvent | AutomationExpiredEvent | AutomationGrantedEvent | AutomationRevokedEvent | AutomationStoppedEvent | CostAdjustmentEvent | CostFixtureReconciledEvent | CostQuarantineResolutionEvent | CostQuarantinedEvent | CostReleasedEvent | CostReservedEvent | CostSettleEvent | CostSettledEvent | CostUnknownEvent | MaterialCreatedEvent | MaterialPublishedEvent | MaterialRevisedEvent | ModelCandidateCreatedEvent | ModelContinuationQueuedEvent | OperationClaimedEvent | TaskBlockedEvent | TaskCancelledEvent | TaskCreatedEvent | TaskDecisionInheritedEvent | TaskDelegatedEvent | TaskNewAttemptEvent | TaskResultDecidedEvent | TaskSucceededEvent | TeamBlockedEvent | TeamCandidateCreatedEvent | TeamInitializedEvent | TeamMemberCandidateEvent | TeamMemberStartedEvent | ToolApprovalDecidedEvent | ToolCancelledEvent | ToolClaimedEvent | ToolCostReleasedEvent | ToolDispatchQueuedEvent | ToolDispatchedEvent | ToolEffectObservedEvent | ToolOperatorReconciledEvent | ToolOutcomeConflictOverflowEvent | ToolOutcomeConflictedEvent | ToolOutcomeUnknownEvent | ToolPreparedEvent | ToolStoppedUnsentEvent
