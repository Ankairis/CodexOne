import { useCallback, useEffect, useMemo, useState } from 'react'
import { Activity, CheckCircle2, Clock3, Copy, KeyRound, RefreshCw, TerminalSquare, Zap } from 'lucide-react'
import { api, copyText } from '../api'
import { LogEntry, OverviewData, RequestEntry } from '../types'

const today = () => {
  const date = new Date()
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
}

export function Overview({ notify }: { notify: (message: string) => void }) {
  const [date, setDate] = useState(today())
  const [data, setData] = useState<OverviewData | null>(null)
  const [logs, setLogs] = useState<LogEntry[]>([])
  const [selected, setSelected] = useState<RequestEntry | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const [overview, applicationLogs] = await Promise.all([
        api<OverviewData>(`/api/admin/overview?date=${encodeURIComponent(date)}`),
        api<{ logs: LogEntry[] }>('/api/admin/logs'),
      ])
      setData(overview)
      setLogs(applicationLogs.logs)
      setSelected((current) => current ? overview.requests.find((item) => item.id === current.id) ?? null : null)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [date])

  useEffect(() => { void load() }, [load])

  const stats = data?.stats
  const totalTokens = (stats?.input_tokens ?? 0) + (stats?.output_tokens ?? 0)
  const logLines = useMemo(() => logs.map((entry) => {
    const stamp = new Date(entry.timestamp).toLocaleTimeString('zh-CN', { hour12: false })
    const fields = entry.fields && Object.keys(entry.fields).length ? ` ${JSON.stringify(entry.fields)}` : ''
    return `${stamp}  ${entry.level.padEnd(5)}  ${entry.message}${fields}`
  }), [logs])

  return (
    <div className="page-stack">
      <div className="page-title-row">
        <div><h1>总览</h1><p>当天请求、延迟和运行日志</p></div>
        <div className="page-actions">
          <input className="date-input" type="date" value={date} onChange={(event) => setDate(event.target.value)} />
          <button className="secondary-button" type="button" onClick={() => void load()} disabled={loading}><RefreshCw className={loading ? 'spin' : ''} size={16} />刷新</button>
        </div>
      </div>
      {error && <div className="notice error">{error}</div>}
      <section className="metrics-grid" aria-label="今日统计">
        <Metric icon={Activity} label="请求数" value={formatNumber(stats?.requests ?? 0)} hint={data?.account_connected ? '账号已连接' : '账号未连接'} />
        <Metric icon={CheckCircle2} label="成功率" value={`${(stats?.success_rate ?? 0).toFixed(1)}%`} hint={`${formatNumber(stats?.successes ?? 0)} 次成功`} tone="green" />
        <Metric icon={Clock3} label="平均延迟" value={`${Math.round(stats?.average_ms ?? 0)} ms`} hint="端到端耗时" tone="amber" />
        <Metric icon={Zap} label="Token" value={formatNumber(totalTokens)} hint={`${formatNumber(stats?.input_tokens ?? 0)} 输入 / ${formatNumber(stats?.output_tokens ?? 0)} 输出 · ${formatNumber(stats?.reasoning_tokens ?? 0)} 推理`} tone="violet" />
      </section>
      <section className="panel request-panel">
        <div className="panel-header"><div><h2>请求详情</h2><span>{data?.requests.length ?? 0} 条记录</span></div>{data && <button className="quiet-button" type="button" onClick={async () => { await copyText(data.base_url); notify('Base URL 已复制') }}><Copy size={15} />复制 Base URL</button>}</div>
        <div className="table-scroll">
          <table>
            <thead><tr><th>时间</th><th>状态</th><th>模型</th><th>API Key</th><th>延迟</th><th>推理</th><th>Token</th><th>Request ID</th></tr></thead>
            <tbody>
              {!loading && !data?.requests.length && <tr><td className="empty-cell" colSpan={8}>这一天还没有请求</td></tr>}
              {data?.requests.map((entry) => (
                <tr key={entry.id} className={selected?.id === entry.id ? 'selected-row' : ''} onClick={() => setSelected(entry)}>
                  <td>{formatTime(entry.created_at)}</td>
                  <td><Status status={entry.status} /></td>
                  <td><code className="model-name">{entry.model || '—'}</code></td>
                  <td>{entry.api_key_name || '—'}</td>
                  <td>{entry.duration_ms} ms</td>
                  <td><code>{reasoningLabel(entry)}</code>{entry.reasoning_tokens > 0 ? ` · ${formatNumber(entry.reasoning_tokens)}` : ''}</td>
                  <td>{formatNumber(entry.input_tokens + entry.output_tokens)}</td>
                  <td><code className="request-id">{entry.request_id}</code></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {selected && <div className="request-detail"><div><span>路径</span><code>{selected.method} {selected.path}</code></div><div><span>输入 / 输出 / 推理</span><strong>{formatNumber(selected.input_tokens)} / {formatNumber(selected.output_tokens)} / {formatNumber(selected.reasoning_tokens)}</strong></div><div><span>请求 / 上游强度</span><code>{selected.reasoning_effort || '默认'} / {selected.upstream_reasoning_effort || '未回显'}</code></div><div><span>首个思考 / 正文</span><strong>{formatLatency(selected.first_reasoning_ms)} / {formatLatency(selected.first_output_ms)}</strong></div><div><span>完整 Request ID</span><code>{selected.request_id}</code></div>{selected.error && <div className="detail-error"><span>错误</span><code>{selected.error}</code></div>}</div>}
      </section>
      <section className="panel log-panel">
        <div className="panel-header"><div><h2><TerminalSquare size={17} />运行日志</h2><span>只显示最近 300 条，不记录请求正文</span></div><span className="live-indicator"><i />LIVE</span></div>
        <pre className="log-viewer">{logLines.length ? logLines.join('\n') : '等待服务日志…'}</pre>
      </section>
    </div>
  )
}

function Metric({ icon: Icon, label, value, hint, tone = '' }: { icon: typeof Activity; label: string; value: string; hint: string; tone?: string }) {
  return <div className={`metric ${tone}`}><div className="metric-icon"><Icon size={18} /></div><div><span>{label}</span><strong>{value}</strong><small>{hint}</small></div></div>
}

function Status({ status }: { status: number }) {
  const tone = status >= 200 && status < 300 ? 'ok' : status === 499 ? 'muted' : 'bad'
  return <span className={`status ${tone}`}><i />{status}</span>
}

function formatNumber(value: number) { return new Intl.NumberFormat('zh-CN', { notation: value >= 10000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(value) }
function formatTime(value: number) { return new Date(value).toLocaleTimeString('zh-CN', { hour12: false }) }
function formatLatency(value?: number) { return value && value > 0 ? `${value} ms` : '—' }
function reasoningLabel(entry: RequestEntry) {
  const requested = entry.reasoning_effort || '默认'
  return entry.upstream_reasoning_effort && entry.upstream_reasoning_effort !== requested
    ? `${requested} → ${entry.upstream_reasoning_effort}`
    : entry.upstream_reasoning_effort || requested
}
