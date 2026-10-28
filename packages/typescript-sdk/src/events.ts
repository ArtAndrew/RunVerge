// SPDX-License-Identifier: Apache-2.0
import { ProtocolError, TransportError } from "./errors.ts";
import { EVENT_PAYLOAD_REGISTRY } from "./eventPayloads.ts";
import type { Event, EventReplayWindow, TaskEventBatch, TaskEventSnapshot, TaskEventStreamFrame } from "./types.js";

export const API_VERSION = "0.14.0";
export const API_VERSION_HEADER = "X-Platform-API-Version";
export const MAX_EVENT_FRAME_BYTES = 65_536;
const MAX_SEQUENCE = "9223372036854775807";
const object = (value: unknown): value is Record<string, unknown> => !!value && typeof value === "object" && !Array.isArray(value);
function fail(): never { throw new ProtocolError("Event response does not match the supported contract"); }
const nonempty = (value: unknown): value is string => typeof value === "string" && value.length > 0 && !/[\u0000-\u001f]/.test(value);
const exact = (value: Record<string, unknown>, fields: string[]): void => { if (Object.keys(value).some(key => !fields.includes(key))) fail(); };
export function eventTimestamp(value:unknown):value is string{
  if(typeof value!=='string')return false;
  const match=/^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.\d{1,9})?(?:Z|([+-])(\d\d):(\d\d))$/.exec(value);
  if(!match)return false;
  const year=Number(match[1]),month=Number(match[2]),day=Number(match[3]);
  const days=[31,(year%4===0&&(year%100!==0||year%400===0))?29:28,31,30,31,30,31,31,30,31,30,31];
  return year>=1&&month>=1&&month<=12&&day>=1&&day<=(days[month-1]??0)&&Number(match[4])<=23&&Number(match[5])<=59&&Number(match[6])<=59&&(!match[7]||(Number(match[8])<=23&&Number(match[9])<=59));
}

export function eventSequence(value: unknown, allowZero = false): string {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value) || value.length > 19 ||
      (value.length === 19 && value > MAX_SEQUENCE) || (!allowZero && value === "0")) fail();
  return value as string;
}
export function compareEventSequences(left: string, right: string): number {
  eventSequence(left, true); eventSequence(right, true);
  return left.length === right.length ? (left === right ? 0 : left < right ? -1 : 1) : left.length < right.length ? -1 : 1;
}
export function eventCursor(value: unknown, allowEmpty = false): string {
  if (typeof value !== "string" || value.length > 1024 || (value === "" ? !allowEmpty : !/^[A-Za-z0-9_-]+$/.test(value))) fail();
  return value as string;
}
export function checkAPIVersion(headers: Pick<Headers, "get">): void {
  if (headers.get(API_VERSION_HEADER) !== API_VERSION) throw new ProtocolError("Incompatible API wire version; commands must not be retried automatically");
}

export function validateEvent(value: unknown, organizationId: string, taskId: string): Event {
  if (!object(value)) fail();
  if (!nonempty(value.id) || value.organization_id !== organizationId || value.task_id !== taskId ||
      !nonempty(value.type) || !eventTimestamp(value.created_at)) fail();
  eventSequence(value.sequence);
  if (value.payload_version === 0) {
    exact(value, ["id", "organization_id", "task_id", "sequence", "type", "created_at", "payload_version", "legacy_data_json"]);
    if (typeof value.legacy_data_json !== "string"||!value.legacy_data_json) fail();
  } else if (value.payload_version === 1) {
    exact(value, ["id", "organization_id", "task_id", "sequence", "type", "created_at", "payload_version", "data"]);
    if (!object(value.data)) fail();
    // The public payload registry validates discriminators and individual fields.
    validatePayload(value.type as string, value.data);
  } else fail();
  return value as unknown as Event;
}

// Public protocol declarations are independently licensed from the server implementation.
interface Field {kind:string;required:boolean;max_bytes?:number;allow_empty?:boolean;minimum?:number;maximum?:number;maximum_decimal?:string;const?:unknown;enum?:readonly string[]}
export function validatePayload(type: string, data: Record<string, unknown>): void {
  const definitions:Record<string,{fields:Record<string,Field>;variants?:readonly {field:string;value:string;required?:readonly string[];forbidden?:readonly string[]}[]}> = EVENT_PAYLOAD_REGISTRY.types;
  const definition = Object.hasOwn(definitions,type)?definitions[type]:undefined;
  if (!definition || Object.keys(data).some(key => !Object.hasOwn(definition.fields,key))) fail();
  for (const [name, field] of Object.entries(definition.fields)) {
    if (!Object.hasOwn(data,name)) { if (field.required) fail(); continue; }
    const value=data[name];
    if (field.kind==='boolean') { if(typeof value!=='boolean')fail(); }
    else if(field.kind==='integer') { if(typeof value!=='number'||!Number.isSafeInteger(value)||value<(field.minimum??0)||value>(field.maximum??2147483647))fail(); }
    else if(field.kind==='decimal') { const decimal=eventSequence(value,true);if(BigInt(decimal)<BigInt(field.minimum??0)||BigInt(decimal)>BigInt(field.maximum_decimal??MAX_SEQUENCE))fail(); }
    else if(typeof value!=='string'||(!field.allow_empty&&!value.trim())||/[\u0000]/.test(value)||new TextEncoder().encode(value).length>(field.max_bytes??65536)||/[\uD800-\uDFFF]/u.test(value.replace(/[\uD800-\uDBFF][\uDC00-\uDFFF]/g,'')))fail();
    else if(field.kind==='digest'&&!/^[0-9a-f]{64}$/.test(value))fail();
    else if(field.kind==='timestamp'&&!eventTimestamp(value))fail();
    if(Object.hasOwn(field,'const')&&value!==field.const)fail();
    if(field.enum&&!field.enum.includes(value as string))fail();
  }
  if(definition.variants){let matched=false;for(const variant of definition.variants){if(data[variant.field]!==variant.value)continue;matched=true;if(variant.required?.some(name=>!Object.hasOwn(data,name))||variant.forbidden?.some(name=>Object.hasOwn(data,name)))fail();}if(!matched)fail();}
}

function windowFacts(value: unknown): EventReplayWindow {
  if (!object(value)) fail();
  exact(value, ["after_sequence", "through_sequence", "version"]);
  if (typeof value.version!=='number'||!Number.isInteger(value.version)||value.version<1||value.version>2147483647) fail();
  if (compareEventSequences(eventSequence(value.after_sequence, true), eventSequence(value.through_sequence, true)) > 0) fail();
  return value as unknown as EventReplayWindow;
}
function eventList(value: unknown, organizationId: string, taskId: string, window?: EventReplayWindow): Event[] {
  if (!Array.isArray(value) || value.length > 100) fail();
  const events = (value as unknown[]).map(v => validateEvent(v, organizationId, taskId));
  const ids = new Set<string>(); let previous = "0";
  for (const event of events) {
    if (ids.has(event.id) || compareEventSequences(event.sequence, previous) <= 0 ||
        (window && (compareEventSequences(event.sequence, window.after_sequence) <= 0 || compareEventSequences(event.sequence, window.through_sequence) > 0))) fail();
    ids.add(event.id); previous = event.sequence;
  }
  return events;
}
export function validateEventBatch(value: unknown, organizationId: string, taskId: string): TaskEventBatch {
  if (!object(value)) fail();
  exact(value, ["organization_id", "task_id", "events", "cursor", "has_more", "reset_required", "reset_reason", "window"]);
  if (value.organization_id !== organizationId || value.task_id !== taskId || typeof value.has_more !== "boolean" || typeof value.reset_required !== "boolean") fail();
  const window = windowFacts(value.window);
  const events = eventList(value.events, organizationId, taskId, window);
  if (value.reset_required) {
    if (events.length || value.cursor !== "" || value.reset_reason !== "history_expired" || value.has_more) fail();
  } else {
    eventCursor(value.cursor);
    if (value.reset_reason !== undefined) fail();
  }
  return value as unknown as TaskEventBatch;
}
export function validateEventSnapshot(value: unknown, organizationId: string, taskId: string): TaskEventSnapshot {
  if (!object(value)) fail();
  exact(value, ["organization_id", "task_id", "task", "events", "cursor", "replay_cursor", "window", "has_earlier_events", "observed_at"]);
  if (value.organization_id !== organizationId || value.task_id !== taskId || !object(value.task) || value.task.organization_id !== organizationId || value.task.id !== taskId ||
      typeof value.has_earlier_events !== "boolean" || !eventTimestamp(value.observed_at)) fail();
  eventCursor(value.cursor); eventCursor(value.replay_cursor);
  // This new read must preserve the same factual money contract as GET /tasks/{id}.
  eventSequence(value.task.reserved_microusd,true);eventSequence(value.task.spent_microusd,true);
  const window = windowFacts(value.window);
  if (eventList(value.events, organizationId, taskId, window).length > 50) fail();
  return value as unknown as TaskEventSnapshot;
}

/** One connection parser. An incomplete frame never produces a resumable cursor. */
export class EventStreamParser {
  private line: number[] = [];
  private frameBytes = 0;
  private skipLF = false;
  private event: string | undefined;
  private id: string | undefined;
  private data: string[] = [];
  private readonly organizationId: string;
  private readonly taskId: string;
  constructor(organizationId: string, taskId: string) { this.organizationId=organizationId; this.taskId=taskId; }
  push(chunk: Uint8Array): TaskEventStreamFrame[] {
    const frames: TaskEventStreamFrame[] = [];
    for (const byte of chunk) {
      if (this.skipLF) { this.skipLF = false; if (byte === 10) {if(this.frameBytes&&++this.frameBytes>MAX_EVENT_FRAME_BYTES)throw new ProtocolError("SSE frame exceeds 64 KiB");continue;} }
      if (++this.frameBytes > MAX_EVENT_FRAME_BYTES) throw new ProtocolError("SSE frame exceeds 64 KiB");
      if(byte===13&&!this.line.length&&this.frameBytes===MAX_EVENT_FRAME_BYTES)throw new ProtocolError("SSE frame exceeds 64 KiB including CRLF");
      if (byte === 10 || byte === 13) {
        const frame = this.endLine(); if (frame) frames.push(frame);
        if (byte === 13) this.skipLF = true;
      } else this.line.push(byte);
    }
    return frames;
  }
  finish(): void {
    if (this.line.length || this.event !== undefined || this.id !== undefined || this.data.length) throw new ProtocolError("SSE connection ended inside a frame; retain the previous cursor");
  }
  private endLine(): TaskEventStreamFrame | undefined {
    let line: string;
    try { line = new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array(this.line)); } catch { throw new ProtocolError("SSE is not valid UTF-8"); }
    this.line = [];
    if (!line) {
      const event = this.event, id = this.id, data = this.data;
      this.event = undefined; this.id = undefined; this.data = []; this.frameBytes = 0;
      if (event === undefined && id === undefined && !data.length) return;
      if (!event || !data.length) fail();
      const value = parseEventJSON(data.join("\n"));
      if (!object(value)) fail();
      exact(value, ["frame_version", "kind", "batch", "error"]);
      if (value.frame_version !== 1) fail();
      if (event === "platform.error" && value.kind === "error") {
        if (id !== undefined || value.batch !== undefined || !object(value.error)) fail();
        exact(value.error, ["code", "message", "retryable"]);
        if (!nonempty(value.error.code) || typeof value.error.message !== "string" || typeof value.error.retryable !== "boolean") fail();
      } else {
        const batch = validateEventBatch(value.batch, this.organizationId, this.taskId);
        if (value.error !== undefined) fail();
        if (batch.reset_required ? (event !== "platform.reset" || value.kind !== "reset_required" || id !== undefined) : (event !== "platform.events" || value.kind !== "batch" || id !== batch.cursor)) fail();
      }
      return value as unknown as TaskEventStreamFrame;
    }
    if (line.startsWith(":")) return;
    const colon = line.indexOf(":");
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1); if (value.startsWith(" ")) value = value.slice(1);
    if (field === "data") this.data.push(value);
    else if (field === "event" && this.event === undefined) this.event = value;
    else if (field === "id" && this.id === undefined) this.id = eventCursor(value);
    else fail();
  }
}

export function parseEventJSON(text: string): unknown {
  let nodes = 0, position=0;
  const whitespace=():void=>{while(/[\t\n\r ]/.test(text[position]??'!'))position++;};
  const string=():string=>{const start=position++;let escaped=false;while(position<text.length){const char=text[position++];if(!escaped&&char==='"'){const value:unknown=JSON.parse(text.slice(start,position));if(typeof value!=='string')fail();return value;}if(!escaped&&char==='\\')escaped=true;else escaped=false;}fail();};
  const scan=(depth:number):void=>{
    if(depth>32||++nodes>10_000)fail();whitespace();const char=text[position];
    if(char==='{'){
      position++;whitespace();const keys=new Set<string>();if(text[position]==='}'){position++;return;}
      while(true){if(text[position]!=='"')fail();const key=string();if(keys.has(key))fail();keys.add(key);whitespace();if(text[position++]!==':')fail();scan(depth+1);whitespace();const delimiter=text[position++];if(delimiter==='}')return;if(delimiter!==',')fail();whitespace();}
    }else if(char==='['){position++;whitespace();if(text[position]===']'){position++;return;}while(true){scan(depth+1);whitespace();const delimiter=text[position++];if(delimiter===']')return;if(delimiter!==',')fail();}}
    else if(char==='"'){string();}
    else if(text.startsWith('true',position))position+=4;
    else if(text.startsWith('false',position))position+=5;
    else if(text.startsWith('null',position))position+=4;
    else{const number=/^-?(?:0|[1-9][0-9]*)/.exec(text.slice(position));if(!number||number[0].length>16||!Number.isSafeInteger(Number(number[0])))fail();position+=number[0].length;}
  };
  try {
    scan(0);whitespace();if(position!==text.length)fail();
    return JSON.parse(text) as unknown;
  } catch (error) { if (error instanceof ProtocolError) throw error; throw new ProtocolError("SSE frame is not valid JSON"); }
}

/** Reads one bounded server connection. Closing the iterator cancels its reader. */
export async function* readEventFrames(response: Response, organizationId: string, taskId: string, options: { signal?: AbortSignal; idleTimeoutMs?: number } = {}): AsyncGenerator<TaskEventStreamFrame> {
  checkAPIVersion(response.headers);
  if (!response.ok || response.headers.get("Content-Type")?.split(";")[0]?.trim().toLowerCase() !== "text/event-stream" || !response.body) throw new ProtocolError("Expected a successful SSE response");
  const idle = options.idleTimeoutMs ?? 30_000;
  if (!Number.isSafeInteger(idle) || idle < 1 || idle > 2_147_483_647) throw new TypeError("idleTimeoutMs must be a positive bounded integer");
  const reader = response.body.getReader();
  const parser = new EventStreamParser(organizationId, taskId);
  let aborted = false;
  const onAbort = (): void => { aborted = true; void reader.cancel(); };
  options.signal?.addEventListener("abort", onAbort, { once: true });
  if (options.signal?.aborted) onAbort();
  try {
    while (true) {
      if (aborted) throw new TransportError("aborted");
      let timer: ReturnType<typeof setTimeout> | undefined;
      let timedOut=false;
      let item: ReadableStreamReadResult<Uint8Array>;
      try {
        item = await Promise.race([reader.read(), new Promise<never>((_resolve, reject) => { timer = setTimeout(() => { timedOut=true; reject(new TransportError("timeout")); void reader.cancel(); }, idle); })]);
      } finally { clearTimeout(timer); }
      if (aborted) throw new TransportError("aborted");
      if(timedOut)throw new TransportError("timeout");
      if (item.done) { parser.finish(); return; }
      for (const frame of parser.push(item.value)) {
        yield frame;
        if (frame.kind !== "batch") return;
      }
    }
  } finally {
    options.signal?.removeEventListener("abort", onAbort);
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}
