// SPDX-License-Identifier: Elastic-2.0
import {useEffect, useRef, useState} from 'react'
import {t} from './i18n'

export default function CopyText({text}: {text: string}) {
  const [status, setStatus] = useState<'idle' | 'copying' | 'copied' | 'failed'>('idle')
  const epoch = useRef(0)
  useEffect(() => {
    epoch.current++
    setStatus('idle')
    return () => { epoch.current++ }
  }, [text])

  async function copy() {
    const submittedEpoch = ++epoch.current
    setStatus('copying')
    try {
      // Copy plain text exactly as supplied. Never render or execute model output.
      await navigator.clipboard.writeText(text)
      if (submittedEpoch === epoch.current) setStatus('copied')
    } catch {
      if (submittedEpoch === epoch.current) setStatus('failed')
    }
  }

  return <div className="copy-text-control">
    <button type="button" className="button button-small button-secondary" disabled={status === 'copying'} onClick={() => void copy()}>{status === 'copying' ? t('Копируем…') : t('Скопировать точный текст')}</button>
    <span role="status" aria-live="polite">{status === 'copied' ? t('Точный текст скопирован.') : status === 'failed' ? t('Не удалось скопировать. Выделите текст вручную или скачайте материал.') : ''}</span>
  </div>
}
