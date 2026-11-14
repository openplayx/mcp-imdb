#!/usr/bin/env bash
# Live check of a running instance whose database has been built.
# Usage: scripts/smoke.sh [base_url]   (TOKEN=... if API_TOKENS is set)
set -euo pipefail
base="${1:-http://127.0.0.1:8810}"
auth=()
[ -n "${TOKEN:-}" ] && auth=(-H "Authorization: Bearer ${TOKEN}")
fail=0
check() { # name path python-expression-on-d
  local body
  if ! body="$(curl -fsS -m 30 "${auth[@]}" "$base$2")"; then
    echo "FAIL $1 (HTTP error)"; fail=1; return
  fi
  if python3 -c "import json,sys; d=json.loads(sys.argv[1]); sys.exit(0 if ($3) else 1)" "$body"; then
    echo "ok   $1"
  else
    echo "FAIL $1: ${body:0:300}"; fail=1
  fi
}
check healthz        /api/healthz                                        "d['ready']"
check status         /api/v1/status                                      "d['data']['counts']['titles']>10000000"
check search         "/api/v1/titles/search?q=shawshank+redemption"      "d['items'][0]['id']=='tt0111161'"
check search-zh      "/api/v1/titles/search?q=%E9%9C%B8%E7%8E%8B%E5%88%AB%E5%A7%AC" "d['items'][0]['id']=='tt0106332'"
check search-year    "/api/v1/titles/search?q=dune&year=1984&type=movie" "d['items'][0]['id']=='tt0087182'"
check title          /api/v1/titles/tt0111161                            "d['rating']['value']>9 and len(d['principals'])>3"
check series         /api/v1/titles/tt0903747                            "d['total_episodes']>=62 and len(d['seasons'])>=5"
check episodes       "/api/v1/titles/tt0903747/episodes?season=1"        "d['total']==7"
check people         "/api/v1/people/search?q=morgan+freeman"            "d['items'][0]['id']=='nm0000151'"
check person         /api/v1/people/nm0000151                            "len(d['credits'])>10"
exit $fail
