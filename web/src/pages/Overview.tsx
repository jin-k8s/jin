import { Link } from 'react-router'
import { ArrowRight, Boxes, CirclePause, FileSearch, Rocket } from 'lucide-react'
import type { ReactNode } from 'react'
import { api, useResource } from '../api'
import { Card, Empty, ErrorBox, PageHeader, StatusBadge, Version } from '../components/ui'
import { shortContext, timeAgo } from '../format'

function Stat({ label, value, icon, tone }: { label: string; value: ReactNode; icon: ReactNode; tone: string }) {
  return (
    <div className="rounded-xl border border-line bg-panel p-5 shadow-sm">
      <div className="flex items-center justify-between">
        <span className="text-sm text-muted">{label}</span>
        <span className={`rounded-lg p-2 ${tone}`}>{icon}</span>
      </div>
      <div className="mt-3 text-3xl font-semibold tabular">{value}</div>
    </div>
  )
}

export function Overview() {
  const contexts = useResource(api.contexts, [])
  const runs = useResource(api.runs, [], 15000)
  const upgrades = useResource(api.upgrades, [], 5000)

  const ups = upgrades.data ?? []
  const attention = ups.filter((u) => u.status === 'awaiting-approval' || u.status === 'failed')
  const active = ups.filter((u) => u.status === 'running')
  const dash = (n: number | undefined) => (n === undefined ? '–' : n)

  return (
    <>
      <PageHeader title="Overview" subtitle="Plan upgrades, approve every hop, and follow them live." />

      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <Stat label="Clusters" value={dash(contexts.data?.length)} icon={<Boxes className="size-4" />} tone="bg-brand/10 text-brand" />
        <Stat label="Plans" value={dash(runs.data?.length)} icon={<FileSearch className="size-4" />} tone="bg-accent/10 text-accent" />
        <Stat label="Running upgrades" value={upgrades.data ? active.length : '–'} icon={<Rocket className="size-4" />} tone="bg-sky-500/10 text-sky-500" />
        <Stat label="Need attention" value={upgrades.data ? attention.length : '–'} icon={<CirclePause className="size-4" />} tone="bg-amber-500/10 text-amber-500" />
      </div>

      <div className="mt-6 grid gap-6 lg:grid-cols-2">
        <Card title="In progress">
          {upgrades.error && <div className="p-4"><ErrorBox error={upgrades.error} /></div>}
          {upgrades.data && attention.length + active.length === 0 && (
            <Empty icon={<Rocket className="size-5" />} title="Nothing waiting on you">
              Upgrades that need an approval or a decision after a failure show up here.
            </Empty>
          )}
          <ul className="divide-y divide-line">
            {[...attention, ...active].map((u) => (
              <li key={u.id}>
                <Link to={`/upgrades/${u.id}`} className="flex items-center gap-4 px-5 py-3.5 hover:bg-panel-2">
                  <div className="min-w-0 flex-1">
                    <div className="truncate font-medium">{shortContext(u.cluster.context)}</div>
                    <div className="mt-0.5 text-xs text-muted">
                      Hop {u.currentHop} of {u.hops.length} · updated {timeAgo(u.updatedAt)}
                    </div>
                  </div>
                  <Version from={u.from} to={u.to} />
                  <StatusBadge status={u.status} />
                  <ArrowRight className="size-4 text-muted" />
                </Link>
              </li>
            ))}
          </ul>
        </Card>

        <Card title="Recent plans" actions={<Link to="/plans" className="text-xs font-medium text-brand hover:underline">View all</Link>}>
          {runs.error && <div className="p-4"><ErrorBox error={runs.error} /></div>}
          {runs.data?.length === 0 && (
            <Empty icon={<FileSearch className="size-5" />} title="No plans yet">
              Go to <Link className="text-brand hover:underline" to="/fleet">Fleet</Link> and plan an upgrade, or run{' '}
              <code className="font-mono">jin plan</code> from your terminal.
            </Empty>
          )}
          <ul className="divide-y divide-line">
            {(runs.data ?? []).slice(0, 6).map((r) => (
              <li key={r.id}>
                <Link to={`/plans/${r.id}`} className="flex items-center gap-4 px-5 py-3.5 hover:bg-panel-2">
                  <div className="min-w-0 flex-1">
                    <div className="truncate font-medium">{shortContext(r.context)}</div>
                    <div className="mt-0.5 text-xs text-muted">{timeAgo(r.startedAt)}</div>
                  </div>
                  {r.status === 'failed' ? (
                    <StatusBadge status="failed" />
                  ) : (
                    <>
                      <Version from={r.current} to={r.target} />
                      <span className={`text-xs font-semibold ${r.blockers ? 'text-red-600 dark:text-red-400' : 'text-emerald-600 dark:text-emerald-400'}`}>
                        {r.blockers ? `${r.blockers} blocker${r.blockers > 1 ? 's' : ''}` : 'Ready'}
                      </span>
                    </>
                  )}
                </Link>
              </li>
            ))}
          </ul>
        </Card>
      </div>
    </>
  )
}
