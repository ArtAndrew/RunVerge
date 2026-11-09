// SPDX-License-Identifier: Elastic-2.0

type LogLine = {number: number; text: string}
type Reference = {text: string; lines: LogLine[]}
type ReferenceRepair = {before: string; after: string}
export type CIReferenceReport =
  | {status: 'unavailable'}
  | {status: 'missing' | 'issues' | 'matched'; references: Reference[];
      repair?: {draft: string; replacements: ReferenceRepair[]}}

const referenceToken = /https?:\/\/[^\s<>()`\[\]"']+|(?<![\w/#])#L\d+(?:-#?L\d+)?/gi
const maxDraftLength = 65536

// This opt-in format is used by examples/ci-triage. References are matched to
// the supplied excerpt only. No URL is opened and no claim is judged here.
export function checkCIReferences(goal: string, draft: string): CIReferenceReport | null {
  const header = /^Run URL:[^\S\r\n]*(.*?)\s*$/gm
  const sources = [...goal.matchAll(header)]
  if (sources.length === 0) return null
  const source = sources[0][1]
  if (sources.length !== 1 || goal.length > 16384 || draft.length > maxDraftLength || !validSource(source)) return {status: 'unavailable'}
  const log = new Map<number, LogLine>()
  for (const line of goal.split(/\r?\n/)) {
    const match = /^L([1-9]\d{0,6})[ \t]+(.+)$/.exec(line)
    if (!match) continue
    const number = Number(match[1])
    if (log.has(number)) return {status: 'unavailable'}
    log.set(number, {number, text: match[2]})
  }
  if (!log.size) return {status: 'unavailable'}

  // Take whole URL tokens first so a foreign URL's suffix cannot match as a
  // separate anchor. Bare anchors are shown as unresolved references.
  const tokens = draft.match(referenceToken) ?? []
  const unique = [...new Set(tokens.map(token => token.replace(/[.,;:!?]+$/, '')))]
  if (unique.length > 32) return {status: 'unavailable'}
  const references: Reference[] = []
  for (const text of unique) {
    if (!text.startsWith(source + '#')) {
      references.push({text, lines: []})
      continue
    }
    const anchor = /^#L([1-9]\d{0,6})(?:-L([1-9]\d{0,6}))?$/.exec(text.slice(source.length))
    const lines = anchor ? readRange(log, Number(anchor[1]), Number(anchor[2] ?? anchor[1])) : []
    references.push({text, lines})
  }
  return {
    status: references.length === 0 ? 'missing' : references.some(item => !item.lines.length) ? 'issues' : 'matched',
    references,
    repair: repairBareReferences(draft, source, log),
  }
}

function repairBareReferences(draft: string, source: string, log: Map<number, LogLine>) {
  const replacements = new Map<string, ReferenceRepair>()
  let fence: {marker: string; length: number} | undefined
  let length = draft.length
  let overflow = false
  const lines = draft.split('\n').map(line => {
    if (overflow) return line
    const marker = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line)
    if (marker) {
      if (!fence) fence = {marker: marker[1][0], length: marker[1].length}
      else if (fence.marker === marker[1][0] && marker[1].length >= fence.length && !marker[2].trim()) fence = undefined
      return line
    }
    if (fence || line.includes('`') || /^(?: {4}|\t)/.test(line)) return line
    return line.replace(referenceToken, (token, index: number) => {
      if (overflow) return token
      const anchor = /^#L([1-9]\d{0,6})(?:-#?L([1-9]\d{0,6}))?$/.exec(token)
      if (!anchor) return token
      const before = line[index - 1]
      const after = line[index + token.length]
      if ((before && !/[\s(]/.test(before)) || (after && !/[\s),.;:!?]/.test(after))) return token
      if (!readRange(log, Number(anchor[1]), Number(anchor[2] ?? anchor[1])).length) return token
      const full = source + '#L' + anchor[1] + (anchor[2] ? '-L' + anchor[2] : '')
      length += full.length - token.length
      if (length > maxDraftLength) {
        overflow = true
        return token
      }
      replacements.set(token, {before: token, after: full})
      return full
    })
  })
  return !overflow && replacements.size ? {draft: lines.join('\n'), replacements: [...replacements.values()]} : undefined
}

function validSource(source: string): boolean {
  if (!/^https:\/\/[^\s<>()`\[\]"'#\\]+$/u.test(source)) return false
  try {
    const url = new URL(source)
    return Boolean(url.hostname && !url.username && !url.password && !url.hash)
  } catch {
    return false
  }
}

function readRange(log: Map<number, LogLine>, first: number, last: number): LogLine[] {
  if (last < first || last - first >= 32) return []
  const lines: LogLine[] = []
  for (let number = first; number <= last; number++) {
    const line = log.get(number)
    if (!line) return []
    lines.push(line)
  }
  return lines
}
