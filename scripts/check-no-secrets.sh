#!/usr/bin/env bash
# Fails the build if anything from the private platform repository, or any
# internal address, reaches this public repository. See docs/adr/0003.
set -uo pipefail

cd "$(dirname "$0")/.."

# Patterns are held base64-encoded so that this file, which is itself scanned,
# does not contain the literals it forbids.
PATTERNS_B64="REJfQ09OTkVDVElPTl9TVFJJTkcKUEFZU1RBQ0sKYmVhdmVyYXBpL2ludGVybmFsCnN2Yy5jbHVzdGVyLmxvY2FsCmRiYWFzLWNwLnRlY2hiZWF2ZXIuaW8KY29udHJvbC50ZWNoYmVhdmVyLmlvCjE4NS4xNDEuNjEuNzcKMTkzLjM3LjIxMy4xOTQKMTg1LjIwNS4yMDkuCkJFR0lOIFJTQSBQUklWQVRFIEtFWQpCRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZCg=="

fail=0
while IFS= read -r pattern; do
  [ -z "$pattern" ] && continue
  hits=$(grep -rInF --binary-files=without-match \
    --exclude-dir=.git \
    --exclude-dir=dist \
    --exclude="check-no-secrets.sh" \
    -- "$pattern" . 2>/dev/null || true)
  if [ -n "$hits" ]; then
    echo "FORBIDDEN CONTENT in a public repository:"
    echo "$hits"
    echo
    fail=1
  fi
done <<< "$(printf '%s' "$PATTERNS_B64" | base64 -d)"

if [ "$fail" -ne 0 ]; then
  echo "See docs/adr/0003-public-repository-boundary.md."
  exit 1
fi
echo "no forbidden content found"
