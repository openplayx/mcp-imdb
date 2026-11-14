# mcp-imdb

IMDb's official [non-commercial datasets](https://developer.imdb.com/non-commercial-datasets/)
as an HTTP API and an MCP server, in one static Go binary (pure-Go SQLite, no cgo).
The service keeps its own local copy and refreshes it whenever IMDb publishes new files.

- **Title search** by primary, original or any regional alternative title (Chinese included),
  ranked by match quality, popularity and, when given, the release year: the right tool for
  turning a file name like `Dune.1984.1080p` into `tt0087182`.
- **Title details**: type, years, runtime, genres, rating and votes, directors, writers,
  principal cast with characters, alternative titles by region; seasons for series; series,
  season and episode number for episodes.
- **Episode lists** per series and season.
- **People**: search, birth/death years, professions, known-for titles, principal credits.

## How updates work

IMDb republishes seven gzip TSV files at `https://datasets.imdbws.com/` about once a day
(~2 GB compressed in total). The updater:

1. on start and then every `IMDB_CHECK_INTERVAL` (default 6 h, ±5 % jitter) sends `HEAD`
   for each file and compares its `ETag` with the versions the current database was built from;
2. if any differs (or there is no database yet), waits until no file changed in the last
   `IMDB_SETTLE` (default 20 min; IMDb uploads the files one after another), then downloads
   the files, builds a new SQLite database next to the old one, and swaps it in atomically:
   queries keep being answered from the old data until the moment of the swap;
3. on failure keeps the old data, records the error in `/api/v1/status` and retries after
   30 min. Finished downloads are reused by the retry; a changed file format (different TSV
   columns) fails the build loudly instead of producing an empty database.

`POST /api/v1/refresh` runs a check immediately; `mcp-imdb update` does one check and exits.

| File | Imported |
|---|---|
| `title.basics` | every title, episodes included (required) |
| `title.ratings` | rating and votes (required) |
| `title.episode` | series → episodes (required) |
| `title.crew` | directors and writers of non-episode titles |
| `title.akas` | alternative titles of non-episode titles, deduplicated per title |
| `title.principals` | principal cast/crew of non-episode titles |
| `name.basics` | people |

The optional ones can be left out with `IMDB_SKIP=akas,principals,crew,names`.

Resources for the full set (Apple M-series, measured 2026-11): import ≈ 25 min of one CPU
core and up to ~3 GB RAM, database ≈ 10 GB. During a rebuild the downloads (~2 GB) and the
new database exist next to the old one, so plan for ~25 GB of free disk.

## Run

```bash
go run ./cmd/mcp-imdb                          # :8810, data in ./data, no auth
API_TOKENS=s3cret DATA_DIR=/var/lib/mcp-imdb go run ./cmd/mcp-imdb
go run ./cmd/mcp-imdb update                   # one-shot download + import
```

Until the first import finishes, `/api/healthz` reports `"ready": false` and queries fail
with `not_ready` (HTTP 503, `Retry-After`).

Docker:

```bash
cp deploy/env.example deploy/.env              # set API_TOKENS
docker compose -f deploy/compose.yaml up -d --build
TOKEN=s3cret scripts/smoke.sh http://127.0.0.1:8810   # once ready
```

211 test host: `scripts/deploy-211.sh` (Compose project `openplayx-imdb`, port 18820,
token in `~/openplayx-imdb/deploy/app-token.txt`).

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN` | `:8810` | listen address (`-listen` flag wins) |
| `API_TOKENS` | empty | comma-separated bearer tokens; empty disables auth |
| `DATA_DIR` | `./data` (`/data` in Docker) | database and downloads |
| `IMDB_UPDATE` | `true` | `false` disables the background updater |
| `IMDB_CHECK_INTERVAL` | `6h` | time between checks |
| `IMDB_SETTLE` | `20m` | wait while a file changed less than this ago; `0` disables |
| `IMDB_SKIP` | empty | optional datasets to leave out |
| `IMDB_KEEP_DOWNLOADS` | `false` | keep the `.tsv.gz` files after an import |
| `IMDB_BASE_URL` | IMDb | dataset location (mirrors, tests) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## HTTP API

With tokens set, every route except `/api/healthz` needs `Authorization: Bearer <token>`.

| Route | Purpose |
|---|---|
| `GET /api/healthz` | liveness, version, `ready`, `built_at` |
| `GET /api/v1/status` | file versions, row counts, updater state (last/next check, last error) |
| `POST /api/v1/refresh` | check for new files now |
| `GET /api/v1/titles/search?q=&year=&type=&limit=10&adult=false` | title search (`limit` ≤ 50) |
| `GET /api/v1/titles/{tt…}?max_akas=100` | details |
| `GET /api/v1/titles/{tt…}/episodes?season=&start=0&limit=200` | episodes (`limit` ≤ 1000) |
| `GET /api/v1/people/search?q=&limit=10` | people search |
| `GET /api/v1/people/{nm…}?limit=50` | person and principal credits |
| `POST /api/mcp` | MCP (below) |

`type` is `any`, `movie` (movie, tvMovie, video), `series` (tvSeries, tvMiniSeries), `short`
(short, tvShort) or an IMDb title type. Episodes are not searchable by name: find the series,
then list its episodes. A `tt…` id as `q` returns that title.

```bash
curl -s 'http://127.0.0.1:8810/api/v1/titles/search?q=霸王别姬&limit=1'
```

```json
{
  "query": "霸王别姬",
  "items": [{
    "id": "tt0106332", "type": "movie", "title": "Farewell My Concubine",
    "original_title": "Ba wang bie ji", "year": 1993, "runtime_minutes": 171,
    "genres": ["Drama", "Music", "Romance"], "rating": {"value": 8.1, "votes": 93000},
    "url": "https://www.imdb.com/title/tt0106332/",
    "matched": {"title": "霸王别姬", "kind": "aka", "region": "CN", "exact": true},
    "score": 159.4
  }]
}
```

`rating` is `null` for titles without votes. `matched` tells which title form matched;
`exact` means the whole normalized title equals the query (case, accents, punctuation and a
leading "The/A/An" ignored). Errors:

```json
{"error": {"status": 404, "code": "not_found", "message": "no such title in the IMDb dataset", "retryable": false}}
```

| Code | HTTP | Meaning |
|---|---|---|
| `invalid_request` | 400 | bad parameter; `detail.field` names it |
| `unauthorized` | 401 | missing or wrong token |
| `not_found` | 404 | no such title or person |
| `not_ready` | 503 | first import still running; `retryable: true` |

## MCP

Tools (all read-only): `search_titles`, `get_title`, `get_episodes`, `search_people`,
`get_person`, `dataset_status`. Results are returned as `structuredContent` plus the same
JSON as text; failures are tool results with `isError: true` and the error object above.

**Streamable HTTP** at `POST /api/mcp`: single JSON-RPC 2.0 messages answered with
`application/json` (no SSE; `GET` → 405), stateless, protocol versions 2025-11-25,
2025-06-18 and 2025-03-26. Same bearer token as the HTTP API; requests with a foreign
`Origin` are refused.

**stdio** for desktop MCP clients (runs the updater too, so the first start downloads and
imports before tools return data):

```json
{
  "mcpServers": {
    "imdb": { "command": "/usr/local/bin/mcp-imdb", "args": ["stdio"], "env": {"DATA_DIR": "/var/lib/mcp-imdb"} }
  }
}
```

## Develop

```bash
go test -race ./...            # offline: fixture datasets served by a fake IMDb
scripts/smoke.sh               # live: a running instance with the real data
scripts/build.sh linux/amd64   # static binaries in dist/
```

```text
cmd/mcp-imdb/     entry point: HTTP server, `stdio`, `update`, `version`
internal/imdb/    TSV import into SQLite, queries, store swap, updater
internal/mcp/     MCP tools and JSON-RPC handling (transport-agnostic)
internal/api/     HTTP routes, auth, /api/mcp
deploy/           compose file and env template
scripts/          build, live smoke test, 211 deploy
```

## Data licence

The datasets are © IMDb.com, Inc. and licensed for **personal and non-commercial use**
only; see IMDb's [terms](https://developer.imdb.com/non-commercial-datasets/). This project
contains no IMDb data; each instance downloads it itself.
