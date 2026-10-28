// SPDX-License-Identifier: Elastic-2.0
// Local, source-key dictionaries. User/model text and API payloads never pass through t().
import {useSyncExternalStore} from 'react'
import {enCommon} from './i18n/en-common.ts'
import {enComponents} from './i18n/en-components.ts'
import {enAdmin} from './i18n/en-admin.ts'

export const supportedLocales = [
  {code:'ru', name:'Русский', intl:'ru-RU'},
  {code:'en', name:'English', intl:'en-US'},
  {code:'es', name:'Español', intl:'es-ES'},
  {code:'pt-BR', name:'Português (Brasil)', intl:'pt-BR'},
  {code:'de', name:'Deutsch', intl:'de-DE'},
  {code:'fr', name:'Français', intl:'fr-FR'},
  {code:'zh-CN', name:'简体中文', intl:'zh-CN'},
] as const
export type Locale = typeof supportedLocales[number]['code']
export type TranslationValues = Readonly<Record<string, string | number>>
export const localeStorageKey = 'runverge.locale'
type Dictionary = Readonly<Record<string, string>>
// Shared terminology wins over feature-specific duplicates.
const dictionaries: Partial<Record<Locale, Dictionary>> = {ru:{}, en:{...enComponents, ...enAdmin, ...enCommon}}
// Additional languages are separate local assets, loaded only when chosen.
const loaders = {
  es: () => import('./i18n/es.ts'),
  'pt-BR': () => import('./i18n/pt-BR.ts'),
  de: () => import('./i18n/de.ts'),
  fr: () => import('./i18n/fr.ts'),
  'zh-CN': () => import('./i18n/zh-CN.ts'),
}
const listeners = new Set<() => void>()
const loadingListeners = new Set<() => void>()
function isLocale(value: unknown): value is Locale { return supportedLocales.some(item => item.code === value) }
export function resolveLocale(saved: unknown, languages: readonly string[] = []): Locale {
  if (isLocale(saved)) return saved
  for (const language of languages) {
    const tag = language.toLowerCase()
    const base = tag.split('-')[0]
    if (base === 'pt') return 'pt-BR'
    // A Simplified Chinese translation must not masquerade as Traditional Chinese.
    if (base === 'zh') {
      if (tag === 'zh' || /^zh-(?:cn|sg|hans)(?:-|$)/.test(tag)) return 'zh-CN'
      continue
    }
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
// Render immediately with a bundled language while another dictionary loads.
let locale: Locale = typeof window === 'undefined' ? 'ru' : 'en'
let selection = 0
let loadingState = {busy:false, error:false}
function setLoadingState(busy: boolean, error: boolean): void {
  if (loadingState.busy === busy && loadingState.error === error) return
  loadingState = {busy,error}
  for (const listener of loadingListeners) listener()
}
function getLoadingState() { return loadingState }
export function useLocaleLoading() {
  return useSyncExternalStore(listener => {
    loadingListeners.add(listener)
    return () => { loadingListeners.delete(listener) }
  }, getLoadingState, getLoadingState)
}
export function getLocale(): Locale { return locale }
export function getIntlLocale(): string { return supportedLocales.find(item => item.code === locale)!.intl }
export function getLocaleLoadError(): boolean { return loadingState.error }
export async function initializeLocale(): Promise<void> { await setLocale(initialLocale()) }
export function syncDocumentLanguage(): void {
  if (typeof document !== 'undefined') document.documentElement.lang = locale
}
export async function setLocale(value: string): Promise<boolean> {
  if (!isLocale(value)) return false
  const request = ++selection
  setLoadingState(true,false)
  let timeout: ReturnType<typeof setTimeout> | undefined
  try {
    if (!dictionaries[value] && value !== 'ru' && value !== 'en') {
      // A stalled asset cannot disable language selection indefinitely. A late
      // response can populate the cache but cannot apply a stale selection.
      const loading = loaders[value]().then(module => { dictionaries[value] = module.default })
      await Promise.race([loading, new Promise<never>((_,reject) => {
        timeout = setTimeout(() => reject(new Error('language-load-timeout')), 8000)
      })])
    }
  } catch {
    if (request === selection) setLoadingState(false,true)
    return false
  } finally {
    clearTimeout(timeout)
  }
  // A slow earlier download may warm the cache, but cannot override a later choice.
  if (request !== selection) return false
  setLoadingState(false,false)
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
  const dictionary = dictionaries[locale]!
  const translated = Object.hasOwn(dictionary, source) ? dictionary[source] : source
  return translated.replace(/\{([A-Za-z][A-Za-z0-9_]*)\}/g, (token, key: string) => Object.hasOwn(values, key) ? String(values[key]) : token)
}
