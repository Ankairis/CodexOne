export class APIError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: 'same-origin',
    ...init,
    headers: {
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...init?.headers,
    },
  })
  if (response.status === 204) return undefined as T

  const contentType = response.headers.get('content-type') ?? ''
  const payload = contentType.includes('application/json')
    ? await response.json()
    : await response.text()
  if (!response.ok) {
    const message = typeof payload === 'object'
      ? payload?.error?.message ?? payload?.error ?? '请求失败'
      : payload || '请求失败'
    if (response.status === 401) window.dispatchEvent(new Event('codexone:unauthorized'))
    throw new APIError(message, response.status)
  }
  return payload as T
}

export function copyText(value: string): Promise<void> {
  return navigator.clipboard.writeText(value)
}
