// SPDX-License-Identifier: Elastic-2.0
import type {CurrentIdentity, Material} from './api'

export interface DraftScope {server:string;principal:string;organization:string}
export interface LocalDraft {materialId?:string;baseVersion?:number;baseDigest?:string;name:string;mediaType:Material['media_type'];content:string;reason:string;baseCandidateId?:string}
const MAX_ENTRIES=8,MAX_ENTRY_BYTES=262*1024
const key=(scope:DraftScope,task:string,material?:string)=>JSON.stringify([scope.server,scope.principal,scope.organization,task,material??null])
export function sameIdentity(scope:DraftScope,identity:CurrentIdentity){return identity.principal_id===scope.principal&&identity.organization_id===scope.organization}

/** Bounded volatile memory only. Callers must reauthorize before exposing a value. */
export class DraftVault {
  private entries=new Map<string,LocalDraft>()
  get size(){return this.entries.size}
  put(scope:DraftScope,task:string,value:LocalDraft){
    if(!scope.server||!scope.principal||!scope.organization||!task)throw new Error('Не подтверждена область локального черновика.')
    if(new TextEncoder().encode(value.content).length>262144||new TextEncoder().encode(value.reason).length>4096||new TextEncoder().encode(value.name).length>120||Object.values(value).reduce((sum,item)=>sum+(typeof item==='string'?new TextEncoder().encode(item).length:16),0)>MAX_ENTRY_BYTES)throw new Error('Локальный черновик превысил предел памяти. Скопируйте текст и сократите его перед продолжением.')
    const id=key(scope,task,value.materialId)
    if(!this.entries.has(id)&&this.entries.size>=MAX_ENTRIES)throw new Error('В памяти уже 8 несохранённых материалов. Сохраните или явно удалите один из них. Текст не вытесняется автоматически.')
    this.entries.set(id,{...value})
  }
  read(scope:DraftScope,task:string,material?:string){const value=this.entries.get(key(scope,task,material));return value?{...value}:undefined}
  drop(scope:DraftScope,task:string,material?:string){this.entries.delete(key(scope,task,material))}
  clear(){this.entries.clear()}
}
