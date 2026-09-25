import { useCallback, useEffect, useRef, useState } from 'react'
import type {
  Assessment,
  AuditEntry,
  Candidate,
  ClusterSettings,
  FleetRow,
  GitHubIntegration,
  GitOpsSettings,
  Info,
  KubeContext,
  Parity,
  PolicyView,
  RunRecord,
  RunSummary,
  Session,
  Upgrade,
} from './types'

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

const unauthorizedListeners = new Set<() => void>()
export function onUnauthorized(fn: () => void) {
  unauthorizedListeners.add(fn)
  return () => {
    unauthorizedListeners.delete(fn)
  }
}

async function request<T>(method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (method !== 'GET') {
    headers['Content-Type'] = 'application/json'
    headers['X-Jin-Request'] = '1' // required by the server for cookie-authenticated writes (CSRF)
  }
  const res = await fetch(`/api/v1${path}`, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  let data: unknown = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    data = null
  }
  if (!res.ok) {
    if (res.status === 401) unauthorizedListeners.forEach((fn) => fn())
    const msg = (data as { error?: string } | null)?.error ?? res.statusText
    throw new ApiError(res.status, msg)
  }
  return data as T
}

const enc = encodeURIComponent

export const api = {
  session: () => request<Session>('GET', '/session'),
  logout: () =>
    fetch('/auth/logout', { method: 'POST', headers: { 'X-Jin-Request': '1' }, credentials: 'same-origin' }).then(() => undefined),
  fleet: (refresh = false) => request<FleetRow[]>('GET', `/fleet${refresh ? '?refresh=1' : ''}`),
  policies: () => request<{ policies: PolicyView[]; default: PolicyView }>('GET', '/policies'),
  settings: (context: string) =>
    request<{ settings: ClusterSettings; tokenPresent?: boolean; tokenSource?: string; registered?: boolean }>('GET', `/settings?context=${enc(context)}`),
  github: () => request<GitHubIntegration>('GET', '/integrations/github'),
  saveGitHub: (token: string) => request<GitHubIntegration>('PUT', '/integrations/github', { token }),
  removeGitHub: () => request<void>('DELETE', '/integrations/github'),
  awsProfiles: () => request<{ profiles: string[] }>('GET', '/aws/profiles'),
  eksClusters: (region: string, profile: string, roleArn: string) =>
    request<{ clusters: string[] }>('GET', `/aws/eks-clusters?region=${enc(region)}&profile=${enc(profile)}&roleArn=${enc(roleArn)}`),
  addCluster: (c: { provider: 'eks'; name: string; region: string; profile: string; roleArn: string }) =>
    request<{ context: string; version: string; endpoint: string }>('POST', '/clusters', c),
  removeCluster: (context: string) => request<void>('DELETE', `/clusters?context=${enc(context)}`),
  saveSettings: (s: ClusterSettings) => request<ClusterSettings>('PUT', '/settings', s),
  discover: (context: string, gitops: GitOpsSettings) =>
    request<{ branch: string; candidates: Candidate[]; warnings?: string[]; clusterHint: string }>('POST', '/settings/discover', { context, gitops }),
  compare: (source: string, target: string) => request<Parity>('POST', '/compare', { source, target }),
  assess: (context: string, target: string) => request<Assessment>('POST', '/assess', { context, target }),
  audit: () => request<AuditEntry[]>('GET', '/audit'),
  auditVerify: () => request<{ intact: boolean; brokenAt: number }>('GET', '/audit/verify'),
  info: () => request<Info>('GET', '/info'),
  contexts: () => request<KubeContext[]>('GET', '/contexts'),
  runs: () => request<RunSummary[]>('GET', '/runs'),
  run: (id: string) => request<RunRecord>('GET', `/runs/${enc(id)}`),
  plan: (context: string, target: string) => request<RunRecord>('POST', '/plans', { context, target }),
  upgrades: () => request<Upgrade[]>('GET', '/upgrades'),
  upgrade: (id: string) => request<Upgrade>('GET', `/upgrades/${enc(id)}`),
  createUpgrade: (runId: string, mode: 'direct' | 'gitops') => request<Upgrade>('POST', '/upgrades', { runId, mode }),
  approve: (id: string, body: { hop: number; confirm: string; comment: string; overrideBlockers: boolean }) =>
    request<Upgrade>('POST', `/upgrades/${enc(id)}/approve`, body),
  retry: (id: string, comment: string) => request<Upgrade>('POST', `/upgrades/${enc(id)}/retry`, { comment }),
  cancel: (id: string) => request<Upgrade>('POST', `/upgrades/${enc(id)}/cancel`),
  streamURL: (id: string) => `/api/v1/upgrades/${enc(id)}/stream`,
}

export interface Resource<T> {
  data: T | undefined
  error: ApiError | Error | undefined
  loading: boolean
  reload: () => void
}

/** Fetches on mount and when deps change; optionally re-polls in the background. */
export function useResource<T>(load: () => Promise<T>, deps: unknown[], pollMs?: number): Resource<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<Error>()
  const [loading, setLoading] = useState(true)
  const loadRef = useRef(load)
  loadRef.current = load

  const reload = useCallback(() => {
    loadRef
      .current()
      .then((d) => {
        setData(d)
        setError(undefined)
      })
      .catch((e: Error) => setError(e))
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    setLoading(true)
    reload()
    if (!pollMs) return
    const t = window.setInterval(reload, pollMs)
    return () => window.clearInterval(t)
  }, deps)

  return { data, error, loading, reload }
}
