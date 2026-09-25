import { useEffect, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router'
import { ArrowLeft, CircleCheck, Plus, Save, Search, Trash2, TriangleAlert } from 'lucide-react'
import { api, useResource } from '../api'
import { Button, Card, ErrorBox, Field, PageHeader, Pill, Spinner, cx, inputCls } from '../components/ui'
import { shortContext } from '../format'
import { useCan } from '../session'
import type { Candidate, ClusterSettings, GitOpsSettings, GitOpsTarget } from '../types'

function targetLabel(t: GitOpsTarget) {
  return t.kind === 'terraform' ? `${t.file}: ${t.address}.${t.attribute}` : `${t.file}: ${t.path}`
}

const roleCls: Record<string, string> = {
  'control-plane': 'bg-violet-500/10 text-violet-700 dark:text-violet-300',
  'node-group': 'bg-teal-500/10 text-teal-700 dark:text-teal-300',
  addon: 'bg-sky-500/10 text-sky-700 dark:text-sky-300',
}

const emptyGitOps: GitOpsSettings = { provider: 'github', owner: '', repo: '', targets: [] }

export function ClusterSettingsPage() {
  const [params] = useSearchParams()
  const context = params.get('context') ?? ''
  const can = useCan()
  const navigate = useNavigate()
  const admin = can('admin')
  const loaded = useResource(() => api.settings(context), [context])
  const [s, setS] = useState<ClusterSettings>()
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<Error>()
  const [discovering, setDiscovering] = useState(false)
  const [cands, setCands] = useState<Candidate[]>()
  const [discoverWarn, setDiscoverWarn] = useState<string[]>()

  useEffect(() => {
    if (loaded.data) setS(loaded.data.settings)
  }, [loaded.data])

  if (loaded.loading && !s) return <Spinner />
  if (loaded.error) return <ErrorBox error={loaded.error} />
  if (!s) return null

  const g = s.gitops
  const setG = (patch: Partial<GitOpsSettings>) => setS({ ...s, gitops: { ...(g ?? emptyGitOps), ...patch } })
  const hasTarget = (t: GitOpsTarget) => g?.targets.some((x) => targetLabel(x) === targetLabel(t))

  const save = async () => {
    setSaving(true)
    setError(undefined)
    setSaved(false)
    try {
      const clean: ClusterSettings = { ...s, environment: s.environment?.trim() || undefined }
      if (clean.gitops && !clean.gitops.owner && !clean.gitops.repo) delete clean.gitops
      setS(await api.saveSettings(clean))
      setSaved(true)
      loaded.reload()
    } catch (e) {
      setError(e as Error)
    } finally {
      setSaving(false)
    }
  }

  const discover = async () => {
    if (!g) return
    setDiscovering(true)
    setError(undefined)
    setCands(undefined)
    try {
      const r = await api.discover(context, g)
      setCands(r.candidates)
      setDiscoverWarn(r.warnings)
      if (!g.baseBranch) setG({ baseBranch: r.branch })
    } catch (e) {
      setError(e as Error)
    } finally {
      setDiscovering(false)
    }
  }

  return (
    <>
      <Link to="/fleet" className="mb-4 inline-flex items-center gap-1 text-sm text-muted hover:text-fg">
        <ArrowLeft className="size-4" /> Fleet
      </Link>
      <PageHeader
        title="Cluster settings"
        subtitle={<span className="font-mono break-all">{shortContext(context)}</span>}
        actions={
          admin && (
            <Button variant="primary" onClick={save} loading={saving} icon={<Save className="size-4" />}>
              Save
            </Button>
          )
        }
      />
      {!admin && (
        <div className="mb-4 rounded-lg border border-line bg-panel-2/60 px-4 py-3 text-sm text-muted">Only administrators can change cluster settings.</div>
      )}
      {error && (
        <div className="mb-4">
          <ErrorBox error={error} />
        </div>
      )}
      {saved && (
        <div className="mb-4 flex items-center gap-2 rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-800 dark:text-emerald-200">
          <CircleCheck className="size-4" /> Settings saved.
        </div>
      )}

      <fieldset disabled={!admin} className="space-y-6">
        <Card title="General">
          <div className="grid gap-4 p-5 sm:grid-cols-2">
            <Field label="Environment" hint="Used by approval policies, e.g. prod or staging.">
              <input value={s.environment ?? ''} onChange={(e) => setS({ ...s, environment: e.target.value })} placeholder="prod" className={inputCls} />
            </Field>
          </div>
        </Card>

        <Card title="Cloud identity for direct upgrades">
          <div className="space-y-4 p-5">
            <p className="text-sm text-muted">
              EKS and GKE clusters are identified from the kubeconfig automatically. AKS needs the subscription and resource group.
            </p>
            <div className="grid gap-4 sm:grid-cols-3">
              <Field label="AKS subscription ID">
                <input value={s.aks?.subscriptionId ?? ''} onChange={(e) => setS({ ...s, aks: { ...(s.aks ?? { resourceGroup: '', name: '' }), subscriptionId: e.target.value } })} className={cx(inputCls, 'font-mono')} />
              </Field>
              <Field label="Resource group">
                <input value={s.aks?.resourceGroup ?? ''} onChange={(e) => setS({ ...s, aks: { ...(s.aks ?? { subscriptionId: '', name: '' }), resourceGroup: e.target.value } })} className={cx(inputCls, 'font-mono')} />
              </Field>
              <Field label="AKS cluster name">
                <input value={s.aks?.name ?? ''} onChange={(e) => setS({ ...s, aks: { ...(s.aks ?? { subscriptionId: '', resourceGroup: '' }), name: e.target.value } })} className={cx(inputCls, 'font-mono')} />
              </Field>
            </div>
            {s.aks && !s.aks.subscriptionId && !s.aks.resourceGroup && !s.aks.name && (
              <button type="button" className="text-xs text-muted underline" onClick={() => setS({ ...s, aks: undefined })}>
                Clear AKS identity
              </button>
            )}
          </div>
        </Card>

        <Card title="GitOps: upgrade through pull requests">
          <div className="space-y-5 p-5">
            <p className="text-sm text-muted">
              Jin opens one pull request per stage against this repository, waits for it to be merged and for your pipeline to apply it, then verifies
              the cluster. It uses the GitHub token from Integrations (stored encrypted) or the server environment.
            </p>
            <div className="grid gap-4 sm:grid-cols-3">
              <Field label="Owner">
                <input value={g?.owner ?? ''} onChange={(e) => setG({ owner: e.target.value })} placeholder="acme" className={inputCls} />
              </Field>
              <Field label="Repository">
                <input value={g?.repo ?? ''} onChange={(e) => setG({ repo: e.target.value })} placeholder="infrastructure" className={inputCls} />
              </Field>
              <Field label="Base branch" hint="Defaults to the repository's default branch.">
                <input value={g?.baseBranch ?? ''} onChange={(e) => setG({ baseBranch: e.target.value || undefined })} placeholder="main" className={inputCls} />
              </Field>
              <Field
                label="Token environment variable (optional)"
                hint={
                  loaded.data?.tokenPresent === false ? (
                    <span className="text-amber-600">
                      No token available. <Link className="underline" to="/integrations">Add one under Integrations</Link>.
                    </span>
                  ) : loaded.data?.tokenSource === 'jin' ? (
                    'Using the token stored in Jin. Set a GITHUB_… variable only to override it.'
                  ) : (
                    `Using ${loaded.data?.tokenSource?.replace('env:', '$') ?? 'the server environment'}.`
                  )
                }
              >
                <input value={g?.tokenEnv ?? ''} onChange={(e) => setG({ tokenEnv: e.target.value || undefined })} placeholder="Stored token" className={cx(inputCls, 'font-mono')} />
              </Field>
              <Field label="GitHub Enterprise API URL" hint="Leave empty for github.com.">
                <input value={g?.baseUrl ?? ''} onChange={(e) => setG({ baseUrl: e.target.value || undefined })} placeholder="https://ghe.example.com/api/v3" className={inputCls} />
              </Field>
              <Field label="Apply timeout (minutes)" hint="How long to wait for the pipeline after a merge.">
                <input type="number" min={5} value={g?.applyTimeoutMinutes ?? ''} onChange={(e) => setG({ applyTimeoutMinutes: e.target.value ? Number(e.target.value) : undefined })} placeholder="180" className={inputCls} />
              </Field>
            </div>

            <div>
              <div className="mb-2 flex items-center justify-between">
                <h3 className="text-sm font-semibold">Version targets</h3>
                <Button type="button" onClick={discover} loading={discovering} disabled={!g?.owner || !g?.repo} icon={<Search className="size-4" />}>
                  Discover in repository
                </Button>
              </div>
              {!g?.targets.length && <p className="rounded-lg border border-dashed border-line px-4 py-6 text-center text-sm text-muted">No targets yet. Discover them from the repository.</p>}
              <ul className="divide-y divide-line rounded-lg border border-line">
                {g?.targets.map((t, i) => (
                  <li key={i} className="flex items-center gap-3 px-4 py-2.5 text-sm">
                    <span className={cx('rounded px-1.5 py-0.5 text-[11px] font-semibold', roleCls[t.role])}>{t.role}</span>
                    {t.name && <Pill>{t.name}</Pill>}
                    <span className="min-w-0 flex-1 truncate font-mono text-xs">{targetLabel(t)}</span>
                    <button type="button" className="rounded p-1 text-muted hover:text-red-600" onClick={() => setG({ targets: g.targets.filter((_, j) => j !== i) })} aria-label="Remove target">
                      <Trash2 className="size-4" />
                    </button>
                  </li>
                ))}
              </ul>
            </div>

            {cands && (
              <div className="rounded-lg border border-line bg-bg/40 p-4">
                <h3 className="mb-2 text-sm font-semibold">
                  Found {cands.length} version field{cands.length === 1 ? '' : 's'}
                </h3>
                {discoverWarn?.map((w, i) => (
                  <p key={i} className="mb-2 flex items-center gap-1.5 text-xs text-amber-700 dark:text-amber-300">
                    <TriangleAlert className="size-3.5" /> {w}
                  </p>
                ))}
                <ul className="space-y-1.5">
                  {cands.map((c, i) => (
                    <li key={i} className="flex flex-wrap items-center gap-2 text-sm">
                      <span className={cx('rounded px-1.5 py-0.5 text-[11px] font-semibold', roleCls[c.target.role])}>{c.target.role}</span>
                      {c.suggested && <Pill className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">matches this cluster</Pill>}
                      <span className="font-mono text-xs">{targetLabel(c.target)}</span>
                      <span className="text-xs text-muted">{c.error ? c.error : `= ${c.current}`}</span>
                      {c.cluster && <span className="text-xs text-muted">({c.cluster})</span>}
                      {!c.error && (
                        <Button
                          type="button"
                          className="ml-auto px-2 py-1 text-xs"
                          disabled={hasTarget(c.target)}
                          icon={<Plus className="size-3.5" />}
                          onClick={() => setG({ targets: [...(g?.targets ?? []), c.target] })}
                        >
                          {hasTarget(c.target) ? 'Added' : 'Add'}
                        </Button>
                      )}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        </Card>
      </fieldset>

      {admin && loaded.data?.registered && (
        <Card className="mt-6 border-red-500/30" title="Remove cluster">
          <div className="flex flex-wrap items-center justify-between gap-4 p-5 text-sm">
            <p className="text-muted">Stops managing this cluster in Jin and deletes its settings. Nothing changes in the cluster or in AWS.</p>
            <Button
              variant="danger"
              icon={<Trash2 className="size-4" />}
              onClick={async () => {
                if (!window.confirm('Remove this cluster from Jin?')) return
                await api.removeCluster(context)
                navigate('/fleet')
              }}
            >
              Remove from Jin
            </Button>
          </div>
        </Card>
      )}
    </>
  )
}
