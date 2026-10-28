import {initializeLocale, syncDocumentLanguage} from './i18n'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import './styles.css'

void initializeLocale()
syncDocumentLanguage()
createRoot(document.getElementById('root')!).render(
  <StrictMode><App /></StrictMode>,
)
