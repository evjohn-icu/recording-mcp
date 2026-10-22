import { useEffect } from 'react'
import { BrowserRouter, Link, Route, Routes, useLocation } from 'react-router-dom'
import { AuthGate } from './auth'
import { PendingForget } from './components/ForgetAction'
import { PendingSuppressions } from './components/PendingSuppressions'
import { TimelinePage } from './pages/TimelinePage'
import { SearchPage } from './pages/SearchPage'
import { DetailPage } from './pages/DetailPage'
import { HistoryPage } from './pages/HistoryPage'
import { installKeyboard } from './keyboard'
import './styles.css'

function KeyboardBridge() {
  const location = useLocation()
  useEffect(() => installKeyboard(), [location.pathname])
  return null
}

function AppShell() {
  return (
    <div className="app">
      <header className="app-header">
        <div className="brand">本地记忆</div>
        <nav className="app-nav" aria-label="主导航">
          <Link to="/">时间线</Link>
          <Link to="/search">搜索</Link>
          <Link to="/history">事实变化</Link>
        </nav>
      </header>
      <main className="app-main">
        <PendingForget profile={sessionStorage.getItem('recording-profile') || ''} />
        <PendingSuppressions />
        <Routes>
          <Route path="/" element={<TimelinePage />} />
          <Route path="/search" element={<SearchPage />} />
          <Route path="/c/:conversationId" element={<DetailPage />} />
          <Route path="/history" element={<HistoryPage />} />
          <Route path="*" element={<p className="not-found">页面不存在</p>} />
        </Routes>
      </main>
    </div>
  )
}

export function Root() {
  return (
    <BrowserRouter>
      <AuthGate>
        <KeyboardBridge />
        <AppShell />
      </AuthGate>
    </BrowserRouter>
  )
}

export default Root
