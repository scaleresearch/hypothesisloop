#!/usr/bin/env bash
# Prints the image tag for the working tree: the short HEAD SHA, plus a hash of the uncommitted
# changes and untracked files when there are any. Hashes without -w, so nothing lands in .git.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

head="$(git rev-parse --short HEAD)"
untracked="$(git ls-files --others --exclude-standard)"
if git diff HEAD --quiet && [[ -z "${untracked}" ]]; then
  echo "${head}"
  exit 0
fi

dirty="$(
  {
    git diff HEAD --binary
    if [[ -n "${untracked}" ]]; then
      printf '%s\n' "${untracked}"
      printf '%s\n' "${untracked}" | git hash-object --stdin-paths
    fi
  } | git hash-object --stdin
)"
echo "${head}-${dirty:0:7}"
