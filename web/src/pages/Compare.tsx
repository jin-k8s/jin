import { useState } from 'react'
import { ArrowLeftRight, ArrowRight, CircleCheck, ListChecks } from 'lucide-react'
import { api, useResource } from '../api'
import { Button, Card, Empty, ErrorBox, Field, PageHeader, Pill, Select, Spinner, cx } from '../components/ui'
import { shortContext } from '../format'
import { useCan } from '../session'
import type { Parity, ParityItem } from '../types'

const statusCls: Record<ParityItem['status'], string> = {
  missing: 'bg-red-500/15 text-red-700 dark:text-red-300',
  'not-ready': 'bg-red-500/15 text-red-700 dark:text-red-300',
  mismatch: 'bg-amber-500/15 text-amber-700 dark:text-amber-300',
  action: 'bg-sky-500/15 text-sky-700 dark:text-sky-300',
  ok: 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-300',
}

export function Compare() {
  const can = useCan()
  const contexts = useResource(api.contexts, [])
  const [blue, setBlue] = useState('')
  const [green, setGreen] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const [result, setResult] = useState<Parity>()
  const [showOK, setShowOK] = useState(false)

  const run = async () => {
    setBusy(true)
    setError(undefined)
    setResult(undefined)
    try {
      setResult(await api.compare(blue, green))
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(false)
    }
  }
  const items = (result?.items ?? []).filter((i) => showOK || i.status !== 'ok')

  return (
    <>
      <PageHeader title="Blue/green" subtitle="Compare a replacement cluster with the live one before you move traffic. Read-only." />
      <Card>
        <div className="grid items-end gap-4 p-5 md:grid-cols-[1fr_auto_1fr_auto]">
          <Field label="Live cluster (blue)">
            <Select value={blue} onChange={setBlue}>
              <option value="">Select…</option>
              {contexts.data?.map((c) => (
                <option key={c.name} value={c.name}>
                  {shortContext(c.name)}
                </option>
              ))}
            </Select>
          </Field>
          <ArrowRight className="mb-2.5 hidden size-5 text-muted md:block" />
          <Field label="Replacement cluster (green)">
            <Select value={green} onChange={setGreen}>
              <option value="">Select…</option>
              {contexts.data?.filter((c) => c.name !== blue).map((c) => (
                <option key={c.name} value={c.name}>
                  {shortContext(c.name)}
                </option>
              ))}
            </Select>
          </Field>
          <Button variant="primary" onClick={run} loading={busy} disabled={!blue || !green || !can('planner')} icon={<ArrowLeftRight className="size-4" />}>
            Compare
          </Button>
        </div>
      </Card>

      <div className="mt-6 space-y-6">
        {busy && <Spinner label="Inspecting both clusters…" />}
        {error && <ErrorBox error={error} title="Comparison failed" />}
        {!result && !busy && !error && (
          <Card>
            <Empty icon={<ArrowLeftRight className="size-5" />} title="Pick two clusters">
              Jin checks namespaces, workloads and their readiness, Helm releases, CRDs, storage and ingress classes and secret backends, and plans the
              data and traffic cutover.
            </Empty>
          </Card>
        )}
        {result && (
          <>
            <div
              className={cx(
                'rounded-xl border px-5 py-4',
                result.ready ? 'border-emerald-500/30 bg-emerald-500/10' : 'border-red-500/30 bg-red-500/10',
              )}
            >
              <p className={cx('text-lg font-semibold', result.ready ? 'text-emerald-800 dark:text-emerald-200' : 'text-red-800 dark:text-red-200')}>
                {result.ready ? 'Green cluster matches blue' : 'Green cluster is not ready for cutover'}
              </p>
              <p className="mt-1 text-sm text-muted">
                {shortContext(result.source)} ({result.sourceVersion}) → {shortContext(result.target)} ({result.targetVersion}) · missing {result.summary.missing} ·
                not ready {result.summary.notReady} · mismatched {result.summary.mismatch} · actions {result.summary.actions} · ok {result.summary.ok}
              </p>
            </div>

            <Card
              title="Differences"
              actions={
                <label className="flex items-center gap-2 text-xs text-muted">
                  <input type="checkbox" checked={showOK} onChange={(e) => setShowOK(e.target.checked)} className="accent-brand" /> Show matching
                </label>
              }
            >
              {items.length === 0 ? (
                <Empty icon={<CircleCheck className="size-5" />} title="No differences" />
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b border-line text-left text-xs tracking-wide text-muted uppercase">
                        <th className="px-5 py-3 font-medium">Status</th>
                        <th className="px-5 py-3 font-medium">Category</th>
                        <th className="px-5 py-3 font-medium">Item</th>
                        <th className="px-5 py-3 font-medium">Blue</th>
                        <th className="px-5 py-3 font-medium">Green</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-line">
                      {items.map((it, i) => (
                        <tr key={i}>
                          <td className="px-5 py-3">
                            <span className={cx('rounded-md px-2 py-0.5 text-xs font-semibold', statusCls[it.status])}>{it.status}</span>
                          </td>
                          <td className="px-5 py-3 text-muted">{it.category}</td>
                          <td className="px-5 py-3">
                            <div className="font-mono text-xs break-all">{it.name}</div>
                            {it.detail && <div className="mt-1 text-xs text-muted">{it.detail}</div>}
                          </td>
                          <td className="px-5 py-3 text-xs text-muted">{it.source ?? '–'}</td>
                          <td className="px-5 py-3 text-xs text-muted">{it.target ?? '–'}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </Card>

            <Card title={<span className="flex items-center gap-2"><ListChecks className="size-4" /> Cutover checklist</span>}>
              <ol className="space-y-2 p-5 text-sm">
                {result.checklist.map((c, i) => (
                  <li key={i} className="flex gap-3">
                    <Pill>{i + 1}</Pill>
                    <span>{c}</span>
                  </li>
                ))}
              </ol>
            </Card>
          </>
        )}
      </div>
    </>
  )
}
