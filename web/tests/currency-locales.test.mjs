// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {formatExactUSD,parseBudget} from '../src/format.ts'

test('all planned locale currency layouts retain exact fractional and huge values',()=>{
 for(const locale of ['ru-RU','en-US','es-ES','pt-BR','de-DE','fr-FR','zh-CN']){
  for(const amount of [0n,1n,-1n,123456789n,-123456789n,9223372036854775807n]){
   const text=formatExactUSD(amount,locale)
   const digits=text.replace(/[^0-9]/g,'')
   const abs=amount<0n?-amount:amount
   const fraction=(abs%1000000n).toString().padStart(6,'0').replace(/0+$/,'').padEnd(2,'0')
   assert.equal(digits,(abs/1000000n).toString()+fraction,locale)
   assert.equal(text.includes('-'),amount<0n,locale)
  }
 }
 assert.equal(formatExactUSD(123456789n,'en-US'),'$123.456789')
 assert.equal(formatExactUSD(123456789n,'de-DE'),'123,456789\u00a0$')
 assert.equal(formatExactUSD(-1n,'zh-CN'),'-US$0.000001')
 assert.equal(parseBudget('1,234567'),1234567)
 assert.throws(()=>parseBudget('1.234,56'))
})
