#!/usr/bin/env bash
# End-to-end smoke test against a live moose deployment (Mac Mini or AWS).
#
# Required env: MOOSE_URL, MOOSE_PAT (an admin PAT, for /v1/audit),
# MOOSE_E2E_QUERY (a term that matches a shared Drive file),
# MOOSE_E2E_DOC_ID (the id of a file shared with the service account).
# Optional: MOOSE_BIN (moose CLI to use; default "moose" on PATH).
# Needs curl and jq.
set -euo pipefail

: "${MOOSE_URL:?MOOSE_URL is required}"
: "${MOOSE_PAT:?MOOSE_PAT is required}"
: "${MOOSE_E2E_QUERY:?MOOSE_E2E_QUERY is required}"
: "${MOOSE_E2E_DOC_ID:?MOOSE_E2E_DOC_ID is required}"
MOOSE_BIN="${MOOSE_BIN:-moose}"
MOOSE_URL="${MOOSE_URL%/}"
export MOOSE_URL MOOSE_PAT

for tool in curl jq "$MOOSE_BIN"; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# 1. healthz 200 without auth.
code="$(curl -sS -o "$tmp/healthz" -w '%{http_code}' "$MOOSE_URL/healthz")"
[[ "$code" == 200 ]] || fail "healthz returned $code"
jq -e '.ok == true' "$tmp/healthz" >/dev/null || fail "healthz body: $(cat "$tmp/healthz")"
pass "healthz 200"

# 2. whoami 200 with the PAT.
code="$(curl -sS -o "$tmp/whoami" -w '%{http_code}' -H "Authorization: Bearer $MOOSE_PAT" "$MOOSE_URL/v1/whoami")"
[[ "$code" == 200 ]] || fail "whoami returned $code"
pass "whoami 200 ($(jq -r '.login' "$tmp/whoami"))"

# 3. No token: 401 with WWW-Authenticate.
code="$(curl -sS -D "$tmp/noauth.h" -o /dev/null -w '%{http_code}' "$MOOSE_URL/v1/whoami")"
[[ "$code" == 401 ]] || fail "unauthenticated whoami returned $code, want 401"
grep -qi '^www-authenticate:' "$tmp/noauth.h" || fail "401 without WWW-Authenticate header"
pass "no token: 401 with WWW-Authenticate"

# 4. moose sources lists gdrive healthy.
"$MOOSE_BIN" sources --json >"$tmp/sources"
jq -e '(.sources // .) | any(.id == "gdrive" and .healthy == true)' "$tmp/sources" >/dev/null \
  || fail "gdrive not healthy: $(cat "$tmp/sources")"
pass "sources: gdrive healthy"

# 5. moose search returns at least one item.
"$MOOSE_BIN" search "$MOOSE_E2E_QUERY" --json >"$tmp/search"
n="$(jq '.items | length' "$tmp/search")"
[[ "$n" -ge 1 ]] || fail "search \"$MOOSE_E2E_QUERY\" returned no items"
pass "search: $n item(s)"

# 6. moose get returns non-empty content.
"$MOOSE_BIN" get "$MOOSE_E2E_DOC_ID" >"$tmp/get"
[[ -s "$tmp/get" ]] || fail "get $MOOSE_E2E_DOC_ID printed nothing"
pass "get: $(wc -c <"$tmp/get" | tr -d ' ') bytes"

# 7. get of a random unshared id exits 3 (not found).
rc=0
"$MOOSE_BIN" get "moose-e2e-unshared-$RANDOM$RANDOM" >/dev/null 2>&1 || rc=$?
[[ "$rc" == 3 ]] || fail "get of unshared id exited $rc, want 3"
pass "get unshared id: exit 3"

# 8. Raw MCP initialize then tools/list lists three tools.
mcp() {
  curl -sS -X POST "$MOOSE_URL/mcp" \
    -H "Authorization: Bearer $MOOSE_PAT" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d "$1"
}
# With an SSE response, keep only the data payload.
mcp_json() { sed -n 's/^data: //p; /^{/p'; }
mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"moose-e2e","version":"0"}}}' \
  | mcp_json >"$tmp/init"
jq -e '.result.serverInfo.name' "$tmp/init" >/dev/null || fail "MCP initialize: $(cat "$tmp/init")"
pass "mcp initialize ($(jq -r '.result.serverInfo.name' "$tmp/init"))"
mcp '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' | mcp_json >"$tmp/tools"
tools="$(jq -r '[.result.tools[].name] | sort | join(",")' "$tmp/tools")" || fail "MCP tools/list: $(cat "$tmp/tools")"
[[ "$tools" == "get_document,list_sources,search" ]] || fail "MCP tools: got $tools"
pass "mcp tools/list: $tools"

# 9. /v1/audit shows these calls.
curl -sS -f -H "Authorization: Bearer $MOOSE_PAT" "$MOOSE_URL/v1/audit?since=$started&limit=500" >"$tmp/audit" \
  || fail "GET /v1/audit failed (is MOOSE_PAT an admin token?)"
for action in search get_document; do
  jq -e --arg a "$action" '.events | any(.action == $a)' "$tmp/audit" >/dev/null \
    || fail "audit has no $action event since $started"
done
jq -e '.events | any(.status == 401)' "$tmp/audit" >/dev/null || fail "audit has no 401 event since $started"
jq -e '.events | any(.surface == "mcp")' "$tmp/audit" >/dev/null || fail "audit has no mcp event since $started"
pass "audit: $(jq '.events | length' "$tmp/audit") event(s) since $started"

echo "smoke test passed against $MOOSE_URL"
