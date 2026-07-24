#!/usr/bin/env bash
# Stop: vet and test the module; if either fails, block so the break gets fixed
# before the turn ends. `go test` compiles everything, so this subsumes a build check.
dir="${CLAUDE_PROJECT_DIR:-.}"
cd "$dir" || exit 0

# Nothing to check until the module actually has packages.
[ -n "$(go list ./... 2>/dev/null)" ] || exit 0

fail=""
if out=$(go vet ./... 2>&1); then :; else
  fail="go vet ./... failed:"$'\n'"$out"
fi
if out=$(go test ./... 2>&1); then :; else
  fail="${fail:+$fail$'\n\n'}go test ./... failed:"$'\n'"$out"
fi

if [ -n "$fail" ]; then
  jq -n --arg r "$fail"$'\n'"Fix before finishing." '{decision:"block", reason:$r}'
fi
exit 0
