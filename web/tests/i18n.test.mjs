// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {resolveLocale, setLocale, getLocale, getIntlLocale, subscribeLocale, localeStorageKey, supportedLocales, t} from '../src/i18n.ts'
import {money, parseBudget, dateTime, errorMessage} from '../src/format.ts'

test('locale selection honors safe saved choice and browser preferences, exposing only complete locales',async()=>{
 assert.deepEqual(supportedLocales.map(item=>item.code),['ru','en','es','pt-BR','de','fr','zh-CN'])
 assert.equal(resolveLocale('ru',['en-US']),'ru')
 assert.equal(resolveLocale('en',['ru-RU']),'en')
 assert.equal(resolveLocale('zh',['de-DE','ru-RU']),'de')
 assert.equal(resolveLocale('<script>',['EN-us']),'en')
 assert.equal(resolveLocale(null,['es-MX']),'es')
 assert.equal(resolveLocale(null,[]),'en')
 assert.equal(resolveLocale(null,['pt-PT']),'pt-BR')
 assert.equal(resolveLocale(null,['zh-Hans-SG']),'zh-CN')
 assert.equal(resolveLocale(null,['zh-TW','fr-CA']),'fr')
 assert.equal(resolveLocale(null,['zh-Hant-CN']),'en')
 assert.equal(resolveLocale('zh-CN',['de-DE']),'zh-CN')
})

test('switching notifies subscribers, updates document language and stores only the locale',async()=>{
 const previousWindow=Object.getOwnPropertyDescriptor(globalThis,'window')
 const previousDocument=Object.getOwnPropertyDescriptor(globalThis,'document')
 const writes=[],seen=[]
 Object.defineProperty(globalThis,'window',{configurable:true,value:{localStorage:{setItem:(...args)=>writes.push(args)}}})
 Object.defineProperty(globalThis,'document',{configurable:true,value:{documentElement:{lang:'ru'}}})
 await setLocale('ru')
 const unsubscribe=subscribeLocale(()=>seen.push(getLocale()))
 try{
  assert.equal(await setLocale('en'),true)
  assert.equal(document.documentElement.lang,'en')
  assert.equal(getIntlLocale(),'en-US')
  assert.equal(t('Подключить пространство'),'Connect workspace')
  assert.equal(t('Исполнитель'),'Agent')
  assert.equal(await setLocale('xx'),false)
  assert.equal(getLocale(),'en')
  assert.equal(await setLocale('en'),true)
  assert.deepEqual(seen,['en'])
  assert.ok(writes.every(([key,value])=>key===localeStorageKey&&['en','ru'].includes(value)))
  unsubscribe();await setLocale('ru');assert.deepEqual(seen,['en'])
 }finally{
  unsubscribe();await setLocale('ru')
  if(previousWindow)Object.defineProperty(globalThis,'window',previousWindow);else delete globalThis.window
  if(previousDocument)Object.defineProperty(globalThis,'document',previousDocument);else delete globalThis.document
 }
})

test('blocked storage still permits switching; placeholder values are inserted once without translation or HTML interpretation',async()=>{
 const previous=Object.getOwnPropertyDescriptor(globalThis,'window')
 Object.defineProperty(globalThis,'window',{configurable:true,value:{localStorage:{setItem(){throw new Error('blocked')}}}})
 try{
  assert.equal(await setLocale('en'),true)
  assert.equal(t('Открыть задачу: {value0}',{value0:'Задачи <b>{value0}</b>'}),'Open task: Задачи <b>{value0}</b>')
  assert.equal(t('unknown_enum'),'unknown_enum')
  assert.equal(t('__proto__'),'__proto__')
  assert.equal(t('Открыть задачу: {value0}'),'Open task: {value0}')
 }finally{await setLocale('ru');if(previous)Object.defineProperty(globalThis,'window',previous);else delete globalThis.window}
})

test('locale formatting preserves exact micro-USD arithmetic and unchanged budget parsing',async()=>{
 try{
  await setLocale('en')
  assert.equal(money('9223372036854775807'),'$9,223,372,036,854.775807')
  assert.equal(money('-1'),'-$0.000001')
  assert.equal(money('1000000'),'$1.00')
  assert.equal(parseBudget('1,000001'),1000001)
  assert.equal(parseBudget('1.000001'),1000001)
  assert.equal(money('01'),'No data')
  const timestamp='2026-10-28T10:00:00Z'
  assert.equal(dateTime(timestamp),new Intl.DateTimeFormat('en-US',{day:'numeric',month:'short',hour:'2-digit',minute:'2-digit'}).format(new Date(timestamp)))
  assert.equal(dateTime('raw-invalid-timestamp'),'raw-invalid-timestamp')
  await setLocale('ru')
  assert.equal(money('1000000'),'1,00\u00a0$')
  assert.equal(money('-1'),'-0,000001\u00a0$')
  assert.equal(parseBudget('1.000001'),1000001)
 }finally{await setLocale('ru')}
})

test('local validation text translates while server error details remain exact',async()=>{
 try{
  await setLocale('en')
  assert.equal(errorMessage(new Error('Укажите организацию и токен доступа.')),'Enter an organization and access token.')
  const serverError=Object.assign(new Error('Укажите организацию и токен доступа.'),{code:'forbidden'})
  assert.equal(errorMessage(serverError),serverError.message)
  assert.equal(errorMessage(new Error('Custom provider detail: Задачи')),'Custom provider detail: Задачи')
 }finally{await setLocale('ru')}
})
