#!/usr/bin/env bash
# Every experiment definition's Dockerfiles (its own Dockerfile.experimentator, plus
# seed/Dockerfile*) are the contract for a run: every file one of them COPYs in, or that COPY'd
# Python imports, has to actually be in the repo, or `make experimentator-image` /
# `make *-workload-image` (or a from-clean-checkout agent) fails/breaks in ways that only show up
# mid-run. Catch both classes of drift here instead:
#   1. a COPY-ed file missing from the seed dir entirely
#   2. a file present on disk but not tracked in git (invisible to a clean checkout/clone)
set -euo pipefail

cd "$(dirname "$0")/.."
status=0

for dockerfile in agents/coordinator/experiments/*/Dockerfile.experimentator agents/coordinator/experiments/*/seed/Dockerfile*; do
  [[ -f "$dockerfile" ]] || continue
  seed_dir="$(dirname "$dockerfile")"

  # COPY <src...> <dst> -- everything but the last token is a source; drop the leading "COPY".
  # A --from=<stage> COPY's src is a path inside that build stage, not the host tree -- skip the
  # whole instruction (its "src" isn't something check-experiment-seeds can validate on disk).
  while read -r -a args; do
    from_stage=0
    [[ "${args[0]}" == "--from="* ]] && from_stage=1
    unset 'args[0]'
    [[ "$from_stage" -eq 1 ]] && continue
    unset 'args[${#args[@]}]' 2>/dev/null || true
    for src in "${args[@]}"; do
      [[ "$src" == "." ]] && continue
      f="$seed_dir/$src"
      if [[ ! -f "$f" ]]; then
        echo "check-experiment-seeds: $dockerfile COPYs '$src' but $f does not exist" >&2
        status=1
      # Some experiment dirs (e.g. smri-fm) are their own nested git repo, not a submodule
      # entry -- `git ls-files` from this (outer) repo's root can never see files tracked inside
      # one, so resolve tracking relative to whichever repo actually owns the file.
      elif ! ( cd "$(dirname "$f")" && git ls-files --error-unmatch "$(basename "$f")" ) >/dev/null 2>&1; then
        echo "check-experiment-seeds: $f exists but is not tracked in git (run: git add $f)" >&2
        status=1
      fi
    done
  done < <(grep -E '^\s*COPY\s' "$dockerfile" | sed -E 's/^\s*COPY\s+//' | sed -E 's/\s+/ /g')
done

if [[ "$status" -eq 0 ]]; then
  echo "check-experiment-seeds: OK"
fi
exit "$status"
