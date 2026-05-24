import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { ConfigProvider } from 'antd'
import 'dayjs/locale/zh-cn'
import App from './App'
import './index.css'
// 触发 i18n 初始化（必须在任何使用 useTranslation 的组件渲染前完成）。
import './i18n'
import { useAntdLocale } from './i18n/LanguageSwitcher'

// 让 antd 内置文案（Pagination、DatePicker、Modal 等）跟随 i18n 语言切换。
function LocalizedApp() {
  const locale = useAntdLocale()
  return (
    <ConfigProvider locale={locale}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </ConfigProvider>
  )
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <LocalizedApp />
  </React.StrictMode>,
)
