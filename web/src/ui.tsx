import {t} from './i18n'
import { useContext, useEffect, useRef, type ReactNode } from 'react'
import {EventReadinessContext} from './eventReadiness'
import { ApiError } from './api'
import { errorMessage, money, statusLabels } from './format'

export type IconName = 'tasks' | 'agents' | 'settings' | 'plus' | 'arrow' | 'refresh' | 'close' | 'check' | 'clock' | 'file' | 'shield' | 'link' | 'menu' | 'stop' | 'activity'
const paths: Record<IconName, ReactNode> = {
  tasks: <><rect x="5" y="4" width="14" height="17" rx="2" /><path d="M9 3h6v3H9zM9 11h6M9 15h4" /></>,
  agents: <><rect x="4" y="7" width="16" height="13" rx="4" /><path d="M12 3v4M2 12v4M22 12v4M9 16h6M8 11h.01M16 11h.01" /></>,
  settings: <><path d="M4 7h16M4 17h16" /><circle cx="9" cy="7" r="3" /><circle cx="15" cy="17" r="3" /></>,
  plus: <path d="M12 5v14M5 12h14" />,
  arrow: <path d="M5 12h14M13 6l6 6-6 6" />,
  refresh: <><path d="M20 7v5h-5M4 17v-5h5" /><path d="M6 7a7 7 0 0 1 12-1l2 6M4 12l2 6a7 7 0 0 0 12-1" /></>,
  close: <path d="m6 6 12 12M6 18 18 6" />,
  check: <path d="m5 12 4 4L19 6" />,
  clock: <><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></>,
  file: <><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9zM14 3v6h6M8 14h8M8 17h5" /></>,
  shield: <><path d="m12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6z" /><path d="m8 12 3 3 5-6" /></>,
  link: <><path d="m10 13 4-4M8 15l-2 2a3 3 0 0 1-4-4l4-4a3 3 0 0 1 4 0M14 9l2-2a3 3 0 0 1 4 4l-4 4a3 3 0 0 1-4 0" /></>,
  menu: <path d="M4 6h16M4 12h16M4 18h16" />,
  stop: <rect x="6" y="6" width="12" height="12" rx="2" />,
  activity: <path d="M2 12h5l3-8 4 16 3-8h5" />,
}

export function Icon({ name, className = '' }: { name: IconName; className?: string }) {
  return <svg className={`icon ${className}`} width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[name]}</svg>
}

export function Status({ status }: { status: string }) {
  return <span className={`status status-${status}`}><span className="status-dot" />{Object.hasOwn(statusLabels, status) ? t(statusLabels[status]) : status}</span>
}

export function ErrorNotice({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  if (!error) return null
  return <div className="notice notice-error" role="alert">
    <div><strong>{t("Не удалось выполнить запрос")}</strong><p>{errorMessage(error)}</p>{error instanceof ApiError && <small>{error.code}{error.requestId ? t(" · Запрос {value0}", {value0: error.requestId}) : ''}</small>}</div>
    {onRetry && <button className="button button-small button-secondary" onClick={onRetry}><Icon name="refresh" />{t("Повторить")}</button>}
  </div>
}

export function EmptyState({ icon = 'tasks', title, description, action }: { icon?: IconName; title: string; description: string; action?: ReactNode }) {
  return <div className="empty-state"><div className="empty-icon"><Icon name={icon} /></div><h3>{title}</h3><p>{description}</p>{action}</div>
}

export function Modal({ title, description, onClose, busy = false, children }: { title: string; description?: string; onClose: () => void; busy?: boolean; children: ReactNode }) {
  const eventReadiness=useContext(EventReadinessContext)
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const dialog = ref.current
    dialog?.showModal()
    return () => dialog?.close()
  }, [])
  return <dialog ref={ref} className="modal" aria-labelledby="modal-title" onCancel={(event) => { event.preventDefault(); if (!busy) onClose() }}>
    <div className="modal-header"><div><h2 id="modal-title">{title}</h2>{description && <p>{description}</p>}</div><button className="icon-button" onClick={onClose} disabled={busy} aria-label={t("Закрыть")}><Icon name="close" /></button></div>
    {eventReadiness.paused&&<div className="notice notice-warning" role="status"><div><strong>{t("Нужно обновить состояние")}</strong><p>{t("Форма и несохранённый текст сохранены. Отправка приостановлена до получения нового снимка.")}</p><button className="button button-secondary" onClick={eventReadiness.refresh} disabled={busy}>{t("Обновить состояние")}</button></div></div>}
    <div inert={eventReadiness.paused}>{children}</div>
  </dialog>
}

export function BudgetBar({ budget, spent, reserved }: { budget: number; spent: string; reserved: string }) {
  const safe = (value: string) => /^(?:0|[1-9][0-9]{0,18})$/.test(value) ? BigInt(value) : 0n
  const percentage = (value: bigint) => budget > 0 ? Number((value * 10_000n / BigInt(budget)) > 10_000n ? 10_000n : value * 10_000n / BigInt(budget)) / 100 : 0
  const used = percentage(safe(spent))
  const held = Math.min(100 - used, percentage(safe(reserved)))
  return <div className="budget-block">
    <div className="budget-label"><strong>{money(spent)}</strong><span>{t("из")} {money(budget)}</span></div>
    <div className="budget-track" role="img" aria-label={t("Потрачено {value0}, зарезервировано {value1}, лимит {value2}", {value0: money(spent), value1: money(reserved), value2: money(budget)})}><span className="budget-used" style={{ width: `${used}%` }} /><span className="budget-held" style={{ width: `${held}%` }} /></div>
  </div>
}

export function Loading({ label = t("Загружаем данные…") }: { label?: string }) {
  return <div className="loading" role="status"><span className="spinner" />{label}</div>
}
