import { createContext, useContext } from 'react'
import type { Role, Session } from './types'

const rank: Record<Role, number> = { viewer: 0, planner: 1, approver: 2, admin: 3 }

export const SessionContext = createContext<Session>({ authenticated: false, sso: false, tokenLogin: false })

export function useSession() {
  return useContext(SessionContext)
}

/** can reports whether the signed-in user holds at least `role`. The server enforces the same rule. */
export function useCan() {
  const s = useSession()
  return (role: Role) => !!s.role && rank[s.role] >= rank[role]
}
