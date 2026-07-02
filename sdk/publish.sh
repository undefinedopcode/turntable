#!/usr/bin/env bash
# Publish the plugin SDKs to their standalone repositories.
#
# The monorepo's sdk/<lang> directories are the canonical source; each publish
# is a `git subtree split` (history-preserving, stable commit ids for unchanged
# history) force-pushed to the standalone repo's main branch. Everything in
# each directory is written to be standalone-ready (module paths, absolute
# README links), so the split needs no adaptation commits.
#
#   ./sdk/publish.sh          # publish all three
#   ./sdk/publish.sh go       # publish one
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

declare -A remotes=(
  [go]="git@github.com:undefinedopcode/turntable-go-sdk.git"
  [python]="git@github.com:undefinedopcode/turntable-python-sdk.git"
  [node]="git@github.com:undefinedopcode/turntable-node-sdk.git"
)

langs=("$@")
if [ ${#langs[@]} -eq 0 ]; then
  langs=(go python node)
fi

for lang in "${langs[@]}"; do
  remote="${remotes[$lang]:-}"
  if [ -z "$remote" ]; then
    echo "unknown sdk '$lang' (want go, python or node)" >&2
    exit 1
  fi
  echo "publishing sdk/$lang -> $remote"
  commit="$(git subtree split --prefix="sdk/$lang" HEAD)"
  git push --force "$remote" "$commit:refs/heads/main"
done

echo "done."
