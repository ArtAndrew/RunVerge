// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {modelTariffLabel,modelResourceLabel,validateModelProfile,unknownOperationCount,chooseModelSelection} from '../src/modelAccounting.ts'
const model={id:'local',model:'qwen-local',version:1,accounting_mode:'operator_unmetered_local',max_charge_microusd:0,max_input_bytes:16384,max_input_tokens:4096,max_output_tokens:256,max_output_bytes:16384,timeout_milliseconds:60000,price_version:'operator-zero',bound_assumptions:'Operator declaration, no hardware measurement',created_at:'2026-10-26T00:00:00Z'}
test('local tariff is an operator assumption and request limits do not claim hardware quotas',()=>{
 assert.match(modelTariffLabel(model),/администратором/);assert.match(modelTariffLabel(model),/не измерены/)
 assert.match(modelResourceLabel(model),/60 с/);assert.match(modelResourceLabel(model),/не квоты CPU\/GPU/)
 assert.match(modelTariffLabel({...model,accounting_mode:'metered',max_charge_microusd:150}),/резерв, не окончательный счёт/)
})
test('missing/unknown accounting and invalid resource bounds cannot display a free model',()=>{
 for(const patch of[{accounting_mode:undefined},{accounting_mode:'free'},{max_charge_microusd:1},{max_charge_microusd:false},{accounting_mode:'metered',max_charge_microusd:0},{timeout_milliseconds:0},{max_input_tokens:1.5},{max_output_bytes:262145}])assert.throws(()=>validateModelProfile({...model,...patch}))
})
test('uncertain operation count preserves bigint precision and is not inferred from money or rows',()=>{
 for(const value of['0','1','9007199254740993','9223372036854775807'])assert.equal(unknownOperationCount(value),value)
 for(const value of[undefined,null,0,false,'01','-1','0\n','9223372036854775808','1e2'])assert.throws(()=>unknownOperationCount(value))
})


test('first-use selection suggests only an explicit valid local model, never a paid model or fixture',()=>{
 const paid={...model,id:'paid',model:'paid-model',accounting_mode:'metered',max_charge_microusd:50}
 assert.equal(chooseModelSelection('',false,[]),'')
 assert.equal(chooseModelSelection('',false,[paid]),'')
 assert.equal(chooseModelSelection('',false,[paid,model]),model.model)
 assert.equal(chooseModelSelection('',false,[{...model,model:'fixture:echo'},{...model,model:'team:coordinator'}]),'')
 assert.equal(chooseModelSelection('',false,[{...model,model:' fixture:echo'},{...model,model:123},{...model,model:''}]),'')
 assert.throws(()=>chooseModelSelection('',false,[{...model,max_charge_microusd:1}]))
})

test('late catalogue pages preserve explicit fixture/paid choices and an already suggested local model',()=>{
 assert.equal(chooseModelSelection('fixture:echo',true,[model]),'fixture:echo')
 assert.equal(chooseModelSelection('paid-model',true,[model]),'paid-model')
 assert.equal(chooseModelSelection('',true,[model]),'')
 assert.equal(chooseModelSelection('first-local',false,[model]),'first-local')
 assert.equal(chooseModelSelection('',false,[model]),model.model)
})
