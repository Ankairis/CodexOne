import { useCallback, useEffect, useState } from 'react'
import { api } from './api'
import { Shell, Page } from './components/Shell'
import { Login } from './pages/Login'
import { Overview } from './pages/Overview'
import { Account } from './pages/Account'
import { Keys } from './pages/Keys'
import { SessionInfo } from './types'

function pageFromHash(): Page {
  const value = window.location.hash.replace('#/', '')
  return value === 'account' || value === 'keys' ? value : 'overview'
}

function localToday() {
  const date = new Date()
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
}

export default function App() {
  const [session, setSession] = useState<SessionInfo | null>(null)
  const [page, setPage] = useState<Page>(pageFromHash())
  const [toast, setToast] = useState('')

  const checkSession = useCallback(async () => {
    try { setSession(await api<SessionInfo>('/api/auth/session')) }
    catch { setSession({ authenticated: false, base_url: '/v1', storage: 'sqlite', client: 'codex-tui', today: localToday() }) }
  }, [])

  useEffect(() => { void checkSession() }, [checkSession])
  useEffect(() => {
    const hash = () => {
      setPage(pageFromHash())
      window.scrollTo({ top: 0, left: 0 })
    }
    const unauthorized = () => setSession((current) => current ? { ...current, authenticated: false } : current)
    window.addEventListener('hashchange', hash)
    window.addEventListener('codexone:unauthorized', unauthorized)
    return () => { window.removeEventListener('hashchange', hash); window.removeEventListener('codexone:unauthorized', unauthorized) }
  }, [])
  useEffect(() => {
    if (!toast) return
    const timer = window.setTimeout(() => setToast(''), 2600)
    return () => window.clearTimeout(timer)
  }, [toast])

  if (!session) return <div className="boot-screen"><div className="brand-mark large"><img src="/codexone-logo.svg" alt="CodexOne" /></div></div>
  if (!session.authenticated) return <><Login onSuccess={() => void checkSession()} />{toast && <div className="toast" role="status">{toast}</div>}</>

  const navigate = (next: Page) => {
    window.location.hash = `/${next}`
    setPage(next)
    window.scrollTo({ top: 0, left: 0 })
  }
  const logout = async () => {
    try {
      await api('/api/auth/logout', { method: 'POST' })
      setSession({ ...session, authenticated: false })
    } catch (cause) {
      setToast(cause instanceof Error ? cause.message : '退出失败')
    }
  }
  const content = page === 'account' ? <Account notify={setToast} /> : page === 'keys' ? <Keys notify={setToast} /> : <Overview today={session.today} notify={setToast} />

  return <><Shell page={page} session={session} onNavigate={navigate} onLogout={() => void logout()} notify={setToast}>{content}</Shell>{toast && <div className="toast" role="status">{toast}</div>}</>
}
