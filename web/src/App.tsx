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

export default function App() {
  const [session, setSession] = useState<SessionInfo | null>(null)
  const [page, setPage] = useState<Page>(pageFromHash())
  const [toast, setToast] = useState('')

  const checkSession = useCallback(async () => {
    try { setSession(await api<SessionInfo>('/api/auth/session')) }
    catch { setSession({ authenticated: false, base_url: '/v1', storage: 'sqlite', client: 'codex-tui' }) }
  }, [])

  useEffect(() => { void checkSession() }, [checkSession])
  useEffect(() => {
    const hash = () => setPage(pageFromHash())
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

  if (!session) return <div className="boot-screen"><div className="brand-mark large"><span>C</span></div></div>
  if (!session.authenticated) return <Login onSuccess={() => void checkSession()} />

  const navigate = (next: Page) => { window.location.hash = `/${next}`; setPage(next) }
  const logout = async () => { await api('/api/auth/logout', { method: 'POST' }); setSession({ ...session, authenticated: false }) }
  const content = page === 'account' ? <Account notify={setToast} /> : page === 'keys' ? <Keys notify={setToast} /> : <Overview notify={setToast} />

  return <><Shell page={page} session={session} onNavigate={navigate} onLogout={() => void logout()} notify={setToast}>{content}</Shell>{toast && <div className="toast" role="status">{toast}</div>}</>
}
