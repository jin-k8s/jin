#!/usr/bin/env bash
# End-to-end test against a throwaway kind cluster. Never touches your current kubeconfig context.
set -euo pipefail

CLUSTER="${JIN_E2E_CLUSTER:-jin-e2e}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
KCFG="$WORK/kubeconfig"
export JIN_HOME="$WORK/jin-home"

cleanup() {
  if [[ "${JIN_E2E_KEEP:-}" != "1" ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

kind create cluster --name "$CLUSTER" --kubeconfig "$KCFG" --wait 120s
K="kubectl --kubeconfig $KCFG"

# PDB that allows zero disruptions -> must be reported as a blocker.
$K create deployment web --image=registry.k8s.io/pause:3.10 --replicas=1
$K create poddisruptionbudget web --selector=app=web --min-available=1
$K rollout status deployment/web --timeout=120s

# Wait for the PDB controller to populate status.
for _ in $(seq 1 30); do
  [[ "$($K get pdb web -o jsonpath='{.status.expectedPods}')" == "1" ]] && break
  sleep 2
done

set +e
"$ROOT/bin/jin" plan --kubeconfig "$KCFG" -o json --fail-on-blockers >"$WORK/plan.json"
code=$?
set -e

fail() { echo "E2E FAIL: $*" >&2; cat "$WORK/plan.json" >&2; exit 1; }

[[ $code -eq 2 ]] || fail "expected exit code 2 (blockers), got $code"
jq -e '.status == "succeeded"' "$WORK/plan.json" >/dev/null || fail "run not succeeded"
jq -e '.cluster.provider == "kind"' "$WORK/plan.json" >/dev/null || fail "provider not detected as kind"
jq -e '[.plan.hops[].findings[] | select(.checkId == "pdb-drain" and (.resource | contains("default/web")))] | length == 1' \
  "$WORK/plan.json" >/dev/null || fail "PDB blocker not reported"
jq -e '.plan.hops[0].steps | map(.phase) | index("control-plane") != null' "$WORK/plan.json" >/dev/null || fail "no control-plane step"

id="$(jq -r .id "$WORK/plan.json")"
"$ROOT/bin/jin" runs list | grep -q "$id" || fail "run $id not listed"
"$ROOT/bin/jin" runs show "$id" -o markdown | grep -q "BLOCKER" || fail "runs show did not render"

echo "E2E OK: run $id"
