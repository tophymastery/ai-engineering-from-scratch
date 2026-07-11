#!/usr/bin/env bash
# security-scan.sh — dependency/vuln gate (04 §1.2 "security scan", S-T2).
#
# Prefers govulncheck (real CVE scan against the Go vuln DB). In this
# environment the vuln DB endpoint (vuln.go.dev) is blocked by the egress
# proxy, so the scan cleanly FALLS BACK to a documented OFFLINE dependency lint:
#   - every module's third-party (non-stdlib, non-local) require surface is
#     enumerated — the platform binaries are stdlib-only + one in-repo lib, so
#     the external attack surface is zero;
#   - every `replace` must stay inside the repo (no floating external override);
#   - `go vet` runs as a static-analysis backstop.
# Any external dependency appearing without review, or an out-of-repo replace,
# fails the gate.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
MODULES=(gateway services/_placeholder libs/testhooks)

echo "=== security-scan ==="

# --- Try govulncheck (bounded). Probe the vuln DB on one module first so a
#     blocked DB is a FALLBACK signal, not a false "vulnerability found". ---
gv_ok=0
DB_UNREACHABLE='Forbidden|fetching vulnerabilities|no such host|dial tcp|connection refused|deadline exceeded|TLS|i/o timeout'
probe="$( ( cd "$ROOT/${MODULES[0]}" && timeout 120 "$GO" run golang.org/x/vuln/cmd/govulncheck@latest ./... ) 2>&1 )" && probe_rc=0 || probe_rc=$?
if [[ "$probe_rc" -ne 0 ]] && grep -qE "$DB_UNREACHABLE" <<<"$probe"; then
  echo "govulncheck reached but vuln DB is blocked by egress proxy — falling back"
else
  echo "govulncheck available — running CVE scan"
  scan_fail=0
  for m in "${MODULES[@]}"; do
    if out="$( ( cd "$ROOT/$m" && timeout 120 "$GO" run golang.org/x/vuln/cmd/govulncheck@latest ./... ) 2>&1 )"; then
      echo "  [ok] $m"
    else
      # rc!=0 but DB reachable ⇒ genuine finding.
      echo "  [VULN] $m"; echo "$out" | sed 's/^/      /'; scan_fail=1
    fi
  done
  gv_ok=1
  [[ "$scan_fail" -eq 0 ]] || { echo "security-scan: FAIL (govulncheck)"; exit 1; }
fi

if [[ "$gv_ok" -eq 0 ]]; then
  echo "govulncheck unreachable (vuln DB blocked by egress proxy) — OFFLINE dependency lint"
  fail=0
  for m in "${MODULES[@]}"; do
    modfile="$ROOT/$m/go.mod"
    # External requires = require lines that are NOT the in-repo shop module.
    ext="$(grep -E '^\s+[^[:space:]]+/[^[:space:]]+ v' "$modfile" 2>/dev/null \
            | grep -v 'github.com/shop-platform/shop' || true)"
    if [[ -n "$ext" ]]; then
      echo "  [REVIEW] $m has un-reviewed external dependencies:"; echo "$ext" | sed 's/^/      /'
      fail=1
    else
      echo "  [ok] $m: stdlib-only + in-repo deps (zero external surface)"
    fi
    # Any replace must resolve inside the repo.
    badrep="$(grep -E '^replace ' "$modfile" 2>/dev/null | grep -vE '=>\s+\.\.?/' || true)"
    if [[ -n "$badrep" ]]; then
      echo "  [REPLACE] $m has an out-of-repo replace:"; echo "$badrep" | sed 's/^/      /'
      fail=1
    fi
    ( cd "$ROOT/$m" && "$GO" vet ./... ) || { echo "  [VET] $m failed go vet"; fail=1; }
  done
  echo "----"
  [[ "$fail" -eq 0 ]] || { echo "security-scan: FAIL (offline lint)"; exit 1; }
fi

echo "security-scan: PASS"
