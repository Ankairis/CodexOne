import { FormEvent, useState } from 'react'
import { ArrowRight, Check, Eye, EyeOff, KeyRound, LoaderCircle, LockKeyhole, Radio, ShieldCheck } from 'lucide-react'
import { api } from '../api'

export function Login({ onSuccess }: { onSuccess: () => void }) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [showPassword, setShowPassword] = useState(false)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setLoading(true)
    setError('')
    try {
      await api('/api/auth/login', { method: 'POST', body: JSON.stringify({ password }) })
      onSuccess()
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '登录失败')
    } finally {
      setLoading(false)
    }
  }

  return (
    <main className="login-page">
      <div className="login-glow login-glow-one" />
      <div className="login-glow login-glow-two" />
      <section className="login-shell">
        <aside className="login-story">
          <div className="login-brand"><div className="brand-mark large"><img src="/codexone-logo-dark.svg" alt="" aria-hidden="true" /></div><span>CodexOne</span></div>
          <div className="login-story-copy">
            <span className="login-kicker">Single-account Codex gateway</span>
            <h1>把 Codex 接到<br />你自己的工具里。</h1>
            <p>轻量、私有、只服务一个上游账号。API、额度和运行状态都在一个地方。</p>
          </div>
          <div className="login-terminal" aria-hidden="true">
            <div className="terminal-head"><span><i /><i /><i /></span><small>gateway</small><span className="terminal-online"><Radio size={12} /> online</span></div>
            <div className="terminal-line"><span>POST</span><code>/v1/responses</code><strong>200</strong></div>
            <div className="terminal-line muted"><span>MODEL</span><code>gpt-5.4</code><strong>max</strong></div>
          </div>
          <div className="login-trust"><span><Check size={14} />本地凭据加密</span><span><Check size={14} />无账号池</span></div>
        </aside>
        <section className="login-panel">
          <div className="login-panel-mark"><ShieldCheck size={20} /></div>
          <div className="login-heading">
            <span className="page-eyebrow">Private console</span>
            <h2>欢迎回来</h2>
            <p>输入管理员密码，继续管理你的网关。</p>
          </div>
          <form onSubmit={submit}>
            <label htmlFor="password">管理员密码</label>
            <div className="input-with-icon">
              <LockKeyhole size={18} />
              <input id="password" autoFocus autoComplete="current-password" type={showPassword ? 'text' : 'password'} value={password} onChange={(event) => setPassword(event.target.value)} placeholder="输入密码" />
              <button className="password-toggle" type="button" aria-label={showPassword ? '隐藏密码' : '显示密码'} aria-pressed={showPassword} onClick={() => setShowPassword((value) => !value)}>
                {showPassword ? <EyeOff size={17} /> : <Eye size={17} />}
              </button>
            </div>
            {error && <div className="form-error" role="alert">{error}</div>}
            <button className="primary-button full login-submit" type="submit" disabled={loading || !password}>
              {loading ? <LoaderCircle className="spin" size={17} /> : <KeyRound size={17} />}
              <span>进入控制台</span>
              {!loading && <ArrowRight size={17} />}
            </button>
          </form>
          <div className="login-help"><ShieldCheck size={15} /><span>密码只用于当前 CodexOne 实例，不会发送给 OpenAI。</span></div>
        </section>
      </section>
      <footer>Built for one account · Powered by your own Codex subscription</footer>
    </main>
  )
}
