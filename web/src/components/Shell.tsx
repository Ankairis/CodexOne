import { ReactNode } from 'react'
import { Activity, Copy, KeyRound, LogOut, Radio, ShieldCheck, UserRound } from 'lucide-react'
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
  const currentPage = nav.find((item) => item.id === page) ?? nav[0]
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
        <div className="nav-caption">控制台</div>
        <nav className="nav-list" aria-label="主导航">
          {nav.map(({ id, label, icon: Icon }) => (
            <button key={id} type="button" className={page === id ? 'nav-item active' : 'nav-item'} aria-current={page === id ? 'page' : undefined} onClick={() => onNavigate(id)}>
              <Icon size={18} /><span>{label}</span>
              {page === id && <i aria-hidden="true" />}
            </button>
          ))}
        </nav>
        <div className="sidebar-foot">
          <div className="gateway-card">
            <div className="gateway-status"><Radio size={15} /><span>Gateway online</span><i /></div>
            <p>仅连接一个上游账号</p>
            <div className="security-note"><ShieldCheck size={15} /><span>Single-account mode</span></div>
          </div>
          <button className="nav-item" type="button" onClick={onLogout}><LogOut size={18} /><span>退出后台</span></button>
        </div>
      </aside>
      <main className="main-area">
        <header className="topbar">
          <div className="topbar-context">
            <div className="mobile-brand"><img src="/codexone-logo.svg" alt="" aria-hidden="true" />CodexOne</div>
            <div className="desktop-context"><span>Workspace</span><strong>{currentPage.label}</strong></div>
          </div>
          <div className="topbar-actions">
            <button className="base-url" type="button" onClick={copyBaseURL} title="复制 Base URL">
              <span className="status-dot" />
              <span className="base-url-copy"><small>BASE URL</small><code>{session.base_url}</code></span>
              <Copy size={15} />
            </button>
            <div className="runtime-chip">{session.storage === 'sqlite' ? 'SQLite' : 'PostgreSQL + Redis'}</div>
          </div>
        </header>
        <div className="page-body">{children}</div>
        <nav className="mobile-nav" aria-label="移动端导航">
          {nav.map(({ id, label, icon: Icon }) => (
            <button key={id} type="button" className={page === id ? 'active' : ''} aria-current={page === id ? 'page' : undefined} onClick={() => onNavigate(id)}><Icon size={18} /><span>{label}</span></button>
          ))}
        </nav>
      </main>
    </div>
  )
}
