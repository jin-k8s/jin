import { useEffect, useState } from 'react'
import { BrowserRouter, Route, Routes } from 'react-router'
import { KeyRound, LogIn } from 'lucide-react'
import { api, onUnauthorized, useResource } from './api'
import { Layout } from './components/Layout'
import { Button, Logo, Spinner } from './components/ui'
import { ClusterSettingsPage } from './pages/ClusterSettings'
import { Compare } from './pages/Compare'
import { Fleet } from './pages/Fleet'
import { Governance } from './pages/Governance'
import { Integrations } from './pages/Integrations'
import { Migrate } from './pages/Migrate'
import { Overview } from './pages/Overview'
import { PlanDetail } from './pages/PlanDetail'
import { Plans } from './pages/Plans'
import { UpgradeDetail } from './pages/UpgradeDetail'
import { Upgrades } from './pages/Upgrades'
import { SessionContext } from './session'
import type { Session } from './types'

function SignIn({ session }: { session: Session }) {
  return (
    <div className="flex min-h-full items-center justify-center p-6">
      <div className="w-full max-w-md rounded-xl border border-line bg-panel p-8 text-center shadow-sm">
        <div className="flex justify-center">
          <Logo size={48} />
        </div>
        <h1 className="mt-4 text-xl font-semibold">Sign in to Jin</h1>
        {session.sso && (
          <a href="/auth/login" className="mt-6 block">
            <Button variant="primary" className="w-full" icon={<LogIn className="size-4" />}>
              Continue with single sign-on
            </Button>
          </a>
        )}
        {session.tokenLogin && (
          <>
            {session.sso && <div className="my-5 text-xs text-muted uppercase">or</div>}
            <p className={`${session.sso ? '' : 'mt-2 '}text-sm text-muted`}>
              Open the link printed by <code className="font-mono">jin server</code> in your terminal. It signs this browser in as an administrator.
            </p>
            <div className="mt-4 flex items-center justify-center gap-2 rounded-lg bg-panel-2 px-3 py-2 font-mono text-xs text-muted">
              <KeyRound className="size-3.5" /> http://localhost:7420/?token=…
            </div>
          </>
        )}
      </div>
    </div>
  )
}

function NotFound() {
  return <p className="text-muted">Page not found.</p>
}

export function App() {
  const session = useResource(api.session, [])
  const info = useResource(api.info, [session.data?.authenticated])
  const [expired, setExpired] = useState(false)
  useEffect(() => onUnauthorized(() => setExpired(true)), [])

  if (!session.data) return <Spinner />
  if (!session.data.authenticated || expired) return <SignIn session={session.data} />

  return (
    <SessionContext.Provider value={session.data}>
      <BrowserRouter>
        <Layout info={info.data}>
          <Routes>
            <Route path="/" element={<Overview />} />
            <Route path="/fleet" element={<Fleet />} />
            <Route path="/clusters" element={<Fleet />} />
            <Route path="/clusters/settings" element={<ClusterSettingsPage />} />
            <Route path="/plans" element={<Plans />} />
            <Route path="/plans/:id" element={<PlanDetail />} />
            <Route path="/upgrades" element={<Upgrades />} />
            <Route path="/upgrades/:id" element={<UpgradeDetail />} />
            <Route path="/compare" element={<Compare />} />
            <Route path="/migrate" element={<Migrate />} />
            <Route path="/governance" element={<Governance />} />
            <Route path="/integrations" element={<Integrations />} />
            <Route path="*" element={<NotFound />} />
          </Routes>
        </Layout>
      </BrowserRouter>
    </SessionContext.Provider>
  )
}
