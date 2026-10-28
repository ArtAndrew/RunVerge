import {getIntlLocale, getLocale, t} from './i18n.ts'
export const statusLabels: Record<string, string> = {
  queued: 'В очереди', running: 'В работе', succeeded: 'Завершена', failed: 'Ошибка',
  cancelled: 'Отменена', waiting_input: 'Нужен ваш ответ', waiting_approval: 'Ждёт согласования', blocked: 'Приостановлена',
}
export const terminalStatuses = new Set(['succeeded', 'failed', 'cancelled'])

export function money(value: string | number): string {
  let amount: bigint
  if (typeof value === 'number') {
    // Numeric values are bounded admission policy amounts only.
    if (!Number.isSafeInteger(value) || value < 0 || value > 1_000_000_000_000) return t('Нет данных')
    amount = BigInt(value)
  } else {
    if (typeof value !== 'string' || value.length > 20 || !/^(?:0|-?[1-9][0-9]*)(?![\s\S])/.test(value)) return t('Нет данных')
    amount = BigInt(value)
    if (amount < -9223372036854775808n || amount > 9223372036854775807n) return t('Нет данных')
  }
  const absolute = amount < 0n ? -amount : amount
  const whole = new Intl.NumberFormat(getIntlLocale(), { maximumFractionDigits: 0 }).format(absolute / 1_000_000n)
  const fraction = (absolute % 1_000_000n).toString().padStart(6, '0').replace(/0+$/, '').padEnd(2, '0')
  return getLocale() === 'en' ? `${amount < 0n ? '-' : ''}$${whole}.${fraction}` : `${amount < 0n ? '-' : ''}${whole},${fraction}\u00a0$`
}

export function parseBudget(value: string): number {
  const normalized = value.trim().replace(',', '.')
  if (!/^\d+(?:\.\d{1,6})?$/.test(normalized)) throw new Error('Укажите положительную сумму в USD, до 6 знаков после запятой.')
  const [whole, fraction = ''] = normalized.split('.')
  const amount = BigInt(whole) * 1_000_000n + BigInt(fraction.padEnd(6, '0'))
  if (amount <= 0n || amount > 1_000_000_000_000n) throw new Error('Сумма должна быть больше нуля и не превышать 1 000 000 USD.')
  return Number(amount)
}

export function dateTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(getIntlLocale(), { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }).format(date)
}

export function byteLength(value: string): number { return new TextEncoder().encode(value).length }
export function errorMessage(error: unknown): string {
  // API error messages may originate on the server; retain their exact text.
  if (error instanceof Error) return 'code' in error ? error.message : t(error.message)
  return t('Не удалось выполнить действие.')
}

export function number(value: number | bigint): string { return new Intl.NumberFormat(getIntlLocale()).format(value) }
