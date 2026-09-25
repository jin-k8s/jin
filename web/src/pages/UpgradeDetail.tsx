import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router'
import { ArrowLeft, Ban, CircleCheck, GitPullRequest, RotateCcw, ShieldCheck, Terminal, TriangleAlert, Users } from 'lucide-react'
import { api, useResource } from '../api'
import { Button, Card, ErrorBox, PageHeader, Pill, Spinner, StatusBadge, StatusIcon, Version, cx } from '../components/ui'
import { clock, dateTime, duration, shortContext } from '../format'
import { useCan, useSession } from '../session'
import type { Upgrade, UpgradeEvent, UpgradeHop } from '../types'

/** Renders http(s) URLs in log lines as links; everything else stays text. */
function Linkify({ text }: { text: string }) {
  const parts = text.split(/(https?:\/\/[^\s)]+)/g)
  return (
    <>
      {parts.map((p, i) =>
        /^https?:\/\//.test(p) ? (
          <a key={i} href={p} target="_blank" rel="noopener noreferrer" className="underline decoration-dotted hover:text-white">
            {p}
          </a>
        ) : (
          p
        ),
      )}
    </>
  )
}

function approverSubjects(hop: UpgradeHop) {
  return [...new Set((hop.approvals ?? []).filter((a) => a.action === 'approve').map((a) => a.subject || a.by))]
}

const stageLabel: Record<string, string> = {
  preflight: 'Preflight',
  'control-plane': 'Control plane',
  'add-ons': 'Add-ons',
  'data-plane': 'Data plane',
  verify: 'Verify',
}

function useNow(active: boolean) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    if (!active) return
    const t = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [active])
  return now
}

/** Streams upgrade events over SSE; the browser reconnects with Last-Event-ID automatically. */
function useEventStream(id: string, onState: () => void) {
  const [events, setEvents] = useState<UpgradeEvent[]>([])
  const [connected, setConnected] = useState(false)
  const onStateRef = useRef(onState)
  onStateRef.current = onState

  useEffect(() => {
    setEvents([])
    const es = new EventSource(api.streamURL(id))
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)
    es.addEventListener('upgrade', (e) => {
      const ev = JSON.parse((e as MessageEvent<string>).data) as UpgradeEvent
      setEvents((prev) => (prev.length && prev[prev.length - 1].seq >= ev.seq ? prev : [...prev, ev]))
      if (ev.level === 'state') onStateRef.current()
    })
    return () => es.close()
  }, [id])

  return { events, connected }
}

function HopTimeline({ u, selected, onSelect }: { u: Upgrade; selected: number; onSelect: (n: number) => void }) {
  return (
    <ol className="flex flex-col gap-1">
      {u.hops.map((h) => (
        <li key={h.index}>
          <button
            onClick={() => onSelect(h.index)}
            className={cx(
              'flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition',
              selected === h.index ? 'bg-brand/10 ring-1 ring-brand/30' : 'hover:bg-panel-2',
            )}
          >
            <StatusIcon status={h.status} />
            <div className="min-w-0 flex-1">
              <div className="text-xs text-muted">Hop {h.index}</div>
              <Version from={h.from} to={h.to} />
            </div>
            {h.blockers > 0 && h.status !== 'succeeded' && <Pill className="bg-red-500/10 text-red-600 dark:text-red-300">{h.blockers}</Pill>}
          </button>
        </li>
      ))}
    </ol>
  )
}

function StagePipeline({ hop, now, progress }: { hop: UpgradeHop; now: number; progress?: UpgradeEvent['progress'] & { stage?: string } }) {
  return (
    <div className="grid gap-3 sm:grid-cols-5">
      {hop.stages.map((s, i) => (
        <div
          key={s.name}
          className={cx(
            'relative rounded-lg border p-3.5',
            s.status === 'running' ? 'border-sky-500/50 bg-sky-500/5' : s.status === 'failed' ? 'border-red-500/50 bg-red-500/5' : 'border-line bg-bg/40',
          )}
        >
          <div className="flex items-center gap-2">
            <StatusIcon status={s.status} className="size-4" />
            <span className="text-xs text-muted">{i + 1}</span>
          </div>
          <div className="mt-2 text-sm font-semibold">{stageLabel[s.name] ?? s.name}</div>
          <div className="mt-0.5 text-xs text-muted tabular">
            {s.startedAt ? duration(s.startedAt, s.finishedAt, now) : s.status === 'skipped' ? 'skipped' : 'waiting'}
          </div>
          {s.status === 'running' && progress && progress.stage === s.name && progress.total > 0 && (
            <div className="mt-2">
              <div className="h-1.5 overflow-hidden rounded-full bg-panel-2">
                <div className="h-full rounded-full bg-sky-500 transition-all" style={{ width: `${(progress.done / progress.total) * 100}%` }} />
              </div>
              <div className="mt-1 text-[11px] text-muted tabular">
                {progress.done}/{progress.total} {progress.unit}
              </div>
            </div>
          )}
          {s.message && <p className={cx('mt-2 line-clamp-3 text-xs', s.status === 'failed' ? 'text-red-600 dark:text-red-300' : 'text-muted')}>{s.message}</p>}
        </div>
      ))}
    </div>
  )
}

function ApprovalPanel({ u, hop, onDone }: { u: Upgrade; hop: UpgradeHop; onDone: () => void }) {
  const [confirm, setConfirm] = useState('')
  const [comment, setComment] = useState('')
  const [override, setOverride] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const can = useCan()
  const session = useSession()
  const pol = u.policy
  const done = approverSubjects(hop)
  const me = session.identity?.subject ?? ''
  const blockedReason = !can('approver')
    ? 'You need the approver role to approve.'
    : pol?.forbidSelfApproval && u.createdBySubject && u.createdBySubject === me
      ? `Policy "${pol.name}" does not allow approving an upgrade you started.`
      : done.includes(me)
        ? 'You already approved this hop; another approver is needed.'
        : pol && !pol.inWindow
          ? `Outside the change window (${pol.windows}).`
          : null
  const commentRequired = override || !!pol?.requireComment
  const valid = !blockedReason && confirm.trim() === hop.to && (!commentRequired || comment.trim() !== '')

  const approve = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.approve(u.id, { hop: hop.index, confirm: confirm.trim(), comment: comment.trim(), overrideBlockers: override })
      onDone()
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="border-amber-500/40">
      <div className="border-b border-line px-5 py-4">
        <div className="flex items-center gap-2 text-amber-700 dark:text-amber-300">
          <ShieldCheck className="size-5" />
          <h2 className="font-semibold">Approval required: hop {hop.index}</h2>
        </div>
        <p className="mt-1 text-sm text-muted">
          Upgrade <span className="font-medium text-fg">{shortContext(u.cluster.context)}</span> from{' '}
          <span className="font-mono">{hop.from}</span> to <span className="font-mono font-semibold text-fg">{hop.to}</span>.
        </p>
        {pol && (
          <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
            <span className="flex items-center gap-1 font-medium text-fg">
              <Users className="size-3.5" /> Policy {pol.name}: {done.length} of {pol.requiredApprovals} approval{pol.requiredApprovals > 1 ? 's' : ''}
            </span>
            {pol.forbidSelfApproval && <span>no self-approval</span>}
            {pol.requireComment && <span>comment required</span>}
            {!!pol.requiredGroups?.length && <span>approvers from {pol.requiredGroups.join(', ')}</span>}
            <span className={pol.inWindow ? '' : 'text-amber-600'}>window: {pol.windows}</span>
          </div>
        )}
      </div>
      <div className="space-y-4 px-5 py-4">
        <ul className="space-y-2 text-sm">
          <li className="flex gap-2">
            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-amber-500" />
            <span>
              <span className="font-medium">Control-plane upgrades cannot be rolled back.</span> Preflight re-inspects the cluster before anything changes.
            </span>
          </li>
          {u.mode === 'direct' ? (
            <li className="flex gap-2">
              <TriangleAlert className="mt-0.5 size-4 shrink-0 text-amber-500" />
              <span>Direct mode changes the cluster outside IaC. Set the version to {hop.to} in Terraform/GitOps afterwards.</span>
            </li>
          ) : (
            <li className="flex gap-2">
              <GitPullRequest className="mt-0.5 size-4 shrink-0 text-emerald-500" />
              <span>GitOps mode: Jin opens a pull request per stage; the hop proceeds as each one is merged and applied by your pipeline.</span>
            </li>
          )}
          <li className="flex gap-2">
            <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-500" />
            <span>
              Data plane for this hop: <span className="font-medium">{hop.dataPlane}</span>
              {hop.dataPlane === 'optional' && ' (nodes stay on their version, within the skew policy)'}.
            </span>
          </li>
        </ul>

        {hop.blockers > 0 && (
          <div className="rounded-lg border border-red-500/30 bg-red-500/10 p-3 text-sm">
            <p className="font-semibold text-red-700 dark:text-red-300">
              The plan reported {hop.blockers} blocker{hop.blockers > 1 ? 's' : ''} for this hop.
            </p>
            <p className="mt-1 text-red-700/80 dark:text-red-300/80">Preflight stops the hop if they still exist, unless you override them with a justification.</p>
            <label className="mt-2 flex items-center gap-2 font-medium">
              <input type="checkbox" checked={override} onChange={(e) => setOverride(e.target.checked)} className="size-4 accent-red-600" />
              Override blockers for this hop
            </label>
          </div>
        )}

        <label className="block">
          <span className="text-sm font-medium">Comment {commentRequired ? <span className="text-red-500">(required)</span> : <span className="text-muted">(optional)</span>}</span>
          <textarea
            value={comment}
            onChange={(e) => setComment(e.target.value)}
            rows={2}
            placeholder="Change ticket, reason, maintenance window…"
            className="mt-1.5 w-full rounded-lg border border-line bg-bg px-3 py-2 text-sm outline-none focus:border-brand"
          />
        </label>
        <label className="block">
          <span className="block text-sm font-medium">
            Type <span className="rounded bg-panel-2 px-1.5 py-0.5 font-mono">{hop.to}</span> to confirm
          </span>
          <input
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            className="mt-1.5 w-40 rounded-lg border border-line bg-bg px-3 py-2 font-mono text-sm outline-none focus:border-brand"
            aria-label="Confirm target version"
          />
        </label>
        {blockedReason && <p className="rounded-lg bg-panel-2 px-3 py-2 text-sm text-muted">{blockedReason}</p>}
        {error && <ErrorBox error={error} title="Approval failed" />}
      </div>
      <div className="flex flex-wrap justify-end gap-2 border-t border-line px-5 py-4">
        {can('approver') && <CancelButton id={u.id} onDone={onDone} />}
        <Button variant="primary" disabled={!valid} loading={busy} onClick={approve} icon={<ShieldCheck className="size-4" />}>
          Approve and run hop {hop.index}
        </Button>
      </div>
    </Card>
  )
}

function CancelButton({ id, onDone, label = 'Cancel upgrade' }: { id: string; onDone: () => void; label?: string }) {
  const [busy, setBusy] = useState(false)
  const cancel = async () => {
    if (!window.confirm('Cancel this upgrade? Cloud operations already submitted continue on the provider side.')) return
    setBusy(true)
    try {
      await api.cancel(id)
      onDone()
    } finally {
      setBusy(false)
    }
  }
  return (
    <Button variant="ghost" onClick={cancel} loading={busy} icon={<Ban className="size-4" />}>
      {label}
    </Button>
  )
}

function FailedPanel({ u, onDone }: { u: Upgrade; onDone: () => void }) {
  const can = useCan()
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const retry = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.retry(u.id, comment.trim())
      onDone()
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Card className="border-red-500/40">
      <div className="px-5 py-4">
        <h2 className="font-semibold text-red-700 dark:text-red-300">Hop {u.currentHop} stopped</h2>
        <p className="mt-1 text-sm break-words">{u.error}</p>
        <p className="mt-2 text-sm text-muted">
          Fix the cause, then retry. Completed stages are not repeated; every stage is safe to re-run.
        </p>
        <input
          value={comment}
          onChange={(e) => setComment(e.target.value)}
          placeholder="What did you change? (optional)"
          className="mt-3 w-full rounded-lg border border-line bg-bg px-3 py-2 text-sm outline-none focus:border-brand"
        />
        {error && (
          <div className="mt-3">
            <ErrorBox error={error} />
          </div>
        )}
      </div>
      {can('approver') && (
        <div className="flex justify-end gap-2 border-t border-line px-5 py-4">
          <CancelButton id={u.id} onDone={onDone} />
          <Button variant="primary" onClick={retry} loading={busy} icon={<RotateCcw className="size-4" />}>
            Retry hop {u.currentHop}
          </Button>
        </div>
      )}
    </Card>
  )
}

function ActivityLog({ events, hop, connected }: { events: UpgradeEvent[]; hop: number | null; connected: boolean }) {
  const ref = useRef<HTMLDivElement>(null)
  const stick = useRef(true)
  const shown = hop === null ? events : events.filter((e) => !e.hop || e.hop === hop)

  useEffect(() => {
    const el = ref.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [shown.length])

  const levelCls: Record<UpgradeEvent['level'], string> = {
    info: 'text-console-fg',
    warn: 'text-amber-300',
    error: 'text-red-400',
    state: 'text-cyan-300 font-semibold',
  }

  return (
    <div className="overflow-hidden rounded-xl border border-line bg-console">
      <div className="flex items-center justify-between border-b border-white/10 px-4 py-2.5">
        <span className="flex items-center gap-2 text-xs font-medium text-slate-300">
          <Terminal className="size-3.5" /> Activity
        </span>
        <span className="flex items-center gap-1.5 text-[11px] text-slate-400">
          <span className={cx('size-1.5 rounded-full', connected ? 'bg-emerald-400' : 'bg-slate-500')} />
          {connected ? 'live' : 'reconnecting'}
        </span>
      </div>
      <div
        ref={ref}
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
        }}
        className="h-96 overflow-y-auto px-4 py-3 font-mono text-xs leading-relaxed"
      >
        {shown.length === 0 && <p className="text-slate-500">Waiting for events…</p>}
        {shown.map((e) => (
          <div key={e.seq} className="flex gap-3">
            <span className="shrink-0 text-slate-500 tabular">{clock(e.time)}</span>
            {e.stage && <span className="w-24 shrink-0 truncate text-slate-400">{e.stage}</span>}
            <span className={cx('break-words', levelCls[e.level])}>
              <Linkify text={e.message} />
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

export function UpgradeDetail() {
  const can = useCan()
  const { id = '' } = useParams()
  const up = useResource(() => api.upgrade(id), [id])
  const { events, connected } = useEventStream(id, up.reload)
  const [selected, setSelected] = useState<number | null>(null)
  const [allHops, setAllHops] = useState(false)
  const u = up.data
  const now = useNow(u?.status === 'running')

  const hopIdx = selected ?? u?.currentHop ?? 1
  const hop = u?.hops[hopIdx - 1]
  const progress = useMemo(() => {
    for (let i = events.length - 1; i >= 0; i--) {
      const e = events[i]
      if (e.hop === hopIdx && e.progress) return { ...e.progress, stage: e.stage }
    }
    return undefined
  }, [events, hopIdx])

  if (up.loading && !u) return <Spinner />
  if (up.error) return <ErrorBox error={up.error} />
  if (!u || !hop) return null

  const isCurrent = hopIdx === u.currentHop

  return (
    <>
      <Link to="/upgrades" className="mb-4 inline-flex items-center gap-1 text-sm text-muted hover:text-fg">
        <ArrowLeft className="size-4" /> Upgrades
      </Link>
      <PageHeader
        title={
          <span className="flex flex-wrap items-center gap-3">
            <span className="break-all">{shortContext(u.cluster.context)}</span>
            <StatusBadge status={u.status} />
          </span>
        }
        subtitle={
          <>
            <Version from={u.from} to={u.to} /> · {u.mode === 'gitops' ? 'GitOps' : 'direct'} mode{u.policy ? ` · policy ${u.policy.name}` : ''} · started by {u.createdBy} {dateTime(u.createdAt)} ·{' '}
            <Link className="text-brand hover:underline" to={`/plans/${u.planRunId}`}>
              plan
            </Link>
          </>
        }
        actions={u.status === 'running' && can('approver') && <CancelButton id={u.id} onDone={up.reload} />}
      />

      <div className="grid gap-6 lg:grid-cols-[16rem_1fr]">
        <Card className="h-fit p-2">
          <HopTimeline u={u} selected={hopIdx} onSelect={setSelected} />
        </Card>

        <div className="min-w-0 space-y-6">
          <Card
            title={
              <span className="flex items-center gap-2">
                Hop {hop.index} <Version from={hop.from} to={hop.to} />
              </span>
            }
            actions={<StatusBadge status={hop.status} />}
          >
            <div className="p-5">
              <StagePipeline hop={hop} now={now} progress={progress} />
              {!!hop.approvals?.length && (
                <ul className="mt-4 space-y-1 text-xs text-muted">
                  {hop.approvals.map((a, i) => (
                    <li key={i}>
                      <span className="font-medium text-fg">{a.action === 'retry' ? 'Retried' : 'Approved'}</span> by {a.by} · {dateTime(a.at)}
                      {a.overrideBlockers && <span className="font-medium text-red-600 dark:text-red-400"> · blockers overridden</span>}
                      {a.comment && <span> · “{a.comment}”</span>}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </Card>

          {u.mode === 'gitops' && u.status === 'running' && (
            <div className="flex items-start gap-2 rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-800 dark:text-emerald-200">
              <GitPullRequest className="mt-0.5 size-4 shrink-0" />
              <span>Jin is working through pull requests. Review and merge them in your repository; links are in the activity log.</span>
            </div>
          )}
          {isCurrent && u.status === 'awaiting-approval' && <ApprovalPanel key={hop.index} u={u} hop={hop} onDone={up.reload} />}
          {isCurrent && u.status === 'failed' && <FailedPanel u={u} onDone={up.reload} />}
          {u.status === 'succeeded' && (
            <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-800 dark:text-emerald-200">
              <p className="font-semibold">Upgrade complete: the cluster is on {u.to}.</p>
              {u.mode === 'direct' && <p className="mt-1">Update the cluster version in your Terraform/GitOps sources to {u.to} so the next apply does not drift.</p>}
            </div>
          )}

          <div>
            <div className="mb-2 flex items-center justify-end">
              <label className="flex items-center gap-2 text-xs text-muted">
                <input type="checkbox" checked={allHops} onChange={(e) => setAllHops(e.target.checked)} className="accent-brand" />
                Show all hops
              </label>
            </div>
            <ActivityLog events={events} hop={allHops ? null : hopIdx} connected={connected} />
          </div>
        </div>
      </div>
    </>
  )
}
