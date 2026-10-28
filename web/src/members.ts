// SPDX-License-Identifier: Elastic-2.0
import type {CurrentIdentity,MembershipRole,OrganizationMember,MembershipCapabilities,MembershipAuditEntry} from '../../packages/typescript-sdk/src/types'
export type {MembershipRole,OrganizationMember,MembershipCapabilities,MembershipAuditEntry}
export interface CreateMember {display_name:string;role:MembershipRole;reason:string}
export interface UpdateMember extends CreateMember {expected_version:number;active:boolean}
export interface RevokeMemberCredentials {expected_version:number;reason:string}
export const membershipRoles:Record<MembershipRole,string>={owner:'Владелец',admin:'Администратор',member:'Участник',viewer:'Просмотр'}
const record=(v:unknown):v is Record<string,unknown>=>Boolean(v&&typeof v==='object'&&!Array.isArray(v))
const role=(v:unknown):v is MembershipRole=>typeof v==='string'&&Object.hasOwn(membershipRoles,v)
const positive=(v:unknown)=>typeof v==='number'&&Number.isSafeInteger(v)&&v>=1
export function memberText(value:unknown,maximum:number):value is string {
 if(typeof value!=='string'||!value.trim()||value.includes('\0'))return false
 for(const character of value){const cp=character.codePointAt(0)!;if(cp>=0xd800&&cp<=0xdfff)return false}
 return new TextEncoder().encode(value).length<=maximum
}
function auditState(v:unknown){return record(v)&&memberText(v.display_name,120)&&role(v.role)&&typeof v.active==='boolean'&&positive(v.version)}
export function validMember(v:unknown,organization?:string):v is OrganizationMember{return auditState(v)&&record(v)&&memberText(v.organization_id,128)&&(!organization||v.organization_id===organization)&&memberText(v.principal_id,128)&&typeof v.principal_active==='boolean'&&memberText(v.created_at,128)&&memberText(v.updated_at,128)&&(v.credentials_revoked_at===undefined||memberText(v.credentials_revoked_at,128))&&Array.isArray(v.available_commands)&&v.available_commands.every(c=>typeof c==='string')&&Array.isArray(v.assignable_roles)&&v.assignable_roles.every(role)}
export function validMembershipCapabilities(v:unknown,organization?:string):v is MembershipCapabilities{return record(v)&&memberText(v.organization_id,128)&&(!organization||v.organization_id===organization)&&typeof v.can_create_members==='boolean'&&v.self_management_allowed===false&&Array.isArray(v.assignable_roles)&&v.assignable_roles.every(role)}
export function validMembershipAudit(v:unknown,organization?:string):v is MembershipAuditEntry{return record(v)&&memberText(v.id,128)&&memberText(v.organization_id,128)&&(!organization||v.organization_id===organization)&&memberText(v.actor_id,128)&&memberText(v.target_principal_id,128)&&['member.created','member.updated','member.credentials_revoked'].includes(v.action as string)&&memberText(v.reason,4096)&&memberText(v.created_at,128)&&auditState(v.after)&&(v.before===undefined||auditState(v.before))}
export function canAdminMembers(identity:CurrentIdentity|null){return Boolean(identity&&identity.principal_kind==='human'&&['owner','admin'].includes(identity.role)&&identity.permissions.includes('identity.manage'))}
export function canManageMember(identity:CurrentIdentity|null,member:OrganizationMember,command:'update'|'revoke_credentials'){return Boolean(canAdminMembers(identity)&&identity&&member.principal_kind==='human'&&identity.organization_id===member.organization_id&&identity.principal_id!==member.principal_id&&(identity.role==='owner'||member.role!=='owner')&&member.available_commands.includes(command))}
export function memberHasChanges(member:OrganizationMember,input:Pick<UpdateMember,'display_name'|'role'|'active'>){return member.display_name!==input.display_name||member.role!==input.role||member.active!==input.active}
export function createMemberPayload(display_name:string,selectedRole:MembershipRole,reason:string,allowed:MembershipRole[]):CreateMember {
 if(!memberText(display_name,120)||!memberText(reason,4096)||!role(selectedRole)||!allowed.includes(selectedRole))throw new Error('Проверьте имя, доступную роль и причину. Имя — до 120 байт, причина — до 4096 байт UTF-8.')
 return {display_name,role:selectedRole,reason}
}
export function updateMemberPayload(member:OrganizationMember,display_name:string,selectedRole:MembershipRole,active:boolean,reason:string):UpdateMember {
 const payload=createMemberPayload(display_name,selectedRole,reason,member.assignable_roles)
 if(typeof active!=='boolean'||!positive(member.version))throw new Error('Не удалось проверить версию и состояние участника.')
 if(!memberHasChanges(member,{display_name,role:selectedRole,active}))throw new Error('Измените имя, роль или состояние участника перед сохранением.')
 return {...payload,active,expected_version:member.version}
}
