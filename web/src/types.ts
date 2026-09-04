export type SessionInfo = {
  authenticated: boolean
  base_url: string
  storage: 'sqlite' | 'postgres'
  client: string
}

export type TodayStats = {
  requests: number
  successes: number
  success_rate: number
  average_ms: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
}

export type RequestEntry = {
  id: string
  request_id: string
  api_key_id?: string
  api_key_name?: string
  method: string
  path: string
  model?: string
  status: number
  duration_ms: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  reasoning_effort?: string
  upstream_reasoning_effort?: string
  first_reasoning_ms?: number
  first_output_ms?: number
  error?: string
  created_at: number
}

export type OverviewData = {
  date: string
  stats: TodayStats
  requests: RequestEntry[]
  base_url: string
  account_connected: boolean
}

export type LogEntry = {
  timestamp: number
  level: string
  message: string
  fields?: Record<string, unknown>
}

export type RateWindow = {
  used_percent: number
  limit_window_seconds?: number
  reset_after_seconds?: number
  reset_at?: number
}

export type RateLimit = {
  allowed?: boolean
  limit_reached?: boolean
  primary_window?: RateWindow | null
  secondary_window?: RateWindow | null
}

export type QuotaPayload = {
  plan_type?: string
  rate_limit?: RateLimit
  additional_rate_limits?: Array<{
    limit_name?: string
    metered_feature?: string
    rate_limit?: RateLimit
  }>
}

export type AccountInfo = {
  connected: boolean
  email?: string
  account_id?: string
  plan_type?: string
  expires_at?: number
  updated_at?: number
  quota?: QuotaPayload
  quota_fetched_at?: number
  client_name: string
}

export type DeviceFlow = {
  flow_id: string
  user_code: string
  verification_url: string
  expires_at: number
  poll_interval: number
}

export type BrowserOAuthFlow = {
  flow_id: string
  authorization_url: string
  redirect_uri: string
  expires_at: number
}

export type APIKey = {
  id: string
  name: string
  prefix: string
  created_at: number
  last_used_at?: number
  revoked_at?: number
}
