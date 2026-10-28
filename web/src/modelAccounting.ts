// SPDX-License-Identifier: Elastic-2.0
import type {AvailableModel} from './api'
import {money} from './format.ts'
import {t} from './i18n.ts'

// Suggest only an explicitly declared local profile. Metered profiles and
// synthetic executors are never auto-selected; later pages preserve a choice.
export function chooseModelSelection(current:string, explicitlySelected:boolean, profiles:AvailableModel[]):string {
  if (explicitlySelected || current) return current
  for (const profile of profiles) {
    if (profile.accounting_mode !== 'operator_unmetered_local' || typeof profile.model !== 'string' || !profile.model || profile.model !== profile.model.trim() || /^(fixture|team):/.test(profile.model)) continue
    validateModelProfile(profile)
    return profile.model
  }
  return ''
}

export function validateModelProfile(value:AvailableModel):void {
  const within=(n:unknown,max:number)=>typeof n==='number'&&Number.isSafeInteger(n)&&n>=1&&n<=max
  if(!within(value.max_input_tokens,1_000_000_000)||!within(value.max_input_bytes,1_048_576)||
    !within(value.max_output_bytes,262144)||!within(value.max_output_tokens,1_000_000)||!within(value.timeout_milliseconds,600000)||
    (value.accounting_mode==='operator_unmetered_local'?value.max_charge_microusd!==0:
    value.accounting_mode!=='metered'||!within(value.max_charge_microusd,1_000_000_000_000)))throw new Error('Некорректные сведения о тарифе и ограничениях модели.')
}

export function modelTariffLabel(value:AvailableModel):string {
  validateModelProfile(value)
  return value.accounting_mode==='operator_unmetered_local'
    ?t('Локальный API: тариф 0 задан администратором. Расходы оборудования и энергии не измерены.')
    :t('Предел одного вызова API: {charge}. Это резерв, не окончательный счёт.', {charge:money(value.max_charge_microusd)})
}

export function modelResourceLabel(value:AvailableModel):string {
  validateModelProfile(value)
  return t('Один запрос: до {inputBytes} байт входа; {inputTokens} входных токенов — предположение профиля, не гарантия по числу байтов. Запрашивается до {outputTokens} выходных токенов; сохраняется до {outputBytes} байт результата. Ожидание ответа до {seconds} с не гарантирует остановку вычисления на сервере. Это ограничения обращений, не квоты CPU/GPU.', {inputBytes:value.max_input_bytes,inputTokens:value.max_input_tokens,outputTokens:value.max_output_tokens,outputBytes:value.max_output_bytes,seconds:value.timeout_milliseconds/1000})
}

export function unknownOperationCount(value:unknown):string {
  if(typeof value!=='string'||value.length>19||!/^(0|[1-9][0-9]*)(?![\s\S])/.test(value)||BigInt(value)>9223372036854775807n)throw new Error('Сведения об операциях, ожидающих сверки, недоступны.')
  return value
}
