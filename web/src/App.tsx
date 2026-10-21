import { useEffect } from 'react'
import { BrowserRouter, Link, Route, Routes, useLocation } from 'react-router-dom'
import { AuthGate } from './auth'
import { AudioBar } from './components/AudioBar'
import { TimelinePage } from './pages/TimelinePage'
import { SearchPage } from './pages/SearchPage'
import { DetailPage } from './pages/DetailPage'
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
        </nav>
      </header>
      <main className="app-main">
        <Routes>
          <Route path="/" element={<TimelinePage />} />
          <Route path="/search" element={<SearchPage />} />
          <Route path="/c/:conversationId" element={<DetailPage />} />
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
        <AudioBar>
          <KeyboardBridge />
          <AppShell />
        </AudioBar>
      </AuthGate>
    </BrowserRouter>
  )
}

export default Root
