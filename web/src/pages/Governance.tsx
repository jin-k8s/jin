import { Download, ShieldAlert, ShieldCheck, Users } from 'lucide-react'
import { api, useResource } from '../api'
import { Card, Empty, ErrorBox, PageHeader, Pill, Spinner } from '../components/ui'
import { dateTime } from '../format'
import { useCan, useSession } from '../session'
import type { PolicyView } from '../types'

function PolicyCard({ p, fallback }: { p: PolicyView; fallback?: boolean }) {
  const match = [...(p.match?.environments ?? []).map((e) => `environment ${e}`), ...(p.match?.contexts ?? []).map((c) => `context ${c}`)]
  return (
    <Card className="p-5">
      <div className="flex items-center justify-between">
        <h3 className="font-semibold">{p.name}</h3>
        {fallback ? <Pill>fallback</Pill> : <Pill>{match.length ? match.join(', ') : 'all clusters'}</Pill>}
      </div>
      <dl className="mt-4 grid grid-cols-2 gap-3 text-sm">
        <div>
          <dt className="text-xs text-muted">Approvals per hop</dt>
          <dd className="font-semibold">{p.requiredApprovals}</dd>
        </div>
        <div>
          <dt className="text-xs text-muted">Self-approval</dt>
          <dd className="font-semibold">{p.forbidSelfApproval ? 'not allowed' : 'allowed'}</dd>
        </div>
        <div>
          <dt className="text-xs text-muted">Comment</dt>
          <dd className="font-semibold">{p.requireComment ? 'required' : 'optional'}</dd>
        </div>
        <div>
          <dt className="text-xs text-muted">Approver groups</dt>
          <dd className="font-semibold">{p.requiredGroups?.length ? p.requiredGroups.join(', ') : 'any approver'}</dd>
        </div>
        <div className="col-span-2">
          <dt className="text-xs text-muted">Change window</dt>
          <dd className="font-semibold">
            {p.windows}{' '}
            {p.windows !== 'any time' && (
              <span className={p.inWindow ? 'text-emerald-600' : 'text-amber-600'}>({p.inWindow ? 'open now' : 'closed now'})</span>
            )}
          </dd>
        </div>
      </dl>
    </Card>
  )
}

function AuditLog() {
  const entries = useResource(api.audit, [], 30000)
  const verify = useResource(api.auditVerify, [], 30000)
  const rows = [...(entries.data ?? [])].reverse().slice(0, 200)
  return (
    <Card
      title={
        <span className="flex items-center gap-2">
          Audit log
          {verify.data &&
            (verify.data.intact ? (
              <span className="flex items-center gap-1 text-xs font-medium text-emerald-600">
                <ShieldCheck className="size-3.5" /> hash chain intact
              </span>
            ) : (
              <span className="flex items-center gap-1 text-xs font-medium text-red-600">
                <ShieldAlert className="size-3.5" /> chain broken at entry {verify.data.brokenAt}
              </span>
            ))}
        </span>
      }
      actions={
        <div className="flex gap-2 text-xs">
          <a className="inline-flex items-center gap-1 rounded-md border border-line px-2.5 py-1.5 font-medium hover:bg-panel-2" href="/api/v1/audit?format=csv">
            <Download className="size-3.5" /> CSV
          </a>
          <a className="inline-flex items-center gap-1 rounded-md border border-line px-2.5 py-1.5 font-medium hover:bg-panel-2" href="/api/v1/audit?format=jsonl">
            <Download className="size-3.5" /> JSONL
          </a>
        </div>
      }
    >
      {entries.loading && !entries.data && <Spinner />}
      {entries.error && (
        <div className="p-4">
          <ErrorBox error={entries.error} />
        </div>
      )}
      {entries.data?.length === 0 && <Empty icon={<ShieldCheck className="size-5" />} title="No audit entries yet" />}
      {rows.length > 0 && (
        <div className="max-h-[32rem] overflow-auto">
          <table className="w-full text-sm">
            <thead className="sticky top-0 bg-panel">
              <tr className="border-b border-line text-left text-xs tracking-wide text-muted uppercase">
                <th className="px-5 py-3 font-medium">Time</th>
                <th className="px-5 py-3 font-medium">Actor</th>
                <th className="px-5 py-3 font-medium">Action</th>
                <th className="px-5 py-3 font-medium">Target</th>
                <th className="px-5 py-3 font-medium">Detail</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {rows.map((e) => (
                <tr key={e.seq}>
                  <td className="px-5 py-2.5 text-xs whitespace-nowrap text-muted">{dateTime(e.time)}</td>
                  <td className="px-5 py-2.5">
                    <div className="text-xs font-medium">{e.actor}</div>
                    {e.role && <div className="text-[11px] text-muted">{e.role}</div>}
                  </td>
                  <td className="px-5 py-2.5 font-mono text-xs">{e.action}</td>
                  <td className="max-w-[14rem] truncate px-5 py-2.5 font-mono text-xs" title={e.target}>
                    {e.target}
                  </td>
                  <td className="max-w-[20rem] truncate px-5 py-2.5 text-xs text-muted" title={e.detail}>
                    {e.detail}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}

export function Governance() {
  const can = useCan()
  const s = useSession()
  const pol = useResource(api.policies, [])
  return (
    <>
      <PageHeader title="Governance" subtitle="Who can do what, what every upgrade hop needs before it runs, and what happened." />
      <div className="space-y-6">
        <Card title={<span className="flex items-center gap-2"><Users className="size-4" /> You</span>}>
          <div className="flex flex-wrap items-center gap-x-8 gap-y-2 p-5 text-sm">
            <div>
              <span className="text-muted">Signed in as </span>
              <span className="font-medium">{s.identity?.email || s.identity?.name}</span>
            </div>
            <div>
              <span className="text-muted">Role </span>
              <span className="font-medium">{s.role}</span>
            </div>
            {!!s.identity?.groups?.length && (
              <div>
                <span className="text-muted">Groups </span>
                <span className="font-medium">{s.identity.groups.join(', ')}</span>
              </div>
            )}
            <p className="w-full text-xs text-muted">
              Roles: viewer (read), planner (plan, start upgrades, compare, assess), approver (approve, retry, cancel), admin (settings, audit). Bindings live
              in <code className="font-mono">$JIN_HOME/config.yaml</code>.
            </p>
          </div>
        </Card>

        <div>
          <h2 className="mb-3 text-sm font-semibold">Approval policies</h2>
          {pol.loading && !pol.data && <Spinner />}
          {pol.error && <ErrorBox error={pol.error} />}
          {pol.data && (
            <div className="grid gap-4 md:grid-cols-2">
              {pol.data.policies.map((p) => (
                <PolicyCard key={p.name} p={p} />
              ))}
              <PolicyCard p={pol.data.default} fallback />
            </div>
          )}
        </div>

        {can('admin') && <AuditLog />}
      </div>
    </>
  )
}
