import { FormEvent, useCallback, useEffect, useState } from 'react'
import { CheckCircle2, Clipboard, Clock3, ExternalLink, FileJson, LoaderCircle, LockKeyhole, LogIn, RefreshCw, ShieldCheck, Unplug, UserRound } from 'lucide-react'
import { api, copyText } from '../api'
import { AccountInfo, DeviceFlow, QuotaPayload, RateWindow } from '../types'
import { Modal } from '../components/Modal'

export function Account({ notify }: { notify: (message: string) => void }) {
  const [account, setAccount] = useState<AccountInfo | null>(null)
  const [flow, setFlow] = useState<DeviceFlow | null>(null)
  const [importOpen, setImportOpen] = useState(false)
  const [passwordOpen, setPasswordOpen] = useState(false)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try { setAccount(await api<AccountInfo>('/api/admin/account')) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '账号加载失败') }
    finally { setLoading(false) }
  }, [])
  useEffect(() => { void load() }, [load])

  useEffect(() => {
    if (!flow) return
    let stopped = false
    const poll = async () => {
      try {
        const result = await api<{ status: string; account?: AccountInfo }>(`/api/admin/account/device/${encodeURIComponent(flow.flow_id)}`)
        if (!stopped && result.status === 'complete') {
          setFlow(null)
          setAccount(result.account ?? null)
          notify('Codex 登录成功')
        }
      } catch (cause) {
        if (!stopped) { setFlow(null); setError(cause instanceof Error ? cause.message : '登录失败') }
      }
    }
    const timer = window.setInterval(() => void poll(), Math.max(flow.poll_interval, 3) * 1000)
    return () => { stopped = true; window.clearInterval(timer) }
  }, [flow, notify])

  const startLogin = async () => {
    setBusy('login'); setError('')
    try {
      const result = await api<DeviceFlow>('/api/admin/account/device', { method: 'POST' })
      setFlow(result)
      window.open(result.verification_url, '_blank', 'noopener,noreferrer')
    } catch (cause) { setError(cause instanceof Error ? cause.message : '无法启动登录') }
    finally { setBusy('') }
  }

  const refreshQuota = async () => {
    setBusy('quota'); setError('')
    try {
      const result = await api<{ quota: QuotaPayload; fetched_at: number }>('/api/admin/account/quota', { method: 'POST' })
      setAccount((current) => current ? { ...current, quota: result.quota, quota_fetched_at: result.fetched_at } : current)
      notify('额度已刷新')
    } catch (cause) { setError(cause instanceof Error ? cause.message : '额度刷新失败') }
    finally { setBusy('') }
  }

  const disconnect = async () => {
    if (!window.confirm('断开当前 Codex 账号？加密保存的 OAuth token 会被删除。')) return
    await api('/api/admin/account', { method: 'DELETE' })
    setAccount({ connected: false, client_name: account?.client_name ?? 'codex-tui' })
    notify('账号已断开')
  }

  if (loading) return <PageLoading />
  const quota = account?.quota

  return (
    <div className="page-stack">
      <div className="page-title-row"><div><h1>Codex 账号</h1><p>唯一上游账号、登录状态和订阅额度</p></div><button className="secondary-button" type="button" onClick={() => setPasswordOpen(true)}><LockKeyhole size={16} />修改后台密码</button></div>
      {error && <div className="notice error">{error}<button type="button" onClick={() => setError('')}>×</button></div>}
      {!account?.connected ? (
        <section className="account-empty">
          <div className="account-empty-icon"><UserRound size={28} /></div>
          <h2>连接你的 Codex 订阅</h2>
          <p>CodexOne 只保存并使用一个上游账号。再次登录会覆盖当前凭据，不会形成账号池。</p>
          <div className="empty-actions">
            <button className="primary-button" type="button" onClick={() => void startLogin()} disabled={busy === 'login'}>{busy === 'login' ? <LoaderCircle className="spin" size={17} /> : <LogIn size={17} />}使用设备码登录</button>
            <button className="secondary-button" type="button" onClick={() => setImportOpen(true)}><FileJson size={17} />导入 auth.json</button>
          </div>
          <div className="compat-line"><ShieldCheck size={15} />上游身份固定为 {account?.client_name || 'codex-tui'}</div>
        </section>
      ) : (
        <>
          <section className="account-summary panel">
            <div className="account-avatar"><UserRound size={24} /></div>
            <div className="account-main"><div className="account-name"><h2>{account.email || 'Codex account'}</h2><span className="status ok"><i />已连接</span></div><div className="account-meta"><span>套餐 <strong>{account.plan_type || quota?.plan_type || '未知'}</strong></span><span>Account ID <code>{account.account_id}</code></span><span>身份 <code>{account.client_name}</code></span></div></div>
            <button className="danger-button" type="button" onClick={() => void disconnect()}><Unplug size={16} />断开</button>
          </section>
          <section className="panel quota-panel">
            <div className="panel-header"><div><h2>订阅额度</h2><span>{account.quota_fetched_at ? `更新于 ${formatDate(account.quota_fetched_at)}` : '尚未读取'}</span></div><button className="secondary-button" type="button" onClick={() => void refreshQuota()} disabled={busy === 'quota'}><RefreshCw className={busy === 'quota' ? 'spin' : ''} size={16} />刷新额度</button></div>
            {quota?.rate_limit ? <div className="quota-grid"><QuotaWindow title="主窗口" window={quota.rate_limit.primary_window} /><QuotaWindow title="周窗口" window={quota.rate_limit.secondary_window} />{quota.additional_rate_limits?.map((limit, index) => <QuotaWindow key={`${limit.limit_name}-${index}`} title={limit.limit_name || limit.metered_feature || '附加额度'} window={limit.rate_limit?.primary_window} />)}</div> : <div className="quota-empty"><Clock3 size={21} /><span>点击“刷新额度”从 Codex 读取最新窗口，不会发起模型请求。</span></div>}
          </section>
          <section className="panel credential-panel"><div><span>Access token</span><strong>加密保存</strong></div><div><span>自动刷新</span><strong><CheckCircle2 size={15} />已启用</strong></div><div><span>凭据到期</span><strong>{account.expires_at ? formatDate(account.expires_at) : '未知'}</strong></div></section>
        </>
      )}
      {flow && <DeviceModal flow={flow} onClose={() => setFlow(null)} notify={notify} />}
      {importOpen && <ImportModal onClose={() => setImportOpen(false)} onImported={(value) => { setAccount(value); setImportOpen(false); notify('auth.json 已导入') }} />}
      {passwordOpen && <PasswordModal onClose={() => setPasswordOpen(false)} onChanged={() => { setPasswordOpen(false); notify('后台密码已修改') }} />}
    </div>
  )
}

function DeviceModal({ flow, onClose, notify }: { flow: DeviceFlow; onClose: () => void; notify: (message: string) => void }) {
  return <Modal title="Codex 设备码登录" onClose={onClose}><div className="modal-body device-flow"><p>在 OpenAI 页面输入下面的设备码。完成授权后，此窗口会自动更新。</p><button className="device-code" type="button" onClick={async () => { await copyText(flow.user_code); notify('设备码已复制') }}><code>{flow.user_code}</code><Clipboard size={17} /></button><a className="primary-button full" href={flow.verification_url} target="_blank" rel="noreferrer"><ExternalLink size={17} />打开 OpenAI 登录页</a><div className="polling"><LoaderCircle className="spin" size={15} />等待授权…</div></div></Modal>
}

function ImportModal({ onClose, onImported }: { onClose: () => void; onImported: (account: AccountInfo) => void }) {
  const [content, setContent] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const submit = async (event: FormEvent) => { event.preventDefault(); setLoading(true); setError(''); try { onImported(await api<AccountInfo>('/api/admin/account/import', { method: 'POST', body: JSON.stringify({ content }) })) } catch (cause) { setError(cause instanceof Error ? cause.message : '导入失败') } finally { setLoading(false) } }
  return <Modal title="导入 Codex auth.json" onClose={onClose} width="wide"><form className="modal-body" onSubmit={submit}><label htmlFor="auth-json">文件内容</label><textarea id="auth-json" className="code-input" value={content} onChange={(event) => setContent(event.target.value)} placeholder={'{\n  "tokens": { ... }\n}'} rows={12} />{error && <div className="form-error">{error}</div>}<div className="modal-actions"><button className="secondary-button" type="button" onClick={onClose}>取消</button><button className="primary-button" type="submit" disabled={!content.trim() || loading}>{loading && <LoaderCircle className="spin" size={16} />}导入并覆盖</button></div></form></Modal>
}

function PasswordModal({ onClose, onChanged }: { onClose: () => void; onChanged: () => void }) {
  const [current, setCurrent] = useState(''); const [next, setNext] = useState(''); const [error, setError] = useState(''); const [loading, setLoading] = useState(false)
  const submit = async (event: FormEvent) => { event.preventDefault(); setLoading(true); setError(''); try { await api('/api/admin/password', { method: 'PUT', body: JSON.stringify({ current_password: current, new_password: next }) }); onChanged() } catch (cause) { setError(cause instanceof Error ? cause.message : '修改失败') } finally { setLoading(false) } }
  return <Modal title="修改后台密码" onClose={onClose}><form className="modal-body" onSubmit={submit}><label>当前密码<input type="password" autoComplete="current-password" value={current} onChange={(event) => setCurrent(event.target.value)} /></label><label>新密码<input type="password" autoComplete="new-password" value={next} onChange={(event) => setNext(event.target.value)} placeholder="至少 12 个字符" /></label>{error && <div className="form-error">{error}</div>}<div className="modal-actions"><button className="secondary-button" type="button" onClick={onClose}>取消</button><button className="primary-button" type="submit" disabled={!current || next.length < 12 || loading}>保存密码</button></div></form></Modal>
}

function QuotaWindow({ title, window }: { title: string; window?: RateWindow | null }) {
  if (!window) return <div className="quota-item unavailable"><div><strong>{title}</strong><span>暂无数据</span></div></div>
  const used = Math.max(0, Math.min(100, window.used_percent ?? 0)); const reset = window.reset_at ? window.reset_at * 1000 : Date.now() + (window.reset_after_seconds ?? 0) * 1000
  return <div className="quota-item"><div className="quota-label"><strong>{title}</strong><span>{used.toFixed(1)}% 已用</span></div><div className="quota-track"><i style={{ width: `${used}%` }} className={used >= 90 ? 'danger' : used >= 70 ? 'warning' : ''} /></div><small>重置时间 {formatDate(reset)}</small></div>
}

function PageLoading() { return <div className="page-loading"><LoaderCircle className="spin" size={22} />加载账号信息…</div> }
function formatDate(value: number) { return new Date(value).toLocaleString('zh-CN', { hour12: false }) }
