import type { ButtonHTMLAttributes, ReactNode } from 'react'
import {
  Ban,
  CircleCheck,
  CircleDashed,
  CirclePause,
  CircleX,
  Info,
  LoaderCircle,
  SkipForward,
  TriangleAlert,
} from 'lucide-react'
import type { Severity, Status } from '../types'

export function cx(...c: (string | false | null | undefined)[]) {
  return c.filter(Boolean).join(' ')
}

export function Logo({ size = 28 }: { size?: number }) {
  // Same file as docs/assets/jin-icon.svg (copied to web/public) so the app and README never drift.
  return <img src="/jin-icon.svg" width={size} height={size} alt="" aria-hidden="true" className="shrink-0 rounded-[22%] ring-1 ring-line" />
}

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost'

export function Button({
  variant = 'secondary',
  loading,
  icon,
  children,
  className,
  disabled,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; loading?: boolean; icon?: ReactNode }) {
  const styles: Record<Variant, string> = {
    primary: 'bg-brand text-brand-fg hover:opacity-90 shadow-sm',
    secondary: 'bg-panel border border-line text-fg hover:bg-panel-2',
    danger: 'bg-red-600 text-white hover:bg-red-700 shadow-sm',
    ghost: 'text-muted hover:text-fg hover:bg-panel-2',
  }
  return (
    <button
      {...rest}
      disabled={disabled || loading}
      className={cx(
        'inline-flex items-center justify-center gap-2 rounded-lg px-3.5 py-2 text-sm font-medium transition',
        'disabled:cursor-not-allowed disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand',
        styles[variant],
        className,
      )}
    >
      {loading ? <LoaderCircle className="size-4 animate-spin" /> : icon}
      {children}
    </button>
  )
}

export function Card({ children, className, title, actions }: { children: ReactNode; className?: string; title?: ReactNode; actions?: ReactNode }) {
  return (
    <section className={cx('rounded-xl border border-line bg-panel shadow-sm', className)}>
      {(title || actions) && (
        <header className="flex items-center justify-between gap-4 border-b border-line px-5 py-3.5">
          <h2 className="text-sm font-semibold">{title}</h2>
          {actions}
        </header>
      )}
      {children}
    </section>
  )
}

export function PageHeader({ title, subtitle, actions }: { title: ReactNode; subtitle?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        {subtitle && <p className="mt-1 text-sm text-muted">{subtitle}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  )
}

const statusStyle: Record<Status, { label: string; cls: string; icon: ReactNode }> = {
  pending: { label: 'Pending', cls: 'text-muted bg-panel-2', icon: <CircleDashed className="size-3.5" /> },
  'awaiting-approval': {
    label: 'Awaiting approval',
    cls: 'text-amber-700 bg-amber-500/15 dark:text-amber-300',
    icon: <CirclePause className="size-3.5" />,
  },
  running: { label: 'Running', cls: 'text-sky-700 bg-sky-500/15 dark:text-sky-300', icon: <LoaderCircle className="size-3.5 animate-spin" /> },
  succeeded: {
    label: 'Succeeded',
    cls: 'text-emerald-700 bg-emerald-500/15 dark:text-emerald-300',
    icon: <CircleCheck className="size-3.5" />,
  },
  failed: { label: 'Failed', cls: 'text-red-700 bg-red-500/15 dark:text-red-300', icon: <CircleX className="size-3.5" /> },
  skipped: { label: 'Skipped', cls: 'text-muted bg-panel-2', icon: <SkipForward className="size-3.5" /> },
  cancelled: { label: 'Cancelled', cls: 'text-muted bg-panel-2', icon: <Ban className="size-3.5" /> },
}

export function StatusBadge({ status }: { status: Status }) {
  const s = statusStyle[status]
  return (
    <span className={cx('inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium whitespace-nowrap', s.cls)}>
      {s.icon}
      {s.label}
    </span>
  )
}

export function StatusIcon({ status, className = 'size-5' }: { status: Status; className?: string }) {
  switch (status) {
    case 'succeeded':
      return <CircleCheck className={cx(className, 'text-emerald-500')} />
    case 'failed':
      return <CircleX className={cx(className, 'text-red-500')} />
    case 'running':
      return <LoaderCircle className={cx(className, 'animate-spin text-sky-500')} />
    case 'awaiting-approval':
      return <CirclePause className={cx(className, 'text-amber-500')} />
    case 'skipped':
      return <SkipForward className={cx(className, 'text-muted')} />
    case 'cancelled':
      return <Ban className={cx(className, 'text-muted')} />
  }
  return <CircleDashed className={cx(className, 'text-muted')} />
}

export function SeverityBadge({ severity }: { severity: Severity }) {
  const map: Record<Severity, { cls: string; icon: ReactNode; label: string }> = {
    blocker: { cls: 'text-red-700 bg-red-500/15 dark:text-red-300', icon: <CircleX className="size-3.5" />, label: 'Blocker' },
    warning: { cls: 'text-amber-700 bg-amber-500/15 dark:text-amber-300', icon: <TriangleAlert className="size-3.5" />, label: 'Warning' },
    info: { cls: 'text-sky-700 bg-sky-500/15 dark:text-sky-300', icon: <Info className="size-3.5" />, label: 'Info' },
  }
  const s = map[severity]
  return (
    <span className={cx('inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs font-semibold', s.cls)}>
      {s.icon}
      {s.label}
    </span>
  )
}

export function Pill({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cx('inline-flex items-center rounded-md bg-panel-2 px-2 py-0.5 text-xs font-medium text-muted', className)}>{children}</span>
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 py-10 text-sm text-muted justify-center">
      <LoaderCircle className="size-4 animate-spin" />
      {label ?? 'Loading…'}
    </div>
  )
}

export function ErrorBox({ error, title = 'Something went wrong' }: { error: Error; title?: string }) {
  return (
    <div role="alert" className="rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-700 dark:text-red-300">
      <p className="font-semibold">{title}</p>
      <p className="mt-0.5 break-words">{error.message}</p>
    </div>
  )
}

export function Empty({ icon, title, children }: { icon: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="flex flex-col items-center px-6 py-14 text-center">
      <div className="mb-3 rounded-xl bg-panel-2 p-3 text-muted">{icon}</div>
      <p className="font-medium">{title}</p>
      {children && <div className="mt-1 max-w-md text-sm text-muted">{children}</div>}
    </div>
  )
}

export function Version({ from, to }: { from: string; to: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 font-mono text-sm tabular">
      <span>{from}</span>
      <span className="text-muted">→</span>
      <span className="font-semibold">{to}</span>
    </span>
  )
}

const providerStyle: Record<string, { label: string; cls: string }> = {
  eks: { label: 'EKS', cls: 'bg-amber-500/15 text-amber-700 dark:text-amber-300' },
  gke: { label: 'GKE', cls: 'bg-sky-500/15 text-sky-700 dark:text-sky-300' },
  aks: { label: 'AKS', cls: 'bg-indigo-500/15 text-indigo-700 dark:text-indigo-300' },
  kind: { label: 'kind', cls: 'bg-panel-2 text-muted' },
}

export function ProviderBadge({ provider }: { provider?: string }) {
  const p = providerStyle[provider ?? ''] ?? { label: provider || 'Kubernetes', cls: 'bg-panel-2 text-muted' }
  return <span className={cx('inline-flex items-center rounded-md px-1.5 py-0.5 text-[11px] font-semibold', p.cls)}>{p.label}</span>
}

export function EnvBadge({ env }: { env?: string }) {
  if (!env) return null
  const prod = env.startsWith('prod')
  return (
    <span className={cx('inline-flex items-center rounded-md px-1.5 py-0.5 text-[11px] font-semibold', prod ? 'bg-red-500/15 text-red-700 dark:text-red-300' : 'bg-panel-2 text-muted')}>
      {env}
    </span>
  )
}

export function Field({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="block">
      <span className="text-sm font-medium">{label}</span>
      <div className="mt-1.5">{children}</div>
      {hint && <span className="mt-1 block text-xs text-muted">{hint}</span>}
    </label>
  )
}

export const inputCls = 'w-full rounded-lg border border-line bg-bg px-3 py-2 text-sm outline-none focus:border-brand disabled:opacity-60'

export function Select({ value, onChange, children, className }: { value: string; onChange: (v: string) => void; children: ReactNode; className?: string }) {
  return (
    <select value={value} onChange={(e) => onChange(e.target.value)} className={cx(inputCls, 'pr-8', className)}>
      {children}
    </select>
  )
}
