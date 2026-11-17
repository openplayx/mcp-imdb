# mcp-imdb

[English](README.md) | 简体中文

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

把 IMDb 官方的[非商业数据集](https://developer.imdb.com/non-commercial-datasets/)封装成 HTTP API 和 MCP 服务。单个静态 Go 二进制（纯 Go 的 SQLite，不需要 cgo）。服务在本地保存一份数据副本，IMDb 发布新文件时自动刷新。

- **片名搜索**：可按主标题、原始标题或任意地区的别名（包括中文）搜索，按匹配程度、热度以及（如果提供）上映年份排序。适合把 `Dune.1984.1080p` 这样的文件名对应到 `tt0087182`。
- **条目详情**：类型、年份、片长、类别、评分和票数、导演、编剧、主要演员及角色、各地区别名；剧集的季信息；单集所属的剧集、季号和集号。
- **分集列表**：按剧集和季列出。
- **人物**：搜索、出生/去世年份、职业、代表作、主要参演作品。

本项目是 [OpenPlayX](https://github.com/openplayx/openplayx) 的一部分，也可以在任何 MCP 客户端中使用。

## 更新机制

IMDb 大约每天在 `https://datasets.imdbws.com/` 重新发布七个 gzip 压缩的 TSV 文件（合计压缩后约 2 GB）。更新器的流程：

1. 启动时以及之后每隔 `IMDB_CHECK_INTERVAL`（默认 6 小时，±5% 抖动），对每个文件发 `HEAD` 请求，把 `ETag` 和当前数据库构建时所用的版本比对；
2. 只要有文件不同（或还没有数据库），就等到最近 `IMDB_SETTLE`（默认 20 分钟）内没有文件再变化（IMDb 是逐个上传文件的），然后下载文件，在旧数据库旁边构建新的 SQLite 数据库，再原子替换：替换那一刻之前，查询一直由旧数据应答；
3. 失败时保留旧数据，把错误记录在 `/api/v1/status` 里，30 分钟后重试。已下载完的文件在重试时复用；如果文件格式变了（TSV 列不同），构建会明确失败，而不是生成一个空数据库。

`POST /api/v1/refresh` 立即执行一次检查；`mcp-imdb update` 检查一次后退出。

| 文件 | 导入内容 |
|---|---|
| `title.basics` | 所有条目，包括单集（必需） |
| `title.ratings` | 评分和票数（必需） |
| `title.episode` | 剧集 → 单集（必需） |
| `title.crew` | 非单集条目的导演和编剧 |
| `title.akas` | 非单集条目的别名，按条目去重 |
| `title.principals` | 非单集条目的主要演职员 |
| `name.basics` | 人物 |

可选文件可以用 `IMDB_SKIP=akas,principals,crew,names` 跳过。

完整数据集的资源占用（2026-11-15 在 Apple M 系列 Mac 上实测：1300 万条目、1000 万单集、510 万别名、2540 万主要演职员记录、1580 万人物）：导入约 5 分钟，只用一个 CPU 核心（其中一半时间在建索引），内存峰值约 3 GB，数据库约 5 GB。重建期间下载文件（约 2 GB）和新数据库与旧数据库同时存在，需保留约 15 GB 空闲磁盘。查询耗时从几毫秒到约 50 毫秒。

## 快速开始

依赖：Go 1.26+（或 Docker），导入需要约 15 GB 空闲磁盘和约 3 GB 内存。

```bash
go run ./src/cmd/mcp-imdb                          # :8810，数据在 ./data，不鉴权
API_TOKENS=s3cret DATA_DIR=/var/lib/mcp-imdb go run ./src/cmd/mcp-imdb
go run ./src/cmd/mcp-imdb update                   # 一次性下载并导入
```

第一次导入完成前，`/api/healthz` 返回 `"ready": false`，查询会以 `not_ready` 失败（HTTP 503，带 `Retry-After`）。

Docker：

```bash
cp deploy/env.example deploy/.env              # 设置 API_TOKENS
docker compose -f deploy/compose.yaml up -d --build
TOKEN=s3cret scripts/smoke.sh http://127.0.0.1:8810   # 就绪后执行
```

| 变量 | 默认值 | 含义 |
|---|---|---|
| `LISTEN` | `:8810` | 监听地址（`-listen` 参数优先） |
| `API_TOKENS` | 空 | 逗号分隔的 Bearer token；为空则不鉴权 |
| `DATA_DIR` | `./data`（Docker 中为 `/data`） | 数据库和下载文件 |
| `IMDB_UPDATE` | `true` | 设为 `false` 关闭后台更新器 |
| `IMDB_CHECK_INTERVAL` | `6h` | 两次检查的间隔 |
| `IMDB_SETTLE` | `20m` | 有文件在此时间内变化过就继续等待；`0` 表示不等待 |
| `IMDB_SKIP` | 空 | 要跳过的可选数据集 |
| `IMDB_KEEP_DOWNLOADS` | `false` | 导入后保留 `.tsv.gz` 文件 |
| `IMDB_BASE_URL` | IMDb | 数据集地址（镜像、测试） |
| `LOG_LEVEL` | `info` | `debug`、`info`、`warn`、`error` |

## HTTP API

设置了 token 后，除 `/api/healthz` 以外的所有路由都需要 `Authorization: Bearer <token>`。

| 路由 | 用途 |
|---|---|
| `GET /api/healthz` | 存活检查、版本、`ready`、`built_at` |
| `GET /api/v1/status` | 文件版本、行数、更新器状态（上次/下次检查、上次错误） |
| `POST /api/v1/refresh` | 立即检查新文件 |
| `GET /api/v1/titles/search?q=&year=&type=&limit=10&adult=false` | 片名搜索（`limit` ≤ 50） |
| `GET /api/v1/titles/{tt…}?max_akas=100` | 详情 |
| `GET /api/v1/titles/{tt…}/episodes?season=&start=0&limit=200` | 分集（`limit` ≤ 1000） |
| `GET /api/v1/people/search?q=&limit=10` | 人物搜索 |
| `GET /api/v1/people/{nm…}?limit=50` | 人物及主要参演作品 |
| `POST /api/mcp` | MCP（见下文） |

`type` 可取 `any`、`movie`（movie、tvMovie、video）、`series`（tvSeries、tvMiniSeries）、`short`（short、tvShort），或某个 IMDb 条目类型。单集不能按名称搜索：先找到剧集，再列出它的分集。`q` 传 `tt…` id 时直接返回该条目。

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

没有票数的条目 `rating` 为 `null`。`matched` 说明匹配上的是哪种标题；`exact` 表示规范化后的完整标题与查询相同（忽略大小写、重音符号、标点和开头的 "The/A/An"）。错误格式：

```json
{"error": {"status": 404, "code": "not_found", "message": "no such title in the IMDb dataset", "retryable": false}}
```

| 错误码 | HTTP | 含义 |
|---|---|---|
| `invalid_request` | 400 | 参数错误；`detail.field` 指出字段 |
| `unauthorized` | 401 | 缺少 token 或 token 错误 |
| `not_found` | 404 | 没有这个条目或人物 |
| `not_ready` | 503 | 第一次导入仍在进行；`retryable: true` |

## MCP

工具（全部只读）：`search_titles`、`get_title`、`get_episodes`、`search_people`、`get_person`、`dataset_status`。结果以 `structuredContent` 返回，同时附带相同内容的 JSON 文本；失败时返回 `isError: true` 的工具结果，内容是上面的错误对象。

**Streamable HTTP**：`POST /api/mcp`，单条 JSON-RPC 2.0 消息，以 `application/json` 应答（不用 SSE；`GET` → 405），无状态，支持协议版本 2025-11-25、2025-06-18 和 2025-03-26。与 HTTP API 使用同一个 Bearer token；来自外部 `Origin` 的请求会被拒绝。

**stdio**，用于桌面 MCP 客户端（同样会运行更新器，所以第一次启动要先下载并导入，工具才有数据）：

```json
{
  "mcpServers": {
    "imdb": { "command": "/usr/local/bin/mcp-imdb", "args": ["stdio"], "env": {"DATA_DIR": "/var/lib/mcp-imdb"} }
  }
}
```

## 开发

```bash
go test -race ./...            # 离线：由模拟的 IMDb 提供测试数据集
scripts/smoke.sh               # 在线：对加载了真实数据的运行实例做检查
scripts/build.sh linux/amd64   # 静态二进制输出到 dist/
```

```text
src/cmd/mcp-imdb/   入口：HTTP 服务、`stdio`、`update`、`version`
src/internal/imdb/  TSV 导入 SQLite、查询、数据库切换、更新器
src/internal/mcp/   MCP 工具和 JSON-RPC 处理（与传输方式无关）
src/internal/api/   HTTP 路由、鉴权、/api/mcp
deploy/             compose 文件和环境变量模板
scripts/            构建和在线冒烟测试
```

## 参与贡献

欢迎提 issue 和 PR，见 [CONTRIBUTING.md](CONTRIBUTING.md)（英文）。安全问题请按 [SECURITY.md](SECURITY.md)（英文）私下报告。

## 数据许可与声明

本项目是非官方项目，与 IMDb 没有关联，也未获 IMDb 认可。数据集版权归 IMDb.com, Inc. 所有，仅授权用于**个人和非商业用途**，见 IMDb 的[条款](https://developer.imdb.com/non-commercial-datasets/)。本项目不包含任何 IMDb 数据，每个实例自行下载。不要将数据或本服务的公开实例用于商业目的。

## 许可证

代码采用 [MIT](LICENSE) 许可证。MIT 只覆盖本项目的代码，不覆盖 IMDb 的数据。
