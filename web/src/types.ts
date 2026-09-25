export type Severity = 'blocker' | 'warning' | 'info'

export interface Finding {
  checkId: string
  severity: Severity
  hop: string
  title: string
  resource?: string
  detail?: string
  remediation?: string
}

export interface PlanStep {
  phase: string
  title: string
  detail?: string
}

export type DataPlaneAction = 'required' | 'optional' | 'final'

export interface PlanHop {
  from: string
  to: string
  dataPlane: DataPlaneAction
  findings: Finding[] | null
  steps: PlanStep[] | null
}

export interface Plan {
  support?: SupportAssessment
  provider: string
  current: string
  target: string
  hops: PlanHop[]
  summary: { blockers: number; warnings: number; info: number }
  ready: boolean
  complete: boolean
  collectionWarnings?: string[]
}

export interface RunRecord {
  id: string
  kind: string
  tool: { version: string; commit: string }
  startedAt: string
  finishedAt: string
  status: 'succeeded' | 'failed'
  error?: string
  cluster: { context: string; provider?: string; version: string; gitVersion?: string; nodes: number; helmReleases: number }
  plan?: Plan
}

export interface RunSummary {
  id: string
  startedAt: string
  status: 'succeeded' | 'failed'
  error?: string
  context: string
  provider?: string
  current: string
  target: string
  hops: number
  blockers: number
  warnings: number
  ready: boolean
  incomplete: boolean
}

export interface EKSRef {
  name: string
  region?: string
  profile?: string
  roleArn?: string
}

export interface KubeContext {
  name: string
  cluster: string
  current: boolean
  eks?: EKSRef
  gke?: GKERef
  aks?: AKSRef
  environment?: string
  gitops: boolean
  registered?: boolean
}

export interface GitHubIntegration {
  configured: boolean
  source?: string
  hint?: string
  setAt?: string
  setBy?: string
  login?: string
  scopes?: string
}

export type Status = 'pending' | 'awaiting-approval' | 'running' | 'succeeded' | 'failed' | 'skipped' | 'cancelled'

export interface Stage {
  name: string
  status: Status
  startedAt?: string
  finishedAt?: string
  message?: string
}

export interface Approval {
  by: string
  subject?: string
  at: string
  action: 'approve' | 'retry'
  comment?: string
  overrideBlockers?: boolean
}

export interface UpgradeHop {
  index: number
  from: string
  to: string
  dataPlane: DataPlaneAction
  blockers: number
  status: Status
  stages: Stage[]
  approvals?: Approval[]
  requiredApprovals?: number
  policy?: string
}

export interface Upgrade {
  id: string
  mode: 'direct' | 'gitops'
  policy?: PolicyView
  createdBySubject?: string
  cluster: { context: string; provider: string; environment?: string; eks?: EKSRef; gke?: GKERef; aks?: AKSRef }
  planRunId: string
  from: string
  to: string
  status: Status
  currentHop: number
  hops: UpgradeHop[]
  error?: string
  createdBy: string
  createdAt: string
  updatedAt: string
  finishedAt?: string
}

export interface UpgradeEvent {
  seq: number
  time: string
  hop?: number
  stage?: string
  level: 'info' | 'warn' | 'error' | 'state'
  message: string
  progress?: { done: number; total: number; unit: string }
}

export interface Info {
  version: string
  commit: string
  modes: string[]
}

export type Role = 'viewer' | 'planner' | 'approver' | 'admin'

export interface Session {
  authenticated: boolean
  sso: boolean
  tokenLogin: boolean
  role?: Role
  identity?: { name: string; email?: string; groups?: string[]; method: 'token' | 'oidc'; subject: string }
}

export interface SupportAssessment {
  provider: string
  current: string
  currentStatus: string
  endOfStandardSupport?: string
  endOfExtendedSupport?: string
  daysToEndOfStandardSupport?: number
  extendedSupportCostPerYearUsd: number
  currentSurchargePerYearUsd: number
  target: string
  targetEndOfStandardSupport?: string
  pricingSource: string
}

export interface GKERef {
  project: string
  location: string
  name: string
}

export interface AKSRef {
  subscriptionId: string
  resourceGroup: string
  name: string
}

export interface FleetRow extends KubeContext {
  provider: string
  version?: string
  nodes: number
  reachable: boolean
  error?: string
  lastPlan?: RunSummary & { support?: SupportAssessment }
  activeUpgrade?: { id: string; status: Status; from: string; to: string; currentHop: number; hops: number }
  policy: string
}

export interface GitOpsTarget {
  role: 'control-plane' | 'node-group' | 'addon'
  name?: string
  kind: 'terraform' | 'yaml'
  file: string
  address?: string
  attribute?: string
  path?: string
  match?: { apiVersionPrefix?: string; kind?: string; name?: string }
  format?: string
}

export interface GitOpsSettings {
  provider: 'github'
  baseUrl?: string
  owner: string
  repo: string
  baseBranch?: string
  tokenEnv?: string
  targets: GitOpsTarget[]
  applyTimeoutMinutes?: number
}

export interface ClusterSettings {
  context: string
  environment?: string
  gitops?: GitOpsSettings
  gke?: GKERef
  aks?: AKSRef
}

export interface Candidate {
  target: GitOpsTarget
  current: string
  provider: string
  cluster?: string
  error?: string
  suggested: boolean
}

export interface PolicyView {
  name: string
  requiredApprovals: number
  forbidSelfApproval: boolean
  requireComment: boolean
  requiredGroups?: string[]
  windows: string
  inWindow: boolean
  match?: { environments?: string[] | null; contexts?: string[] | null }
}

export interface ParityItem {
  category: string
  name: string
  status: 'missing' | 'not-ready' | 'mismatch' | 'action' | 'ok'
  source?: string
  target?: string
  detail?: string
}

export interface Parity {
  source: string
  target: string
  sourceVersion: string
  targetVersion: string
  items: ParityItem[] | null
  summary: { missing: number; notReady: number; mismatch: number; actions: number; ok: number }
  ready: boolean
  checklist: string[]
  warnings?: string[]
}

export interface MigrationItem {
  category: string
  binding: string
  mapping: string
  effort: 'low' | 'medium' | 'high'
  count: number
  examples?: string[]
}

export interface Assessment {
  context: string
  source: string
  target: string
  items: MigrationItem[] | null
  summary: { items: number; byCategory: Record<string, number>; effortPoints: number; dataGiB: number; externalEndpoints: number; workloads: number }
  size: 'S' | 'M' | 'L' | 'XL'
  notes: string[]
  warnings?: string[]
}

export interface AuditEntry {
  seq: number
  time: string
  actor: string
  role?: string
  action: string
  target?: string
  detail?: string
  remote?: string
  hash: string
}
