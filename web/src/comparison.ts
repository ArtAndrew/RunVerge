// SPDX-License-Identifier: Elastic-2.0
import type {Artifact, TaskCandidate, TaskDecision} from './api'

// Historical results have their own run IDs. Do not turn a historical
// comparison into a current review snapshot or infer identity from filenames.
export async function comparisonArtifact(
  organizationId: string, taskId: string, candidate: TaskCandidate, artifact: Artifact,
): Promise<Artifact> {
  if (candidate.organization_id !== organizationId || candidate.task_id !== taskId ||
      artifact.organization_id !== organizationId || artifact.task_id !== taskId ||
      artifact.id !== candidate.artifact_id || artifact.version !== candidate.artifact_version ||
      typeof artifact.content !== 'string' || !/^[a-f0-9]{64}$/.test(candidate.content_digest)) {
    throw new Error('Не удалось подтвердить точную версию для сравнения.')
  }
  const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(artifact.content))
  const digest = [...new Uint8Array(bytes)].map(byte => byte.toString(16).padStart(2, '0')).join('')
  if (digest !== candidate.content_digest) throw new Error('Не удалось подтвердить точную версию для сравнения.')
  return {...artifact}
}

export function comparisonDecisions(candidate: TaskCandidate, decisions: TaskDecision[]): TaskDecision[] {
  return decisions.filter(decision => decision.organization_id === candidate.organization_id &&
    decision.task_id === candidate.task_id && decision.candidate_id === candidate.id &&
    decision.run_id === candidate.run_id && decision.artifact_id === candidate.artifact_id &&
    decision.artifact_version === candidate.artifact_version && decision.content_digest === candidate.content_digest)
}
