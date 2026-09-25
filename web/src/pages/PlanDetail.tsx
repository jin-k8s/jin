import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { ArrowLeft, ChevronDown, CircleCheck, GitPullRequest, Rocket, Zap } from 'lucide-react'
import { api, useResource } from '../api'
import { Button, Card, ErrorBox, PageHeader, Pill, SeverityBadge, Spinner, StatusBadge, Version, cx } from '../components/ui'
import { dateTime, plural, shortContext } from '../format'
import { Rich } from '../components/Rich'
import { useCan } from '../session'
import type { KubeContext, PlanHop, SupportAssessment } from '../types'

const phaseStyle: Record<string, string> = {
  prepare: 'bg-red-500/10 text-red-600 dark:text-red-300',
  'control-plane': 'bg-violet-500/10 text-violet-600 dark:text-violet-300',
  'add-ons': 'bg-sky-500/10 text-sky-600 dark:text-sky-300',
  'data-plane': 'bg-teal-500/10 text-teal-600 dark:text-teal-300',
  verify: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-300',
}

function HopCard({ hop, index, defaultOpen }: { hop: PlanHop; index: number; defaultOpen: boolean }) {
  const [open, setOpen] = useState(defaultOpen)
  const findings = hop.findings ?? []
  const blockers = findings.filter((f) => f.severity === 'blocker').length
  const warnings = findings.filter((f) => f.severity === 'warning').length

  return (
    <Card className="overflow-hidden">
      <button onClick={() => setOpen(!open)} className="flex w-full items-center gap-4 px-5 py-4 text-left hover:bg-panel-2/50" aria-expanded={open}>
        <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-brand/10 text-sm font-semibold text-brand">{index + 1}</span>
        <div className="flex flex-1 flex-wrap items-center gap-3">
          <Version from={hop.from} to={hop.to} />
          {blockers > 0 && <span className="text-xs font-semibold text-red-600 dark:text-red-400">{plural(blockers, 'blocker')}</span>}
          {warnings > 0 && <span className="text-xs font-semibold text-amber-600 dark:text-amber-400">{plural(warnings, 'warning')}</span>}
          {findings.length === 0 && (
            <span className="inline-flex items-center gap-1 text-xs font-semibold text-emerald-600 dark:text-emerald-400">
              <CircleCheck className="size-3.5" /> No findings
            </span>
          )}
          <Pill>data plane: {hop.dataPlane}</Pill>
        </div>
        <ChevronDown className={cx('size-4 text-muted transition', open && 'rotate-180')} />
      </button>

      {open && (
        <div className="grid gap-0 border-t border-line lg:grid-cols-5">
          <div className="border-line p-5 lg:col-span-3 lg:border-r">
            <h3 className="mb-3 text-xs font-semibold tracking-wide text-muted uppercase">Findings</h3>
            {findings.length === 0 && <p className="text-sm text-muted">Nothing blocks this hop.</p>}
            <ul className="space-y-3">
              {findings.map((f, i) => (
                <li key={i} className="rounded-lg border border-line bg-bg/50 p-3.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <SeverityBadge severity={f.severity} />
                    <span className="font-mono text-[11px] text-muted">{f.checkId}</span>
                  </div>
                  <p className="mt-2 text-sm font-medium"><Rich text={f.title} /></p>
                  {f.resource && <p className="mt-1 font-mono text-xs break-all text-muted">{f.resource}</p>}
                  {f.detail && <p className="mt-2 text-sm text-muted"><Rich text={f.detail} /></p>}
                  {f.remediation && (
                    <p className="mt-2 rounded-md bg-emerald-500/10 px-3 py-2 text-sm text-emerald-800 dark:text-emerald-200">
                      <span className="font-semibold">Fix: </span>
                      <Rich text={f.remediation} />
                    </p>
                  )}
                </li>
              ))}
            </ul>
          </div>
          <div className="p-5 lg:col-span-2">
            <h3 className="mb-3 text-xs font-semibold tracking-wide text-muted uppercase">Steps</h3>
            <ol className="space-y-3">
              {(hop.steps ?? []).map((s, i) => (
                <li key={i} className="flex gap-3">
                  <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-panel-2 text-[11px] font-semibold text-muted">{i + 1}</span>
                  <div className="min-w-0">
                    <span className={cx('rounded px-1.5 py-0.5 text-[11px] font-semibold', phaseStyle[s.phase] ?? 'bg-panel-2 text-muted')}>{s.phase}</span>
                    <p className="mt-1 text-sm font-medium"><Rich text={s.title} /></p>
                    {s.detail && <p className="mt-1 text-xs leading-relaxed text-muted"><Rich text={s.detail} /></p>}
                  </div>
                </li>
              ))}
            </ol>
          </div>
        </div>
      )}
    </Card>
  )
}

type Mode = 'gitops' | 'direct'

function modeAvailability(ctx?: KubeContext): Record<Mode, string | null> {
  return {
    gitops: ctx?.gitops ? null : 'Configure a repository and version targets in the cluster settings.',
    direct: ctx && (ctx.eks || ctx.gke || ctx.aks) ? null : 'Needs an EKS or GKE context, or AKS identity in the cluster settings.',
  }
}

function ModePicker({ mode, setMode, ctx }: { mode: Mode; setMode: (m: Mode) => void; ctx?: KubeContext }) {
  const avail = modeAvailability(ctx)
  const opts: { id: Mode; title: string; icon: React.ReactNode; body: string }[] = [
    {
      id: 'gitops',
      title: 'GitOps (pull requests)',
      icon: <GitPullRequest className="size-4" />,
      body: 'Jin opens a pull request per stage in your IaC repository and waits for your pipeline to apply it. No drift.',
    },
    {
      id: 'direct',
      title: 'Direct (cloud API)',
      icon: <Zap className="size-4" />,
      body: 'Jin calls the EKS, GKE or AKS API. Fast, but update the version in Terraform/GitOps afterwards to avoid drift.',
    },
  ]
  return (
    <div className="grid gap-3 md:grid-cols-2">
      {opts.map((o) => {
        const reason = avail[o.id]
        return (
          <button
            key={o.id}
            type="button"
            disabled={!!reason}
            onClick={() => setMode(o.id)}
            className={cx(
              'rounded-xl border p-4 text-left transition disabled:cursor-not-allowed disabled:opacity-60',
              mode === o.id && !reason ? 'border-brand bg-brand/5 ring-1 ring-brand/30' : 'border-line bg-panel hover:bg-panel-2/60',
            )}
          >
            <div className="flex items-center gap-2 font-semibold">
              {o.icon} {o.title}
            </div>
            <p className="mt-1.5 text-sm text-muted">{reason ?? o.body}</p>
          </button>
        )
      })}
    </div>
  )
}

function SupportCard({ s }: { s: SupportAssessment }) {
  const d = (x?: string) => (x ? new Date(x).toLocaleDateString([], { dateStyle: 'medium' }) : '–')
  const extended = s.currentStatus === 'extended-support'
  return (
    <Card className={cx('p-5', extended && 'border-red-500/40')}>
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <div className="text-xs text-muted">Support window ({s.provider.toUpperCase()})</div>
          <div className={cx('mt-1 text-lg font-semibold', extended ? 'text-red-600 dark:text-red-400' : '')}>
            {s.current} is in {s.currentStatus.replace('-', ' ')}
          </div>
          <div className="mt-0.5 text-sm text-muted">
            Standard support ends {d(s.endOfStandardSupport)} · extended support ends {d(s.endOfExtendedSupport)}
            {s.targetStatus === 'extended-support'
              ? ` · ${s.target} is also in extended support${s.firstStandardVersion ? `; standard pricing from ${s.firstStandardVersion}` : ''}`
              : s.targetEndOfStandardSupport && ` · ${s.target} is supported until ${d(s.targetEndOfStandardSupport)}`}
          </div>
        </div>
        {s.extendedSupportCostPerYearUsd > 0 && (
          <div className="text-right">
            <div className="text-xs text-muted">{extended ? 'Paying now' : 'Extended support would cost'}</div>
            <div className={cx('text-2xl font-semibold tabular', extended && 'text-red-600 dark:text-red-400')}>
              +${Math.round(extended ? s.currentSurchargePerYearUsd : s.extendedSupportCostPerYearUsd).toLocaleString()}
              <span className="text-sm font-normal text-muted">/year</span>
            </div>
            <div className="text-[11px] text-muted">list price, per cluster</div>
          </div>
        )}
      </div>
    </Card>
  )
}

export function PlanDetail() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const can = useCan()
  const run = useResource(() => api.run(id), [id])
  const contexts = useResource(api.contexts, [])
  const [starting, setStarting] = useState(false)
  const [startError, setStartError] = useState<Error>()
  const [chosen, setChosen] = useState<Mode>()

  if (run.loading && !run.data) return <Spinner />
  if (run.error) return <ErrorBox error={run.error} />
  const r = run.data!
  const p = r.plan
  const ctx = contexts.data?.find((c) => c.name === r.cluster.context)
  const avail = modeAvailability(ctx)
  const mode: Mode = chosen ?? (avail.gitops === null ? 'gitops' : 'direct')
  const canAutomate = r.status === 'succeeded' && !!p && avail[mode] === null && can('planner')

  const start = async () => {
    setStarting(true)
    setStartError(undefined)
    try {
      const u = await api.createUpgrade(r.id, mode)
      navigate(`/upgrades/${u.id}`)
    } catch (e) {
      setStartError(e as Error)
      setStarting(false)
    }
  }

  return (
    <>
      <Link to="/plans" className="mb-4 inline-flex items-center gap-1 text-sm text-muted hover:text-fg">
        <ArrowLeft className="size-4" /> Plans
      </Link>
      <PageHeader
        title={<span className="break-all">{shortContext(r.cluster.context)}</span>}
        subtitle={
          <>
            Planned {dateTime(r.startedAt)} · <span className="font-mono">{r.id}</span>
          </>
        }
        actions={
          p && (
            <Button variant="primary" icon={<Rocket className="size-4" />} onClick={start} loading={starting} disabled={!canAutomate}>
              Start upgrade
            </Button>
          )
        }
      />

      {r.status === 'failed' && <ErrorBox error={new Error(r.error ?? 'unknown error')} title="Inspection failed" />}

      {p && (
        <div className="space-y-6">
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <Card className="p-5">
              <div className="text-xs text-muted">Upgrade</div>
              <div className="mt-2 text-lg">
                <Version from={p.current} to={p.target} />
              </div>
              <div className="mt-1 text-xs text-muted">
                {plural(p.hops.length, 'hop')} · {p.provider}
              </div>
            </Card>
            <Card className="p-5">
              <div className="text-xs text-muted">Result</div>
              <div className="mt-2">
                {p.ready ? <StatusBadge status="succeeded" /> : <span className="text-lg font-semibold text-red-600 dark:text-red-400">Blocked</span>}
              </div>
              <div className="mt-1 text-xs text-muted">{p.ready ? 'No blockers found' : 'Resolve or override blockers'}</div>
            </Card>
            <Card className="p-5">
              <div className="text-xs text-muted">Findings</div>
              <div className="mt-2 flex gap-4 text-sm font-semibold tabular">
                <span className={p.summary.blockers ? 'text-red-600 dark:text-red-400' : 'text-muted'}>{plural(p.summary.blockers, 'blocker')}</span>
                <span className={p.summary.warnings ? 'text-amber-600 dark:text-amber-400' : 'text-muted'}>{plural(p.summary.warnings, 'warning')}</span>
              </div>
              <div className="mt-1 text-xs text-muted">{p.summary.info} informational</div>
            </Card>
            <Card className="p-5">
              <div className="text-xs text-muted">Cluster</div>
              <div className="mt-2 text-sm font-semibold">
                {r.cluster.nodes} nodes · {r.cluster.helmReleases} Helm releases
              </div>
              <div className="mt-1 font-mono text-xs text-muted">{r.cluster.gitVersion}</div>
            </Card>
          </div>

          {p.support && <SupportCard s={p.support} />}
          {contexts.data && can('planner') && (
            <div>
              <h2 className="mb-2 text-sm font-semibold">How should Jin run this upgrade?</h2>
              <ModePicker mode={mode} setMode={setChosen} ctx={ctx} />
              <p className="mt-2 text-xs text-muted">
                Either way, every hop waits for approval under the cluster's policy.
                {avail.gitops !== null && can('admin') && (
                  <>
                    {' '}
                    <Link to={`/clusters/settings?context=${encodeURIComponent(r.cluster.context)}`} className="font-medium text-brand hover:underline">
                      Set up GitOps for this cluster →
                    </Link>
                  </>
                )}
              </p>
            </div>
          )}
          {startError && <ErrorBox error={startError} title="Could not start the upgrade" />}

          {!!p.collectionWarnings?.length && (
            <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm text-amber-800 dark:text-amber-200">
              <p className="font-semibold">Some data could not be collected; findings may be missing.</p>
              <ul className="mt-1 list-disc pl-5">
                {p.collectionWarnings.map((w, i) => (
                  <li key={i} className="break-words">
                    {w}
                  </li>
                ))}
              </ul>
            </div>
          )}

          <div className="space-y-4">
            {p.hops.map((h, i) => (
              <HopCard key={h.to} hop={h} index={i} defaultOpen={i === 0 || (h.findings ?? []).some((f) => f.severity === 'blocker')} />
            ))}
          </div>
        </div>
      )}
    </>
  )
}
