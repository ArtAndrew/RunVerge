// SPDX-License-Identifier: Apache-2.0
/** Explicit steps for an already authenticated client; no automatic approvals,
 * dispatch, retries or continuation. The caller inspects every exact result
 * before choosing continueInspectedBatch and its additional model cost. */
import type {Client,ModelToolProposal,ModelToolResultBinding} from '../../packages/typescript-sdk/src/index.ts';

export async function prepareExactProposal(client:Client,taskId:string,proposalId:string,requestKey:string){
 const task=await client.getTask(taskId),proposal=await client.getModelProposal(taskId,proposalId);
 if(task.candidate_id!==proposal.candidate_id||proposal.arguments_json===undefined)throw new Error('Inspect the current exact proposal again');
 return client.prepareToolExecution(taskId,{occurrenceId:`model:${proposal.id}`,modelProposalId:proposal.id,
  expectedTaskVersion:task.version,candidateId:proposal.candidate_id,artifactId:proposal.artifact_id,
  artifactVersion:proposal.artifact_version,contentDigest:proposal.content_digest,toolId:proposal.tool_id,
  toolVersion:proposal.tool_version,argumentsJson:proposal.arguments_json,expiresInSeconds:900},{idempotencyKey:requestKey});
}

export async function continueInspectedBatch(client:Client,taskId:string,reason:string,requestKey:string){
 const task=await client.getTask(taskId),proposals:ModelToolProposal[]=[],seen=new Set<string>();let cursor:string|undefined;
 do{const page=await client.modelProposals(taskId,cursor?{cursor}:{});proposals.push(...page.data.filter(p=>p.candidate_id===task.candidate_id));cursor=page.next_cursor;if(cursor){if(seen.has(cursor)||seen.size>=128)throw new Error('Invalid or unbounded pagination');seen.add(cursor);}}while(cursor);
 const source=proposals[0];if(!source)throw new Error('No current model-tool batch');
 const results:ModelToolResultBinding[]=[];
 for(const proposal of proposals){
  if(!proposal.execution_id)throw new Error('Prepare and explicitly resolve every proposal first');
  const view=await client.getToolExecution(taskId,proposal.execution_id),execution=view.execution;
  if(execution.status==='succeeded'||execution.status==='rejected'){
   if(!view.receipt)throw new Error('Exact tool receipt is required');
   results.push({proposal_id:proposal.id,execution_id:execution.id,execution_version:execution.version,status:execution.status,receipt_id:view.receipt.id,content_digest:view.receipt.content_digest});
  }else if(execution.status==='denied'||execution.status==='expired'||execution.status==='cancelled'){
   results.push({proposal_id:proposal.id,execution_id:execution.id,execution_version:execution.version,status:execution.status});
  }else throw new Error('Unknown or incomplete effect blocks continuation');
 }
 return client.continueModel(taskId,{expectedTaskVersion:task.version,candidateId:source.candidate_id,artifactId:source.artifact_id,artifactVersion:source.artifact_version,contentDigest:source.content_digest,results,acknowledgeCost:true,reason},{idempotencyKey:requestKey});
}
