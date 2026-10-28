import {t} from './i18n'
import type { BrowserSession, Organization } from './api'
import { dateTime } from './format'
import { ErrorNotice, Icon } from './ui'

const roles: Record<string, string> = { owner: 'Владелец', admin: 'Администратор', member: 'Участник', viewer: 'Просмотр' }

export default function AuthPanel({ session, organizations, selected, busy, error, more, onSelect, onMore, onRefresh, onRotate, onLogout }: {
  session: BrowserSession | null; organizations: Organization[]; selected?: string; busy: boolean; error: unknown; more: boolean
  onSelect: (id: string) => void; onMore: () => void; onRefresh: () => void; onRotate: () => void; onLogout: () => void
}) {
  return <div className="settings-grid"><section className="panel connection-panel">
    <div className="panel-title"><h2>{session ? t("Ваши организации") : t("Вход в рабочее пространство")}</h2><Icon name="shield" /></div>
    <ErrorNotice error={error} onRetry={onRefresh} />
    {!session ? <><p>{t("Войдите через настроенный сервис вашей организации. Пароль вводится на странице провайдера входа.")}</p><form method="post" action="/auth/oidc/start"><button className="button button-primary" type="submit" disabled={busy}><Icon name="link" />{t("Войти через организацию")}</button></form></> : <>
      <label className="field">{t("Организация")}<select aria-label={t("Организация для работы")} value={selected ?? ''} onChange={event => onSelect(event.target.value)} disabled={busy}>
        <option value="" disabled>{t("Выберите организацию")}</option>{organizations.map(org => <option key={org.id} value={org.id}>{org.name} · {Object.hasOwn(roles, org.role) ? t(roles[org.role]) : org.role}</option>)}
      </select><small>{t("Здесь только организации, доступные вашей учётной записи. Права проверяются сервером.")}</small></label>
      {!organizations.length && <p>{t("Доступных организаций пока нет. Администратор должен добавить вашу учётную запись в организацию.")}</p>}
      {more && <button className="button button-secondary" disabled={busy} onClick={onMore}>{t("Загрузить ещё организации")}</button>}
      <p className="muted">{t("Сессия действует до")} {dateTime(session.expires_at)}.</p>
      <div className="heading-actions"><button className="button button-secondary" disabled={busy} onClick={onRotate}>{t("Обновить сессию")}</button><button className="button button-secondary" disabled={busy} onClick={onLogout}>{t("Выйти")}</button></div>
    </>}
  </section><aside className="connection-guide"><span className="guide-icon"><Icon name="shield" /></span><h2>{t("Вход вашей организации")}</h2><p>{t("Доступ привязан к учётной записи, которую подтвердил администратор.")}</p><ul><li>{t("Пароль и секрет сессии недоступны этому интерфейсу.")}</li><li>{t("Смена организации не добавляет прав.")}</li><li>{t("Выход закрывает сессию платформы.")}</li></ul><p className="muted">{t("Сессия провайдера входа управляется отдельно. После выхода повторный вход может не запросить пароль.")}</p></aside></div>
}
