#!/usr/bin/env bash
# PostToolUse(Write|Edit): format the edited Go file in place.
f=$(jq -r '.tool_input.file_path // .tool_response.filePath // empty')
case "$f" in
  *.go) [ -f "$f" ] && gofmt -w "$f" ;;
esac
exit 0
