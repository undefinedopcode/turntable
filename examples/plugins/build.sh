#!/usr/bin/env bash
# Build every example plugin connector into ./bin/<name>.
#
# Each plugin is its own Go module (as a real external plugin would be), so they
# are not built by `go build ./...` from the repo root — run this instead.
#
#   ./examples/plugins/build.sh
#
# Then point a `plugin` source's `command:` at the built binary (see PLUGINS.md).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
mkdir -p "$root/bin"

for dir in "$root"/examples/plugins/*/; do
  [ -f "$dir/go.mod" ] || continue
  name="$(basename "$dir")"
  echo "building $name -> bin/$name"
  (cd "$dir" && go build -o "$root/bin/$name" .)
done

echo "done."
