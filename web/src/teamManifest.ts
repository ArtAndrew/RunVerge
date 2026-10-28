// SPDX-License-Identifier: Elastic-2.0
// Presentation-only parser for the version-1 manifest emitted by team_results.go.
// It never authorizes a decision, resolves a link, or parses quoted source content.
export const teamResultLimits = {bytes: 256 * 1024, depth: 8, values: 4096, members: 32} as const

export interface TeamSource {
  task_id: string
  candidate_id: string
  artifact_id: string
  artifact_version: number
  content_digest: string
  content: string
}
export interface TeamMemberResult {
  id: string
  task_id: string
  run_id?: string
  status: 'candidate' | 'failed' | 'cancelled' | 'skipped'
  result?: TeamSource
}
export interface TeamManifest {
  version: 1
  pattern: 'sequence' | 'parallel' | 'delegate_verify'
  join_policy: 'all' | 'allow_partial'
  complete: boolean
  all_fixture: boolean
  verification: 'awaiting_authorized_decision'
  members: TeamMemberResult[]
}
export type TeamParseResult = {ok: true; manifest: TeamManifest; bytes: number} | {ok: false; reason: 'size' | 'depth' | 'count' | 'format'}

const digest = /^[0-9a-f]{64}(?![\s\S])/
const object = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value)
function text(value: unknown, maxBytes = 128, nonblank = true): value is string {
  return typeof value === 'string' && (!nonblank || Boolean(value.trim())) && !value.includes('\0') &&
    ![...value].some(c => {const n = c.codePointAt(0)!; return n >= 0xd800 && n <= 0xdfff}) &&
    value.length <= maxBytes && new TextEncoder().encode(value).byteLength <= maxBytes
}
function keys(value: Record<string, unknown>, expected: string[]): boolean {
  return Object.keys(value).every(key => expected.includes(key))
}

// Preflight nesting/size before JSON.parse. Strings are left opaque, including
// JSON-looking source text. Duplicate keys (also escaped aliases) are rejected.
function preflight(content: string): 'depth' | 'count' | 'format' | null {
  const stack: {kind: string; keys: Set<string>}[] = []
  let values = 0
  for (let i = 0; i < content.length; i++) {
    const char = content[i]
    if (char === '"') {
      const start = i
      for (i++; i < content.length && content[i] !== '"'; i++) if (content[i] === '\\') i++
      if (i >= content.length) return 'format'
      if (++values > teamResultLimits.values) return 'count'
      let next = i + 1
      while (/\s/.test(content[next] ?? '') && next < content.length) next++
      const frame = stack.at(-1)
      if (frame?.kind === '{' && content[next] === ':') {
        let key: unknown
        try {key = JSON.parse(content.slice(start, i + 1))} catch {return 'format'}
        if (typeof key !== 'string' || frame.keys.has(key)) return 'format'
        frame.keys.add(key)
      }
    } else if (char === '{' || char === '[') {
      stack.push({kind: char, keys: new Set()})
      if (stack.length > teamResultLimits.depth) return 'depth'
      if (++values > teamResultLimits.values) return 'count'
    } else if (char === '}' || char === ']') {
      if (stack.pop()?.kind !== (char === '}' ? '{' : '[')) return 'format'
    } else if (char === ',' && ++values > teamResultLimits.values) return 'count'
  }
  return stack.length ? 'format' : null
}

export function parseTeamResult(content: string): TeamParseResult {
  if (typeof content !== 'string' || content.length > teamResultLimits.bytes) return {ok: false, reason: 'size'}
  const bytes = new TextEncoder().encode(content).byteLength
  if (bytes > teamResultLimits.bytes) return {ok: false, reason: 'size'}
  const bounded = preflight(content)
  if (bounded) return {ok: false, reason: bounded}
  let value: unknown
  try {value = JSON.parse(content)} catch {return {ok: false, reason: 'format'}}
  const invalid: TeamParseResult = {ok: false, reason: 'format'}
  if (!object(value) || !keys(value, ['version','pattern','join_policy','complete','all_fixture','verification','members']) ||
      value.version !== 1 || (typeof value.pattern !== 'string' || !['sequence','parallel','delegate_verify'].includes(value.pattern)) ||
      (typeof value.join_policy !== 'string' || !['all','allow_partial'].includes(value.join_policy)) || typeof value.complete !== 'boolean' ||
      typeof value.all_fixture !== 'boolean' || value.verification !== 'awaiting_authorized_decision' ||
      !Array.isArray(value.members) || value.members.length < 1 || value.members.length > teamResultLimits.members) return invalid
  if (value.pattern === 'delegate_verify' && value.members.length !== 2) return invalid
  const ids = new Set<string>(), tasks = new Set<string>()
  for (const member of value.members) {
    if (!object(member) || !keys(member, ['id','task_id','run_id','status','result']) ||
        !text(member.id) || !text(member.task_id) || ids.has(member.id) || tasks.has(member.task_id) ||
        (typeof member.status !== 'string' || !['candidate','failed','cancelled','skipped'].includes(member.status)) ||
        ('run_id' in member && !text(member.run_id))) return invalid
    ids.add(member.id); tasks.add(member.task_id)
    if (member.status === 'candidate') {
      const source = member.result
      if (!text(member.run_id) || !object(source) || !keys(source, ['task_id','candidate_id','artifact_id','artifact_version','content_digest','content']) ||
          source.task_id !== member.task_id || !text(source.candidate_id) || !text(source.artifact_id) ||
          !Number.isSafeInteger(source.artifact_version) || Number(source.artifact_version) < 1 ||
          typeof source.content_digest !== 'string' || !digest.test(source.content_digest) ||
          !text(source.content, teamResultLimits.bytes, false)) return invalid
    } else if ('result' in member) return invalid
  }
  const complete = value.members.every(member => member.status === 'candidate')
  if (value.complete !== complete || (value.join_policy === 'all' && !complete) || !value.members.some(member => member.status === 'candidate')) return invalid
  return {ok: true, manifest: value as unknown as TeamManifest, bytes}
}

export const teamPatternLabels = {sequence: 'Последовательно', parallel: 'Параллельно', delegate_verify: 'Исполнитель и проверяющий'} as const
export const teamMemberLabels = {candidate: 'Материал включён', failed: 'Не удалось получить материал', cancelled: 'Выполнение отменено', skipped: 'Участник пропущен'} as const
export function exactTextBytes(content: string): number {return new TextEncoder().encode(content).byteLength}
