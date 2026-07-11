#!/usr/bin/env bash
# changed-paths.sh — path-based change detection for the monorepo (04 §1.1).
#
# Prints the set of buildable top-level paths affected by a set of changed
# files, one per line, sorted & de-duplicated. Rules:
#   * services/<x>/**      -> services/<x>
#   * bffs/<y>/**          -> bffs/<y>
#   * gateway/**           -> gateway
#   * libs/**              -> EVERYTHING (a shared lib change rebuilds all,
#                            because any module may import it)
#   * everything else      -> no build impact (docs, contracts, deploy,
#                            tools, ci, root files) => contributes nothing
#
# Input modes:
#   tools/changed-paths.sh --base <ref>     diff shop/ against a git ref
#   tools/changed-paths.sh --files a b c    explicit shop-relative paths
#   printf '%s\n' a b | tools/changed-paths.sh --stdin
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

list_buildable() {
  # nullglob so an empty dir (e.g. bffs/ with only a README) expands to nothing
  # rather than a literal pattern that would trip the -d test under pipefail.
  shopt -s nullglob
  [ -d "$ROOT/gateway" ] && echo "gateway"
  for d in "$ROOT"/services/*/; do [ -d "$d" ] && echo "services/$(basename "$d")"; done
  for d in "$ROOT"/bffs/*/; do [ -d "$d" ] && echo "bffs/$(basename "$d")"; done
  shopt -u nullglob
  return 0
}

map_file() {
  # Echo the affected path for one shop-relative file, or __ALL__, or nothing.
  local f="$1"
  case "$f" in
    libs/*)     echo "__ALL__" ;;
    services/*) echo "services/$(printf '%s' "$f" | cut -d/ -f2)" ;;
    bffs/*)     echo "bffs/$(printf '%s' "$f" | cut -d/ -f2)" ;;
    gateway/*)  echo "gateway" ;;
    *)          : ;; # docs / contracts / deploy / tools / ci / root: no impact
  esac
}

collect_input() {
  local mode="$1"; shift
  case "$mode" in
    --base)
      local ref="$1"
      local top prefix
      top="$(git -C "$ROOT" rev-parse --show-toplevel)"
      # Path of shop/ relative to the git repo root (e.g. "shop/"), so we can
      # keep only files inside shop/ and re-root them to shop-relative paths.
      prefix="${ROOT#"$top"/}"
      git -C "$top" diff --name-only "$ref" -- "$prefix" \
        | sed "s#^${prefix}/##"
      ;;
    --files) printf '%s\n' "$@" ;;
    --stdin) cat ;;
    *) echo "usage: changed-paths.sh --base <ref> | --files <paths...> | --stdin" >&2; exit 2 ;;
  esac
}

main() {
  local mode="${1:---stdin}"; shift || true
  local files affected all=0
  files="$(collect_input "$mode" "$@")"

  affected="$(
    while IFS= read -r f; do
      [ -z "$f" ] && continue
      map_file "$f"
    done <<< "$files"
  )"

  if printf '%s\n' "$affected" | grep -qx "__ALL__"; then
    all=1
  fi

  if [ "$all" -eq 1 ]; then
    list_buildable | sort -u
  else
    printf '%s\n' "$affected" | grep -v '^$' | sort -u || true
  fi
}

main "$@"
