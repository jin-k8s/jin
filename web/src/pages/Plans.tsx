import { useNavigate } from 'react-router'
import { FileSearch } from 'lucide-react'
import { api, useResource } from '../api'
import { Card, Empty, ErrorBox, PageHeader, Pill, Spinner, StatusBadge, Version } from '../components/ui'
import { dateTime, shortContext } from '../format'

export function Plans() {
  const runs = useResource(api.runs, [], 15000)
  const navigate = useNavigate()

  return (
    <>
      <PageHeader title="Plans" subtitle="Every plan run, from the UI or the CLI, is recorded here." />
      <Card>
        {runs.loading && !runs.data && <Spinner />}
        {runs.error && <div className="p-4"><ErrorBox error={runs.error} /></div>}
        {runs.data?.length === 0 && (
          <Empty icon={<FileSearch className="size-5" />} title="No plans yet">
            Plan an upgrade from the Clusters page or with <code className="font-mono">jin plan</code>.
          </Empty>
        )}
        {!!runs.data?.length && (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-line text-left text-xs tracking-wide text-muted uppercase">
                  <th className="px-5 py-3 font-medium">Cluster</th>
                  <th className="px-5 py-3 font-medium">Upgrade</th>
                  <th className="px-5 py-3 font-medium">Result</th>
                  <th className="px-5 py-3 font-medium">Planned</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {runs.data.map((r) => (
                  <tr key={r.id} onClick={() => navigate(`/plans/${r.id}`)} className="cursor-pointer hover:bg-panel-2/60">
                    <td className="max-w-sm px-5 py-3.5">
                      <div className="truncate font-medium">{shortContext(r.context)}</div>
                      <div className="mt-0.5 font-mono text-xs text-muted">{r.id}</div>
                    </td>
                    <td className="px-5 py-3.5">
                      {r.status === 'succeeded' ? (
                        <div className="flex items-center gap-2">
                          <Version from={r.current} to={r.target} />
                          <Pill>
                            {r.hops} hop{r.hops === 1 ? '' : 's'}
                          </Pill>
                        </div>
                      ) : (
                        <span className="text-muted">–</span>
                      )}
                    </td>
                    <td className="px-5 py-3.5">
                      {r.status === 'failed' ? (
                        <StatusBadge status="failed" />
                      ) : (
                        <div className="flex items-center gap-2 text-xs font-semibold">
                          <span className={r.blockers ? 'text-red-600 dark:text-red-400' : 'text-emerald-600 dark:text-emerald-400'}>
                            {r.blockers ? `${r.blockers} blocker${r.blockers > 1 ? 's' : ''}` : 'Ready'}
                          </span>
                          {r.warnings > 0 && <span className="text-amber-600 dark:text-amber-400">{r.warnings} warning{r.warnings > 1 ? 's' : ''}</span>}
                          {r.incomplete && <Pill>incomplete data</Pill>}
                        </div>
                      )}
                    </td>
                    <td className="px-5 py-3.5 whitespace-nowrap text-muted">{dateTime(r.startedAt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  )
}
