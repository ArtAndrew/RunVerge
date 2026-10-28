// SPDX-License-Identifier: Elastic-2.0
// Local, source-key dictionaries. User/model text and API payloads never pass through t().
import {useSyncExternalStore} from 'react'
import {enCommon} from './i18n/en-common.ts'
import {enComponents} from './i18n/en-components.ts'
import {enAdmin} from './i18n/en-admin.ts'

export const supportedLocales = [{code:'ru', name:'Русский'}, {code:'en', name:'English'}] as const
export type Locale = typeof supportedLocales[number]['code']
export type TranslationValues = Readonly<Record<string, string | number>>
export const localeStorageKey = 'runverge.locale'
// Shared terminology wins over feature-specific duplicates.
const dictionaries: Record<Locale, Readonly<Record<string, string>>> = {ru:{}, en:{...enComponents, ...enAdmin, ...enCommon}}
const listeners = new Set<() => void>()
function isLocale(value: unknown): value is Locale { return value === 'ru' || value === 'en' }
// Register another language here only after its dictionary and UI have been reviewed.
export function resolveLocale(saved: unknown, languages: readonly string[] = []): Locale {
  if (isLocale(saved)) return saved
  for (const language of languages) {
    const base = language.toLowerCase().split('-')[0]
    if (isLocale(base)) return base
  }
  return 'en'
}
function initialLocale(): Locale {
  if (typeof window === 'undefined') return 'ru'
  let saved: unknown
  try { saved = window.localStorage.getItem(localeStorageKey) } catch { /* Optional storage. */ }
  return resolveLocale(saved, typeof navigator === 'undefined' ? [] : navigator.languages?.length ? navigator.languages : [navigator.language])
}
let locale = initialLocale()
export function getLocale(): Locale { return locale }
export function getIntlLocale(): string { return locale === 'ru' ? 'ru-RU' : 'en-US' }
export function syncDocumentLanguage(): void {
  if (typeof document !== 'undefined') document.documentElement.lang = locale
}
export function setLocale(value: string): boolean {
  if (!isLocale(value)) return false
  try { if (typeof window !== 'undefined') window.localStorage.setItem(localeStorageKey, value) } catch { /* In-memory switching still works. */ }
  if (value === locale) { syncDocumentLanguage(); return true }
  locale = value
  syncDocumentLanguage()
  for (const listener of listeners) listener()
  return true
}
export function subscribeLocale(listener: () => void): () => void {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}
export function useLocale(): Locale { return useSyncExternalStore(subscribeLocale, getLocale, getLocale) }
export function t(source: string, values: TranslationValues = {}): string {
  const dictionary = dictionaries[locale]
  const translated = Object.hasOwn(dictionary, source) ? dictionary[source] : source
  return translated.replace(/\{([A-Za-z][A-Za-z0-9_]*)\}/g, (token, key: string) => Object.hasOwn(values, key) ? String(values[key]) : token)
}
