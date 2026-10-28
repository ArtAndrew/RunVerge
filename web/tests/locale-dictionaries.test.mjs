// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import {supportedLocales, setLocale, getLocale, getIntlLocale, t} from '../src/i18n.ts'

const entries = Object.entries
const tokens = value => [...value.matchAll(/\{([A-Za-z][A-Za-z0-9_]*)\}/g)].map(match => match[0]).sort()
const edges = value => [value.match(/^\s*/)[0], value.match(/\s*$/)[0]]

test('all shipped dictionaries cover English keys and preserve placeholders and joined-text boundaries', async()=>{
  for (const section of ['common','components','admin']) {
    const reference = Object.values(await import(`../src/i18n/en-${section}.ts`))[0]
    for (const {code} of supportedLocales.filter(item => !['ru','en'].includes(item.code))) {
      const file = new URL(`../src/i18n/${code}-${section}.ts`,import.meta.url)
      const dictionary = Object.values(await import(file))[0]
      assert.deepEqual(Object.keys(dictionary).sort(),Object.keys(reference).sort(),`${code}/${section} keys`)
      for (const [key,value] of entries(dictionary)) {
        assert.equal(typeof value,'string',`${code}: ${key}`)
        assert.ok(value.trim().length,`${code}: empty ${key}`)
        assert.deepEqual(tokens(value),tokens(reference[key]),`${code}: placeholders ${key}`)
        assert.deepEqual(edges(value),edges(reference[key]),`${code}: boundaries ${key}`)
        assert.doesNotMatch(value,/[А-Яа-яЁё]/,`${code}: untranslated Russian ${key}`)
      }
      // Translation files are data only; no service URL or executable translation provider.
      const source = await readFile(file,'utf8')
      assert.match(source,/SPDX-License-Identifier: Elastic-2.0/)
    }
  }
})

test('lazy language loading changes labels and formatting without translating inserted content',async()=>{
  try {
    for (const {code,intl} of supportedLocales) {
      assert.equal(await setLocale(code),true,code)
      assert.equal(getLocale(),code)
      assert.equal(getIntlLocale(),intl)
      const userText = 'Original Задачи <b>{value0}</b>'
      assert.ok(t('Открыть задачу: {value0}',{value0:userText}).includes(userText))
      assert.equal(t('unrecognized-provider-status'),'unrecognized-provider-status')
      if (code !== 'ru') assert.notEqual(t('Задачи'),'Задачи',code)
    }
  } finally { await setLocale('ru') }
})

test('the latest choice wins over a still loading translation',async()=>{
  // Use a fresh module instance so the non-English dictionary is initially cold.
  const isolated = await import('../src/i18n.ts?selection-race')
  const earlier = isolated.setLocale('es')
  const later = isolated.setLocale('en')
  assert.equal(await later,true)
  assert.equal(await earlier,false)
  assert.equal(isolated.getLocale(),'en')
})
