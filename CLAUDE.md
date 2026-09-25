# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make build        # web UI (web/ → internal/server/ui/dist) + Go binary bin/jin
make build-go     # Go only; uses whatever UI assets are already built
make test         # go test -race ./... and UI type-check
make lint         # golangci-lint v2 (.golangci.yml); `make fmt` applies gofmt+goimports
make e2e          # hack/e2e.sh: throwaway kind cluster, asserts on `jin plan -o json` via jq
make ui-dev       # Vite dev server proxying /api to a running `jin server` on :7420
go test ./internal/upgrade -run TestMultiApproverPolicy -v   # single test
```

npm must be run with `--ignore-scripts` (enforced by the developer's npm config).

Never run `jin plan`, `jin server` or kubectl against the developer's current kube context without explicit
permission: it points at a real EKS cluster. Use kind with a temp kubeconfig and a temp `JIN_HOME`.

## Architecture

```
inventory → check → plan → runrecord ─► report (CLI)
snapshot → parity (blue/green) · migrate (cross-cloud)          app: kubeconfig, Env.Plan/Snapshot, Connector
server (API, SSE, UI, auth, RBAC, policy, audit) ─► upgrade.Engine ─► executor/{eks,gke,aks} | gitops ─► verify
```

- `inventory`: the only planner code reading cluster state; it must stay **read-only (get/list)**. Only the
  server-version read is fatal; other failures become `Inventory.Warnings` → `Plan.Complete=false`. Declared
  API versions come from Helm release Secrets (only apiVersion/kind/name retained; a test asserts values never
  leak) and last-applied annotations.
- `compat`: embedded strict YAML knowledge base; quote versions; `verifiedThrough` drives `kb-coverage`.
  Never add data without an upstream source.
- `support`: EKS support calendar (`DescribeClusterVersions`) and extended-support cost; list prices only.
- `check` → `plan`: findings carry `Hop` (version that must not be reached first). Single-minor hops;
  data-plane action per hop from the skew policy.
- `upgrade`: execution engine. Hop = preflight → control-plane → add-ons → data-plane → verify.
  - State: `$JIN_HOME/upgrades/<id>.json` + append-only `<id>.events.jsonl`; single process. `mu` guards
    documents, `evMu` guards events; never emit while holding `mu` inside `mutate`.
  - Invariants: a hop starts only when `len(hop.Approvers()) >= Required` (distinct subjects); one active
    (`awaiting-approval|running|failed`) upgrade per context; `Retry` resumes the failed stage; `Resume`
    restarts `running` upgrades; shutdown leaves state untouched; only `Cancel` ends a run.
  - **Executors must be idempotent** (check state first, attach to in-flight operations or PRs). Executors
    may implement `StageTimeouter` (GitOps waits days for review).
- `executor/eks|gke|aks`: provider APIs behind small interfaces/HTTP; tests use stateful fakes. AKS waits
  require state *and* version to match (ARM can report a stale `Succeeded` right after a PUT).
- `gitops`: formatting-preserving edits (hclwrite for Terraform incl. `var.*`/`local.*` resolution; in-place
  scalar replacement for YAML), repository discovery, GitHub REST client, executor (one PR per stage,
  deterministic branch `jin/<upgrade>/<version>-<stage>`, waits for merge then observes the cluster).
- `clusters`: per-context settings (`$JIN_HOME/clusters.json`): environment, GitOps repo and targets, AKS/GKE
  identity. Tokens are never stored; `tokenEnv` must match `^(GITHUB|JIN_GITHUB)_…`.
- `secrets`: AES-256-GCM store for UI-entered credentials (`$JIN_HOME/secrets.json`, key in `secrets.key`,
  both 0600; the secret name is bound as AAD). Values are write-only through the API and never audited.
  GitHub token precedence (`Env.GitHubToken`): explicit `tokenEnv` → stored token → `GITHUB_TOKEN`.
- Registered clusters (`clusters.Registration`, context `eks:<region>:<name>`) are added from the UI and
  reached without a kubeconfig: `app/eks.go` builds a rest.Config with the stored endpoint/CA and an STS
  presigned token (`k8s-aws-v1.…`, refreshed every 10 min). `AWSClients` is the test seam. A missing
  kubeconfig is valid. Never accept cloud access keys in the UI; use profiles/SSO/roles on the server.
- `snapshot`, `migrate`: read-only workload/cloud-binding snapshot; blue/green `Compare`; `Assess` maps
  bindings to the target cloud with effort points → S/M/L/XL.
- `config`, `auth`, `policy`, `audit`: `$JIN_HOME/config.yaml` (strict) for OIDC, RBAC bindings, approval
  policies and allowed GitHub Enterprise hosts. Sessions are HMAC-signed cookies with strict base64; roles
  are re-evaluated per request; Bearer/token sessions are admin. Policies are enforced in the server
  (`approve`, `retry`), the engine counts distinct approvers. Audit is a SHA-256 hash chain in
  `$JIN_HOME/audit.jsonl`; CSV export neutralises formulas.
- `server`: routes registered with a minimum role via `authorize`; cookie writes require `X-Jin-Request: 1`
  (CSRF); loopback listeners reject foreign `Host`. Record security-relevant actions with `s.record`.
  Repository base URLs must pass `Config.AllowedGitHubURL` (prevents sending tokens elsewhere).
- `web/`: React 19, React Router 8, Tailwind v4, Vite 8, TypeScript 7. `src/types.ts` mirrors the Go JSON;
  `useCan(role)` hides controls, but the server is the enforcement point. The logo is `web/public/jin-icon.svg`,
  a copy of `docs/assets/jin-icon.svg`.

Module path `github.com/jin-k8s/jin` matches the GitHub org `jin-k8s`.
