import type { ReactNode } from 'react'
import { NavLink } from 'react-router'
import { ArrowLeftRight, Boxes, FileSearch, LayoutDashboard, LogOut, Plane, Plug, Rocket, ShieldCheck } from 'lucide-react'
import { api } from '../api'
import { useSession } from '../session'
import { Logo, cx } from './ui'
import type { Info } from '../types'

const nav = [
  { to: '/', label: 'Overview', icon: LayoutDashboard, end: true },
  { to: '/fleet', label: 'Fleet', icon: Boxes },
  { to: '/plans', label: 'Plans', icon: FileSearch },
  { to: '/upgrades', label: 'Upgrades', icon: Rocket },
  { to: '/compare', label: 'Blue/green', icon: ArrowLeftRight },
  { to: '/migrate', label: 'Migrate', icon: Plane },
  { to: '/governance', label: 'Governance', icon: ShieldCheck },
  { to: '/integrations', label: 'Integrations', icon: Plug },
]

export function Layout({ info, children }: { info?: Info; children: ReactNode }) {
  const s = useSession()
  const signOut = async () => {
    await api.logout()
    window.location.href = '/'
  }
  return (
    <div className="flex min-h-full">
      <aside className="sticky top-0 hidden h-screen w-60 shrink-0 flex-col border-r border-line bg-panel md:flex">
        <div className="flex items-center gap-2.5 px-5 py-5">
          <Logo />
          <div>
            <div className="text-lg leading-none font-bold tracking-tight">jin</div>
            <div className="mt-0.5 text-[11px] text-muted">Kubernetes upgrades</div>
          </div>
        </div>
        <nav className="flex flex-col gap-0.5 px-3">
          {nav.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                cx(
                  'flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm font-medium transition',
                  isActive ? 'bg-brand/10 text-brand' : 'text-muted hover:bg-panel-2 hover:text-fg',
                )
              }
            >
              <Icon className="size-4" />
              {label}
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto border-t border-line px-5 py-4 text-xs text-muted">
          <div className="flex items-start justify-between gap-2">
            <div className="min-w-0">
              <div className="truncate font-medium text-fg">{s.identity?.email || s.identity?.name}</div>
              <div className="mt-0.5">
                <span className="rounded bg-panel-2 px-1.5 py-0.5 font-medium">{s.role}</span>
                <span className="ml-1.5">{s.identity?.method === 'oidc' ? 'SSO' : 'token'}</span>
              </div>
            </div>
            <button onClick={signOut} className="rounded-md p-1.5 hover:bg-panel-2 hover:text-fg" title="Sign out" aria-label="Sign out">
              <LogOut className="size-4" />
            </button>
          </div>
          {info && <div className="mt-2 font-mono">jin {info.version}</div>}
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center gap-3 border-b border-line bg-panel px-4 py-3 md:hidden">
          <Logo size={24} />
          <span className="font-bold">jin</span>
          <nav className="ml-auto flex gap-0.5 overflow-x-auto">
            {nav.map(({ to, icon: Icon, end, label }) => (
              <NavLink
                key={to}
                to={to}
                end={end}
                aria-label={label}
                className={({ isActive }) => cx('rounded-md p-2', isActive ? 'bg-brand/10 text-brand' : 'text-muted')}
              >
                <Icon className="size-4" />
              </NavLink>
            ))}
          </nav>
        </header>
        <main className="mx-auto w-full max-w-7xl flex-1 px-4 py-6 md:px-8 md:py-8">{children}</main>
      </div>
    </div>
  )
}
