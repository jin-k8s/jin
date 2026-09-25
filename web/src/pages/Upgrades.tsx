import { useNavigate } from 'react-router'
import { Rocket } from 'lucide-react'
import { api, useResource } from '../api'
import { Card, Empty, ErrorBox, PageHeader, Spinner, StatusBadge, Version } from '../components/ui'
import { shortContext, timeAgo } from '../format'

export function Upgrades() {
  const ups = useResource(api.upgrades, [], 5000)
  const navigate = useNavigate()
  return (
    <>
      <PageHeader title="Upgrades" subtitle="Automated upgrades with an approval gate before every hop." />
      <Card>
        {ups.loading && !ups.data && <Spinner />}
        {ups.error && <div className="p-4"><ErrorBox error={ups.error} /></div>}
        {ups.data?.length === 0 && (
          <Empty icon={<Rocket className="size-5" />} title="No upgrades yet">
            Open a plan for an EKS cluster and choose <span className="font-medium">Start upgrade</span>.
          </Empty>
        )}
        {!!ups.data?.length && (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-line text-left text-xs tracking-wide text-muted uppercase">
                  <th className="px-5 py-3 font-medium">Cluster</th>
                  <th className="px-5 py-3 font-medium">Upgrade</th>
                  <th className="px-5 py-3 font-medium">Progress</th>
                  <th className="px-5 py-3 font-medium">Status</th>
                  <th className="px-5 py-3 font-medium">Updated</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {ups.data.map((u) => {
                  const done = u.hops.filter((h) => h.status === 'succeeded').length
                  return (
                    <tr key={u.id} onClick={() => navigate(`/upgrades/${u.id}`)} className="cursor-pointer hover:bg-panel-2/60">
                      <td className="max-w-sm px-5 py-3.5">
                        <div className="truncate font-medium">{shortContext(u.cluster.context)}</div>
                        <div className="mt-0.5 text-xs text-muted">started by {u.createdBy}</div>
                      </td>
                      <td className="px-5 py-3.5">
                        <Version from={u.from} to={u.to} />
                      </td>
                      <td className="px-5 py-3.5">
                        <div className="flex items-center gap-2">
                          <div className="h-1.5 w-24 overflow-hidden rounded-full bg-panel-2">
                            <div className="h-full rounded-full bg-brand" style={{ width: `${(done / u.hops.length) * 100}%` }} />
                          </div>
                          <span className="text-xs text-muted tabular">
                            {done}/{u.hops.length} hops
                          </span>
                        </div>
                      </td>
                      <td className="px-5 py-3.5">
                        <StatusBadge status={u.status} />
                      </td>
                      <td className="px-5 py-3.5 whitespace-nowrap text-muted">{timeAgo(u.updatedAt)}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  )
}
