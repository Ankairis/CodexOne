import { FormEvent, useCallback, useEffect, useState } from 'react'
import { Check, Clipboard, KeyRound, LoaderCircle, Plus, ShieldAlert, Trash2 } from 'lucide-react'
import { api, copyText } from '../api'
import { APIKey } from '../types'
import { Modal } from '../components/Modal'

export function Keys({ notify }: { notify: (message: string) => void }) {
  const [keys, setKeys] = useState<APIKey[]>([])
  const [loading, setLoading] = useState(true)
  const [createOpen, setCreateOpen] = useState(false)
  const [secret, setSecret] = useState('')
  const [error, setError] = useState('')
  const load = useCallback(async () => { try { setKeys((await api<{ keys: APIKey[] }>('/api/admin/keys')).keys) } catch (cause) { setError(cause instanceof Error ? cause.message : '加载失败') } finally { setLoading(false) } }, [])
  useEffect(() => { void load() }, [load])

  const revoke = async (key: APIKey) => {
    if (!window.confirm(`撤销 API Key“${key.name}”？现有客户端会立即无法访问。`)) return
    try { await api(`/api/admin/keys/${encodeURIComponent(key.id)}`, { method: 'DELETE' }); await load(); notify('API Key 已撤销') } catch (cause) { setError(cause instanceof Error ? cause.message : '撤销失败') }
  }

  return (
    <div className="page-stack">
      <div className="page-title-row"><div><h1>API Keys</h1><p>管理访问 /v1 的客户端凭据</p></div><button className="primary-button" type="button" onClick={() => setCreateOpen(true)}><Plus size={17} />创建 Key</button></div>
      {error && <div className="notice error">{error}<button type="button" onClick={() => setError('')}>×</button></div>}
      <section className="panel key-panel">
        <div className="panel-header"><div><h2>访问密钥</h2><span>密钥明文只在创建时显示一次</span></div><span className="count-badge">{keys.filter((key) => !key.revoked_at).length} 个有效</span></div>
        <div className="table-scroll">
          <table>
            <thead><tr><th>名称</th><th>前缀</th><th>创建时间</th><th>最近使用</th><th>状态</th><th aria-label="操作" /></tr></thead>
            <tbody>
              {!loading && keys.length === 0 && <tr><td colSpan={6} className="empty-cell"><KeyRound size={22} />还没有 API Key</td></tr>}
              {keys.map((key) => <tr key={key.id}><td><strong>{key.name}</strong></td><td><code>{key.prefix}••••••</code></td><td>{formatDate(key.created_at)}</td><td>{key.last_used_at ? formatDate(key.last_used_at) : '从未'}</td><td>{key.revoked_at ? <span className="status muted"><i />已撤销</span> : <span className="status ok"><i />有效</span>}</td><td className="action-cell">{!key.revoked_at && <button className="icon-button danger" type="button" onClick={() => void revoke(key)} title="撤销"><Trash2 size={16} /></button>}</td></tr>)}
            </tbody>
          </table>
        </div>
      </section>
      <section className="security-band"><ShieldAlert size={19} /><div><strong>Key 不等于上游账号</strong><span>这些 Key 只控制你自己的客户端是否能访问。无论创建多少 Key，上游始终只有同一个 Codex 账号。</span></div></section>
      {createOpen && <CreateKeyModal onClose={() => setCreateOpen(false)} onCreated={async (value) => { setCreateOpen(false); setSecret(value); await load() }} />}
      {secret && <SecretModal secret={secret} onClose={() => setSecret('')} notify={notify} />}
    </div>
  )
}

function CreateKeyModal({ onClose, onCreated }: { onClose: () => void; onCreated: (secret: string) => void }) {
  const [name, setName] = useState(''); const [loading, setLoading] = useState(false); const [error, setError] = useState('')
  const submit = async (event: FormEvent) => { event.preventDefault(); setLoading(true); setError(''); try { const result = await api<{ secret: string }>('/api/admin/keys', { method: 'POST', body: JSON.stringify({ name }) }); onCreated(result.secret) } catch (cause) { setError(cause instanceof Error ? cause.message : '创建失败') } finally { setLoading(false) } }
  return <Modal title="创建 API Key" onClose={onClose}><form className="modal-body" onSubmit={submit}><label>名称<input autoFocus value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：我的 MacBook" maxLength={64} /></label>{error && <div className="form-error">{error}</div>}<div className="modal-actions"><button className="secondary-button" type="button" onClick={onClose}>取消</button><button className="primary-button" type="submit" disabled={!name.trim() || loading}>{loading ? <LoaderCircle className="spin" size={16} /> : <Plus size={16} />}创建</button></div></form></Modal>
}

function SecretModal({ secret, onClose, notify }: { secret: string; onClose: () => void; notify: (message: string) => void }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => { await copyText(secret); setCopied(true); notify('API Key 已复制') }
  return <Modal title="保存你的 API Key" onClose={onClose}><div className="modal-body"><div className="notice warning"><ShieldAlert size={17} />关闭窗口后无法再次查看完整 Key。</div><button className="secret-value" type="button" onClick={() => void copy()}><code>{secret}</code>{copied ? <Check size={18} /> : <Clipboard size={18} />}</button><button className="primary-button full" type="button" onClick={onClose}>我已保存</button></div></Modal>
}

function formatDate(value: number) { return new Date(value).toLocaleString('zh-CN', { hour12: false }) }
