// SPDX-License-Identifier: Elastic-2.0
// Current wire shapes for pre-existing intercepted UI scenarios, not live evidence.
export async function replyEventFixture(route,task){
 const p=new URL(route.request().url()).pathname
 const headers={'X-Platform-API-Version':'0.13.0'},window={after_sequence:'0',through_sequence:'0',version:1}
 if(p.endsWith('/event-snapshot')){await route.fulfill({headers,json:{data:{organization_id:task.organization_id,task_id:task.id,task,events:[],cursor:'fixture_head',replay_cursor:'fixture_floor',window,has_earlier_events:false,observed_at:new Date().toISOString()}}});return true}
 if(p.endsWith('/event-stream')){const batch={organization_id:task.organization_id,task_id:task.id,events:[],cursor:'fixture_head',has_more:false,reset_required:false,window};await route.fulfill({headers,contentType:'text/event-stream',body:`event: platform.events\nid: fixture_head\ndata: ${JSON.stringify({frame_version:1,kind:'batch',batch})}\n\n`});return true}
 return false
}
