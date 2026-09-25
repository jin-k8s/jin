import { useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { Boxes, FileSearch, Plus, RefreshCw, Settings2, X } from 'lucide-react'
import { AddClusterDialog } from '../components/AddClusterDialog'
import { api, useResource } from '../api'
import { Button, Card, Empty, EnvBadge, ErrorBox, PageHeader, Pill, ProviderBadge, Spinner, StatusBadge, cx, inputCls } from '../components/ui'
import { timeAgo } from '../format'
import { useCan } from '../session'
import type { FleetRow, SupportAssessment } from '../types'

function displayName(r: FleetRow) {
  return r.eks?.name ?? r.gke?.name ?? r.aks?.name ?? r.name
}

export function PlanDialog({ ctx, onClose }: { ctx: FleetRow; onClose: () => void }) {
  const navigate = useNavigate()
  const [target, setTarget] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()

  const submit = async () => {
    setBusy(true)
    setError(undefined)
    try {
      const rec = await api.plan(ctx.name, target.trim())
      navigate(`/plans/${rec.id}`)
    } catch (e) {
      setError(e as Error)
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4 backdrop-blur-sm" role="dialog" aria-modal="true">
      <div className="w-full max-w-md rounded-xl border border-line bg-panel shadow-xl">
        <div className="flex items-center justify-between border-b border-line px-5 py-4">
          <h2 className="font-semibold">Plan an upgrade</h2>
          <button onClick={onClose} className="rounded-md p-1 text-muted hover:bg-panel-2" aria-label="Close" disabled={busy}>
            <X className="size-4" />
          </button>
        </div>
        <div className="space-y-4 px-5 py-5">
          <div>
            <div className="text-xs font-medium tracking-wide text-muted uppercase">Cluster</div>
            <div className="mt-1 flex items-center gap-2 font-medium break-all">
              {displayName(ctx)} <ProviderBadge provider={ctx.provider} />
            </div>
          </div>
          <label className="block">
            <span className="text-sm font-medium">Target version</span>
            <input value={target} onChange={(e) => setTarget(e.target.value)} placeholder="Next minor version" className={cx(inputCls, 'mt-1.5 font-mono')} disabled={busy} />
            <span className="mt-1 block text-xs text-muted">For example 1.33. Leave empty to plan the next minor version.</span>
          </label>
          <p className="text-xs text-muted">Jin inspects the cluster with read-only API calls. Nothing is changed.</p>
          {busy && <Spinner label="Inspecting the cluster… this usually takes under a minute" />}
          {error && <ErrorBox error={error} title="Planning failed" />}
        </div>
        <div className="flex justify-end gap-2 border-t border-line px-5 py-4">
          <Button onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="primary" onClick={submit} loading={busy} icon={<FileSearch className="size-4" />}>
            Build plan
          </Button>
        </div>
      </div>
    </div>
  )
}

export function SupportCell({ s }: { s?: SupportAssessment }) {
  if (!s || !s.currentStatus) return <span className="text-muted">–</span>
  if (s.currentStatus === 'extended-support') {
    return (
      <div>
        <span className="text-xs font-semibold text-red-600 dark:text-red-400">Extended support</span>
        <div className="text-xs text-muted">+${Math.round(s.currentSurchargePerYearUsd).toLocaleString()}/yr</div>
      </div>
    )
  }
  if (s.currentStatus === 'unsupported') return <span className="text-xs font-semibold text-red-600 dark:text-red-400">Unsupported</span>
  const days = s.daysToEndOfStandardSupport
  return (
    <div>
      <span className={cx('text-xs font-semibold', days !== undefined && days <= 90 ? 'text-amber-600 dark:text-amber-400' : 'text-emerald-600 dark:text-emerald-400')}>
        Standard support
      </span>
      {days !== undefined && <div className="text-xs text-muted">{days} days left</div>}
    </div>
  )
}

export function Fleet() {
  const can = useCan()
  const [refresh, setRefresh] = useState(0)
  const fleet = useResource(() => api.fleet(refresh > 0), [refresh], 60000)
  const [planning, setPlanning] = useState<FleetRow>()
  const [adding, setAdding] = useState(false)
  const rows = fleet.data ?? []
  const extended = rows.filter((r) => r.lastPlan?.support?.currentStatus === 'extended-support')
  const surcharge = extended.reduce((a, r) => a + (r.lastPlan?.support?.currentSurchargePerYearUsd ?? 0), 0)

  return (
    <>
      <PageHeader
        title="Fleet"
        subtitle="Clusters added in Jin and from your kubeconfig: live version, support window, last plan and active upgrade."
        actions={
          <>
            <Button onClick={() => setRefresh((n) => n + 1)} loading={fleet.loading && !!fleet.data} icon={<RefreshCw className="size-4" />}>
              Refresh
            </Button>
            {can('admin') && (
              <Button variant="primary" onClick={() => setAdding(true)} icon={<Plus className="size-4" />}>
                Add cluster
              </Button>
            )}
          </>
        }
      />
      {surcharge > 0 && (
        <div className="mb-4 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-800 dark:text-red-200">
          <span className="font-semibold">
            {extended.length} cluster{extended.length > 1 ? 's are' : ' is'} in EKS extended support, about ${Math.round(surcharge).toLocaleString()} per year extra.
          </span>{' '}
          Upgrading returns them to standard pricing.
        </div>
      )}
      <Card>
        {fleet.loading && !fleet.data && <Spinner label="Contacting clusters…" />}
        {fleet.error && (
          <div className="p-4">
            <ErrorBox error={fleet.error} />
          </div>
        )}
        {fleet.data?.length === 0 && (
          <Empty icon={<Boxes className="size-5" />} title="No clusters yet">
            Use <b>Add cluster</b> for EKS, or add a kubeconfig context with <code className="font-mono">gcloud container clusters get-credentials</code> or{' '}
            <code className="font-mono">az aks get-credentials</code>.
          </Empty>
        )}
        {rows.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-line text-left text-xs tracking-wide text-muted uppercase">
                  <th className="px-5 py-3 font-medium">Cluster</th>
                  <th className="px-5 py-3 font-medium">Version</th>
                  <th className="px-5 py-3 font-medium">Support</th>
                  <th className="px-5 py-3 font-medium">Last plan</th>
                  <th className="px-5 py-3 font-medium">Upgrade</th>
                  <th className="px-5 py-3" />
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {rows.map((r) => (
                  <tr key={r.name} className="hover:bg-panel-2/60">
                    <td className="max-w-xs px-5 py-3.5">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="truncate font-medium">{displayName(r)}</span>
                        <ProviderBadge provider={r.provider} />
                        <EnvBadge env={r.environment} />
                        {r.gitops && <Pill>GitOps</Pill>}
                        {r.registered && <Pill>added in Jin</Pill>}
                        {r.current && <Pill className="bg-brand/10 text-brand">current</Pill>}
                      </div>
                      <div className="mt-0.5 truncate font-mono text-xs text-muted" title={r.name}>
                        {r.eks?.region ?? r.gke?.location ?? r.aks?.resourceGroup ?? r.name} · policy {r.policy}
                      </div>
                    </td>
                    <td className="px-5 py-3.5">
                      {r.reachable ? (
                        <div>
                          <span className="font-mono font-semibold">{r.version}</span>
                          <div className="text-xs text-muted">{r.nodes} nodes</div>
                        </div>
                      ) : (
                        <span className="text-xs text-red-600 dark:text-red-400" title={r.error}>
                          unreachable
                        </span>
                      )}
                    </td>
                    <td className="px-5 py-3.5">
                      <SupportCell s={r.lastPlan?.support} />
                    </td>
                    <td className="px-5 py-3.5">
                      {r.lastPlan ? (
                        <Link to={`/plans/${r.lastPlan.id}`} className="hover:underline">
                          {r.lastPlan.status === 'failed' ? (
                            <span className="text-xs font-semibold text-red-600">failed</span>
                          ) : (
                            <span className={cx('text-xs font-semibold', r.lastPlan.blockers ? 'text-red-600 dark:text-red-400' : 'text-emerald-600 dark:text-emerald-400')}>
                              → {r.lastPlan.target} · {r.lastPlan.blockers ? `${r.lastPlan.blockers} blockers` : 'ready'}
                            </span>
                          )}
                          <div className="text-xs text-muted">{timeAgo(r.lastPlan.startedAt)}</div>
                        </Link>
                      ) : (
                        <span className="text-muted">–</span>
                      )}
                    </td>
                    <td className="px-5 py-3.5">
                      {r.activeUpgrade ? (
                        <Link to={`/upgrades/${r.activeUpgrade.id}`}>
                          <StatusBadge status={r.activeUpgrade.status} />
                          <div className="mt-0.5 text-xs text-muted">
                            hop {r.activeUpgrade.currentHop}/{r.activeUpgrade.hops} → {r.activeUpgrade.to}
                          </div>
                        </Link>
                      ) : (
                        <span className="text-muted">–</span>
                      )}
                    </td>
                    <td className="px-5 py-3.5 text-right whitespace-nowrap">
                      <Link to={`/clusters/settings?context=${encodeURIComponent(r.name)}`} className="mr-2 inline-flex rounded-md p-2 text-muted hover:bg-panel-2 hover:text-fg" title="Cluster settings" aria-label="Cluster settings">
                        <Settings2 className="size-4" />
                      </Link>
                      {can('planner') && (
                        <Button onClick={() => setPlanning(r)} icon={<FileSearch className="size-4" />}>
                          Plan
                        </Button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      {planning && <PlanDialog ctx={planning} onClose={() => setPlanning(undefined)} />}
      {adding && <AddClusterDialog onClose={() => setAdding(false)} onAdded={() => setRefresh((n) => n + 1)} />}
    </>
  )
}
