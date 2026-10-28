// SPDX-License-Identifier: Elastic-2.0
import type {Event,TaskEventBatch,TaskEventSnapshot} from '../../packages/typescript-sdk/src/types'
import {compareEventSequences} from '../../packages/typescript-sdk/src/events.ts'

const stable=(value:unknown):string=>JSON.stringify(value&&typeof value==='object'&&!Array.isArray(value)?Object.fromEntries(Object.entries(value).sort(([a],[b])=>a.localeCompare(b)).map(([key,item])=>[key,JSON.parse(stable(item))])):Array.isArray(value)?value.map(item=>JSON.parse(stable(item))):value)

/** Bounded display cache; neither events nor cursors authorize a mutation. */
export class EventJournal{
 private values:Event[]=[]
 private high='0'
 cursor=''
 truncated=false
 private readonly limit:number
 constructor(limit=300){this.limit=limit;if(!Number.isInteger(limit)||limit<1||limit>1000)throw new Error('Invalid journal limit')}
 snapshot(value:TaskEventSnapshot):Event[]{this.values=value.events.slice(-this.limit);this.high=value.window.through_sequence;this.cursor=value.cursor;this.truncated=value.has_earlier_events||value.events.length>this.limit;return this.current()}
 batch(value:TaskEventBatch):Event[]{
  if(value.reset_required)throw new Error('History reset must be acknowledged explicitly')
  const merged=[...this.values],bySequence=new Map(merged.map(event=>[event.sequence,event])),byId=new Map(merged.map(event=>[event.id,event]))
  let high=this.high
  for(const event of value.events){
   const sequence=bySequence.get(event.sequence),identifier=byId.get(event.id)
   if(sequence||identifier){if(!sequence||!identifier||stable(sequence)!==stable(event)||stable(identifier)!==stable(event))throw new Error('Conflicting duplicate event');continue}
   if(compareEventSequences(event.sequence,high)<=0)throw new Error('Event moved backwards outside the retained cache')
   merged.push(event);bySequence.set(event.sequence,event);byId.set(event.id,event);high=event.sequence
  }
  this.truncated=this.truncated||merged.length>this.limit
  this.values=merged.slice(-this.limit);this.high=high;this.cursor=value.cursor
  return this.current()
 }
 current():Event[]{return [...this.values]}
}
export type EventConnectionState='connecting'|'live'|'reconnecting'|'reset_required'|'stopped'
export function eventReconnectDelay(attempt:number,random=Math.random):number{return Math.min(10_000,500*2**Math.min(5,Math.max(0,attempt)))+Math.floor(Math.min(1,Math.max(0,random()))*250)}
