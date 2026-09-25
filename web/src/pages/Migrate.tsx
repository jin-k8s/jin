import { useState } from 'react'
import { Download, Plane } from 'lucide-react'
import { api, useResource } from '../api'
import { Button, Card, Empty, ErrorBox, Field, PageHeader, Pill, ProviderBadge, Select, Spinner, cx } from '../components/ui'
import { shortContext } from '../format'
import { useCan } from '../session'
import type { Assessment, MigrationItem } from '../types'

const effortCls: Record<MigrationItem['effort'], string> = {
  high: 'bg-red-500/15 text-red-700 dark:text-red-300',
  medium: 'bg-amber-500/15 text-amber-700 dark:text-amber-300',
  low: 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-300',
}

const clouds = [
  { id: 'eks', label: 'Amazon EKS' },
  { id: 'gke', label: 'Google GKE' },
  { id: 'aks', label: 'Azure AKS' },
]

function download(a: Assessment) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(a, null, 2)], { type: 'application/json' }))
  const link = document.createElement('a')
  link.href = url
  link.download = `jin-migration-${a.source}-to-${a.target}.json`
  link.click()
  URL.revokeObjectURL(url)
}

export function Migrate() {
  const can = useCan()
  const contexts = useResource(api.contexts, [])
  const [ctx, setCtx] = useState('')
  const [target, setTarget] = useState('gke')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const [a, setA] = useState<Assessment>()

  const run = async () => {
    setBusy(true)
    setError(undefined)
    setA(undefined)
    try {
      setA(await api.assess(ctx, target))
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(false)
    }
  }

  const byCat = new Map<string, MigrationItem[]>()
  for (const it of a?.items ?? []) byCat.set(it.category, [...(byCat.get(it.category) ?? []), it])

  return (
    <>
      <PageHeader title="Migrate" subtitle="What ties a cluster to its cloud, and what moving it to another provider involves. Read-only." />
      <Card>
        <div className="grid items-end gap-4 p-5 md:grid-cols-[1fr_16rem_auto]">
          <Field label="Cluster">
            <Select value={ctx} onChange={setCtx}>
              <option value="">Select…</option>
              {contexts.data?.map((c) => (
                <option key={c.name} value={c.name}>
                  {shortContext(c.name)}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Move to">
            <Select value={target} onChange={setTarget}>
              {clouds.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.label}
                </option>
              ))}
            </Select>
          </Field>
          <Button variant="primary" onClick={run} loading={busy} disabled={!ctx || !can('planner')} icon={<Plane className="size-4" />}>
            Assess
          </Button>
        </div>
      </Card>

      <div className="mt-6 space-y-6">
        {busy && <Spinner label="Inspecting the cluster…" />}
        {error && <ErrorBox error={error} title="Assessment failed" />}
        {!a && !busy && !error && (
          <Card>
            <Empty icon={<Plane className="size-5" />} title="Pick a cluster and a destination">
              Jin maps identity, storage and data, load balancing, ingress, registries, secrets, scheduling and cloud operators to their equivalents.
            </Empty>
          </Card>
        )}
        {a && (
          <>
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-5">
              <Card className="p-5 lg:col-span-1">
                <div className="text-xs text-muted">Estimated size</div>
                <div className="mt-2 text-4xl font-bold tracking-tight">{a.size}</div>
                <div className="mt-1 flex items-center gap-1.5 text-xs text-muted">
                  <ProviderBadge provider={a.source} /> → <ProviderBadge provider={a.target} />
                </div>
              </Card>
              {[
                ['Cloud bindings', a.summary.items],
                ['Volume data', `${a.summary.dataGiB.toFixed(1)} GiB`],
                ['External endpoints', a.summary.externalEndpoints],
                ['Workloads', a.summary.workloads],
              ].map(([k, v]) => (
                <Card key={k} className="p-5">
                  <div className="text-xs text-muted">{k}</div>
                  <div className="mt-2 text-2xl font-semibold tabular">{v}</div>
                </Card>
              ))}
            </div>

            <Card
              title="What changes"
              actions={
                <Button onClick={() => download(a)} icon={<Download className="size-4" />}>
                  Export JSON
                </Button>
              }
            >
              {(a.items ?? []).length === 0 ? (
                <Empty icon={<Plane className="size-5" />} title="No cloud-specific bindings found" />
              ) : (
                <div className="divide-y divide-line">
                  {[...byCat.entries()].map(([cat, items]) => (
                    <div key={cat} className="p-5">
                      <h3 className="mb-3 text-xs font-semibold tracking-wide text-muted uppercase">{cat}</h3>
                      <ul className="space-y-3">
                        {items.map((it, i) => (
                          <li key={i} className="rounded-lg border border-line bg-bg/40 p-3.5">
                            <div className="flex flex-wrap items-center gap-2">
                              <span className={cx('rounded-md px-2 py-0.5 text-xs font-semibold', effortCls[it.effort])}>{it.effort} effort</span>
                              <span className="font-medium">{it.binding}</span>
                              <Pill>{it.count}</Pill>
                            </div>
                            <p className="mt-2 text-sm">
                              <span className="text-muted">On the target: </span>
                              {it.mapping}
                            </p>
                            {!!it.examples?.length && <p className="mt-1.5 font-mono text-xs break-all text-muted">{it.examples.join(' · ')}</p>}
                          </li>
                        ))}
                      </ul>
                    </div>
                  ))}
                </div>
              )}
            </Card>

            <Card title="Keep in mind">
              <ul className="list-disc space-y-1.5 p-5 pl-9 text-sm text-muted">
                {a.notes.map((n, i) => (
                  <li key={i}>{n}</li>
                ))}
                {a.warnings?.map((w, i) => (
                  <li key={`w${i}`} className="text-amber-700 dark:text-amber-300">
                    {w}
                  </li>
                ))}
              </ul>
            </Card>
          </>
        )}
      </div>
    </>
  )
}
