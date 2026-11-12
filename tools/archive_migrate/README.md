# archive_migrate — 按订阅分片的条目归档导出与对账工具

面向 Miniflux 库（PostgreSQL）十万级历史条目迁移：按 `feed_id`（订阅）分片导出
结构化 JSONL 归档，单次扫描完成写片与校验，产出可对账清单，支持断点续跑。

## 设计取舍

| 方案 | 问题 | 本工具 |
|---|---|---|
| 逐条流式写 + 二次扫描校验 | 省内存但要全量读两遍 | 不采用 |
| 分片全量缓冲再校验 | 峰值内存随最大片线性膨胀（实测 12 万行、2.4 万/片时 329 MiB） | 不采用 |
| **有序流式 + 单遍写片校验 + 状态外置 SQLite** | — | ✅ 实测同数据峰值 **20.4 MiB**，与分片大小无关 |

关键机制：

1. **单遍流式**：数据源按 `(feed_id, id)` 有序（PG 端 `ORDER BY`，服务端命名游标
   取数），一个 feed 一个分片，切 feed 即收尾。峰值内存约等于单行。
2. **状态外置**：已见 hash、分片清单、问题台账全部落在输出目录 `state.db`，
   进程不持有大 map。
3. **断点续跑**：分片先写 `tmp/*.tmp`，校验通过后原子 `rename` 并在 `state.db`
   记 `status=done`；重跑时 done 的片跳过、不重做。崩溃残留（tmp、未登记分片）
   启动时自动清理；未完成片重处理前先幂等清除其旧台账。
4. **缺陷与完整性分离**：源数据缺陷（缺正文/重复 hash/孤儿）只记账不阻断；
   归档自身完整性问题（sha256/行数/字节/交叉核对/总数）才是 verify 硬失败。

## 输出目录结构

```
<output>/
├── shards/                      # 正常订阅分片
│   ├── shard_feed_1.jsonl
│   └── shard_feed_1.manifest.json   # 行数/字节/sha256/首尾 entry_id
├── orphans/                     # 订阅已删但条目仍在（feeds 清单中无此 feed_id）
├── unknown/                     # 未提供 feeds 清单，无法定性（补上清单重跑即可）
├── tmp/                         # 写片中的临时文件，正常结束后为空
├── state.db                     # SQLite：feeds/seen_hash/issues/meta
└── verify_report.json           # verify 产出的独立校验报告
```

分片每行一个条目（JSONL），字段对齐 Miniflux `entries` 表：
`id,user_id,feed_id,hash,published_at,title,url,comments_url,author,share_code,
content,status,starred,reading_time,created_at,changed_at,tags,language`。

## 缺陷台账（issues 表 / 报告 issue_counts）

| kind | 含义 |
|---|---|
| `MISSING_CONTENT` | 正文为空或纯空白 |
| `EMPTY_HASH` | hash 为空（无法参与去重） |
| `MISSING_TITLE` / `MISSING_URL` | 标题/URL 缺失 |
| `DUP_HASH_IN_FEED` | 同 feed 内 hash 重复（等价违反 `unique(feed_id,hash)`） |
| `DUP_HASH_CROSS_FEED` | 同一 hash 出现在多个订阅 |
| `ORPHAN_ENTRY` | feed_id 不在在册订阅清单，或行缺 feed_id |

## 用法

读取不可信导出文件时用隔离模式：`python3 -I archive_migrate.py ...`
（`-I` 使 cwd 不入 `sys.path`，防止同目录恶意 `json.py` 等被导入）。

### 1. 从 PostgreSQL 直连（推荐，需 psycopg2）

```bash
python3 -I archive_migrate.py export \
  --source pg-psycopg2 \
  --dsn "postgres://user:pass@host:5432/miniflux?sslmode=require" \
  --output /mnt/archive/out
```

服务端命名游标 `itersize=2000` 流式取数，结果集不进客户端内存。
也可用环境变量 `DATABASE_URL` 代替 `--dsn`。

### 2. 无 Python 驱动时（经 psql 管道）

```bash
python3 -I archive_migrate.py export --source pg-psql \
  --dsn "host=… user=… dbname=…" --output /mnt/archive/out
```

内部执行 `COPY (SELECT row_to_json(t) FROM (<sql>) t) TO STDOUT`，
要求机器上有 `psql` 且凭据可用（~/.pgpass 或 PGPASSWORD）。

### 3. 离线 JSONL（先从库里导出，再搬到处理机）

```bash
# 库机上（psql），两条命令都必须带 ORDER BY：
psql "$DSN" -v ON_ERROR_STOP=1 -c \
 "COPY (SELECT row_to_json(t) FROM (SELECT id,user_id,title,feed_url FROM feeds ORDER BY id) t) TO STDOUT" \
 > feeds.jsonl
psql "$DSN" -v ON_ERROR_STOP=1 -c \
 "COPY (SELECT row_to_json(t) FROM (SELECT id,user_id,feed_id,hash,published_at,title,url,
        comments_url,author,share_code,content,status,starred,reading_time,created_at,
        changed_at,tags,language FROM entries ORDER BY feed_id,id) t) TO STDOUT" \
 > entries.jsonl

# 处理机上：
python3 -I archive_migrate.py export --source jsonl \
  --entries entries.jsonl --feeds feeds.jsonl --output ./out
```

### 4. 独立校验与对账

```bash
python3 -I archive_migrate.py verify --output ./out                 # 硬失败退出码 1
python3 -I archive_migrate.py verify --output ./out --fail-on-defects  # 有缺陷台账也失败（CI 门禁）
python3 -I archive_migrate.py report --output ./out                 # 文本对账清单
python3 -I archive_migrate.py report --output ./out --json > recon.json
```

verify 不信任 `state.db`：重读每个分片重算 sha256/行数/字节，与 manifest 和
state.db 三方交叉核对，并做源端总数对账与跨片重复扫描。

### 断点续跑行为

- 中断后原样重跑同一命令即可；done 分片跳过（日志 `resumed_skipped`）。
- 只想重跑某片：从 `state.db` 的 `feeds` 表删掉该 feed_id 行（其 seen_hash/
  issues 行会在重处理前自动清理），重跑后仅该片重建，sha256 与首跑一致。

## 测试 / 演示

`make_demo_data.py` 生成带已知缺陷的合成数据（默认每订阅 3000 条）：

```bash
python3 -I make_demo_data.py --out ./demo-in --per-feed 24000
python3 -I archive_migrate.py export --source jsonl \
  --entries ./demo-in/entries.jsonl --feeds ./demo-in/feeds.jsonl --output ./demo-out
python3 -I archive_migrate.py verify  --output ./demo-out
python3 -I archive_migrate.py report  --output ./demo-out
```

仅依赖 Python 3.11+ 标准库；psycopg2 与 psql 均为可选数据源。
