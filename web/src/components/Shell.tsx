import { ReactNode } from 'react'
import { Activity, Copy, KeyRound, LogOut, ShieldCheck, UserRound } from 'lucide-react'
import { copyText } from '../api'
import { SessionInfo } from '../types'

export type Page = 'overview' | 'account' | 'keys'

type Props = {
  page: Page
  session: SessionInfo
  children: ReactNode
  onNavigate: (page: Page) => void
  onLogout: () => void
  notify: (message: string) => void
}

const nav: Array<{ id: Page; label: string; icon: typeof Activity }> = [
  { id: 'overview', label: '总览', icon: Activity },
  { id: 'account', label: '账号', icon: UserRound },
  { id: 'keys', label: 'API Keys', icon: KeyRound },
]

export function Shell({ page, session, children, onNavigate, onLogout, notify }: Props) {
  const copyBaseURL = async () => {
    await copyText(session.base_url)
    notify('Base URL 已复制')
  }

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand">
          <div className="brand-mark"><img src="/codexone-logo-dark.svg" alt="" aria-hidden="true" /></div>
          <div><strong>CodexOne</strong><small>Personal gateway</small></div>
        </div>
        <nav className="nav-list" aria-label="主导航">
          {nav.map(({ id, label, icon: Icon }) => (
            <button key={id} type="button" className={page === id ? 'nav-item active' : 'nav-item'} onClick={() => onNavigate(id)}>
              <Icon size={18} /><span>{label}</span>
            </button>
          ))}
        </nav>
        <div className="sidebar-foot">
          <div className="security-note"><ShieldCheck size={16} /><span>单账号模式</span></div>
          <button className="nav-item" type="button" onClick={onLogout}><LogOut size={18} /><span>退出后台</span></button>
        </div>
      </aside>
      <main className="main-area">
        <header className="topbar">
          <div className="mobile-brand">CodexOne</div>
          <button className="base-url" type="button" onClick={copyBaseURL} title="复制 Base URL">
            <span className="status-dot" />
            <code>{session.base_url}</code>
            <Copy size={15} />
          </button>
          <div className="runtime-chip">{session.storage === 'sqlite' ? 'SQLite' : 'PostgreSQL + Redis'}</div>
        </header>
        <div className="page-body">{children}</div>
        <nav className="mobile-nav" aria-label="移动端导航">
          {nav.map(({ id, label, icon: Icon }) => (
            <button key={id} type="button" className={page === id ? 'active' : ''} onClick={() => onNavigate(id)}><Icon size={18} /><span>{label}</span></button>
          ))}
        </nav>
      </main>
    </div>
  )
}
