// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {DraftVault,sameIdentity} from '../src/draftVault.ts'
const scope={server:'https://platform.test/api/v1',principal:'principal',organization:'org'}
const draft={materialId:'material',baseVersion:1,baseDigest:'a'.repeat(64),name:'draft',mediaType:'text/plain',content:'Unsaved exact text\r\n',reason:'Edit'}
test('local recovery isolates endpoint principal organization task and material without credential keys',()=>{
 const vault=new DraftVault();vault.put(scope,'task',draft)
 for(const other of[{...scope,server:'https://other.test/api/v1'},{...scope,principal:'other'},{...scope,organization:'other'}])assert.equal(vault.read(other,'task','material'),undefined)
 assert.equal(vault.read(scope,'other','material'),undefined);assert.equal(vault.read(scope,'task','other'),undefined);assert.equal(vault.read(scope,'task'),undefined)
 const value=vault.read(scope,'task','material');assert.deepEqual(value,draft);value.content='altered';assert.equal(vault.read(scope,'task','material').content,draft.content)
 assert.equal(sameIdentity(scope,{principal_id:'principal',organization_id:'org'}),true);assert.equal(sameIdentity(scope,{principal_id:'other',organization_id:'org'}),false)
})
test('eight buffers are retained without eviction and explicit removal frees capacity',()=>{
 const vault=new DraftVault();for(let i=0;i<8;i++)vault.put(scope,'task'+i,draft)
 assert.throws(()=>vault.put(scope,'ninth',draft),/8/);assert.equal(vault.size,8);for(let i=0;i<8;i++)assert.deepEqual(vault.read(scope,'task'+i,'material'),draft)
 vault.put(scope,'task0',{...draft,content:'Updated'});assert.equal(vault.size,8)
 vault.drop(scope,'task0','material');vault.put(scope,'ninth',draft);assert.equal(vault.size,8);vault.clear();assert.equal(vault.size,0)
})
test('UTF8 bounds reject before replacing an existing buffer and new draft has a distinct key',()=>{
 const vault=new DraftVault();vault.put(scope,'task',draft)
 for(const change of[{content:'я'.repeat(131073)},{reason:'я'.repeat(2049)},{name:'я'.repeat(61)}])assert.throws(()=>vault.put(scope,'task',{...draft,...change}))
 assert.equal(vault.read(scope,'task','material').content,draft.content)
 vault.put(scope,'task',{...draft,materialId:undefined,content:'New draft'});assert.equal(vault.read(scope,'task').content,'New draft');assert.equal(vault.size,2)
 vault.put(scope,'limit',{...draft,content:'\n'.repeat(262144),reason:'x'.repeat(4096)});assert.equal(vault.read(scope,'limit','material').content.length,262144)
})
