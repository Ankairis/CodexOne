import { FormEvent, useState } from 'react'
import { ArrowRight, KeyRound, LoaderCircle, LockKeyhole } from 'lucide-react'
import { api } from '../api'

export function Login({ onSuccess }: { onSuccess: () => void }) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

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
      <section className="login-panel">
        <div className="login-brand"><div className="brand-mark large"><img src="/codexone-logo.svg" alt="" aria-hidden="true" /></div><span>CodexOne</span></div>
        <div className="login-heading">
          <h1>进入管理后台</h1>
          <p>使用服务首次启动时生成的管理员密码。</p>
        </div>
        <form onSubmit={submit}>
          <label htmlFor="password">管理员密码</label>
          <div className="input-with-icon"><LockKeyhole size={17} /><input id="password" autoFocus autoComplete="current-password" type="password" value={password} onChange={(event) => setPassword(event.target.value)} placeholder="输入密码" /></div>
          {error && <div className="form-error" role="alert">{error}</div>}
          <button className="primary-button full" type="submit" disabled={loading || !password}>
            {loading ? <LoaderCircle className="spin" size={17} /> : <KeyRound size={17} />}
            登录
            {!loading && <ArrowRight size={17} />}
          </button>
        </form>
      </section>
      <footer>Single-account Codex gateway</footer>
    </main>
  )
}
