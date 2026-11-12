#!/usr/bin/env python3
"""archive_migrate.py — 按订阅分片的条目归档导出与对账工具（Miniflux 库迁移）.

设计要点
========
1. 流式扫描，单次全量扫描完成「写分片 + 片内校验」。条目按 feed_id 有序到达，
   一个 feed 一个 JSONL 分片，切换 feed 时收尾（sha256/行数落盘、原子改名）。
   峰值内存约等于单行大小，与分片大小无关。
2. 全局状态（已见 hash、feed 清单、分片清单、问题台账）全部落在输出目录的
   state.db (SQLite)，进程不持有大 map，10 万+ 条目内存恒定。
3. 断点续跑：分片先写 tmp/，校验通过后原子 rename 到 shards/ 并在 state.db 记
   status=done；重跑时 done 的分片直接跳过，其 hash 已在 seen_hash 表里，
   跨片重复检测不会因续跑而失效。
4. 源数据缺陷（缺正文/重复 hash/孤儿条目）只记账不阻断；归档自身完整性问题
   （校验和不符、清单对不上）才是 verify 的硬失败。

数据源可插拔：
  --source pg-psycopg2  直连 PostgreSQL（需 psycopg2，服务端命名游标流式）
  --source pg-psql      通过 psql COPY ... TO STDOUT 管道取数（无需 Python 驱动）
  --source jsonl        离线 JSONL 导出（条目文件须按 feed_id 有序；可配 feeds 文件）

用法见 --help。读取不可信数据时请以 `python3 -I` 运行。
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime as _dt
import hashlib
import json
import os
import re
import sqlite3
import sys
import tempfile
import time

TOOL_VERSION = "1.0.0"

# entries 表的规范字段（对齐 Miniflux internal/storage/entry_query_builder.go）
ENTRY_FIELDS = [
    "id", "user_id", "feed_id", "hash", "published_at", "title", "url",
    "comments_url", "author", "share_code", "content", "status", "starred",
    "reading_time", "created_at", "changed_at", "tags", "language",
]

ISSUE_KINDS = (
    "MISSING_CONTENT", "EMPTY_HASH", "DUP_HASH_IN_FEED", "DUP_HASH_CROSS_FEED",
    "ORPHAN_ENTRY", "MISSING_TITLE", "MISSING_URL",
)


# --------------------------------------------------------------------------
# 工具函数
# --------------------------------------------------------------------------
def utcnow_iso() -> str:
    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def json_default(obj):
    if isinstance(obj, (_dt.datetime, _dt.date)):
        return obj.isoformat()
    if isinstance(obj, (bytes, bytearray)):
        return obj.decode("utf-8", "replace")
    raise TypeError(f"不可序列化的类型: {type(obj)!r}")


def canonical_entry(raw: dict) -> dict:
    """把来源行收敛成规范结构，缺失键补 None / 空数组，保证分片 schema 稳定。"""
    out = {}
    for key in ENTRY_FIELDS:
        out[key] = raw.get(key)
    tags = out.get("tags")
    if tags is None:
        out["tags"] = []
    elif not isinstance(tags, list):
        # PG text[] 经某些通道可能给成 "{a,b}" 字符串
        tags = tags.strip("{}")
        out["tags"] = [t for t in tags.split(",") if t] if tags else []
    for nullable_text in ("content", "title", "url", "hash", "author",
                          "comments_url", "share_code", "language", "status"):
        if out[nullable_text] is not None and not isinstance(out[nullable_text], str):
            out[nullable_text] = str(out[nullable_text])
    return out


# --------------------------------------------------------------------------
# state.db
# --------------------------------------------------------------------------
SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS feeds (
    feed_id     INTEGER PRIMARY KEY,
    user_id     INTEGER,
    title       TEXT,
    feed_url    TEXT,
    shard       TEXT,
    entry_count INTEGER NOT NULL DEFAULT 0,
    byte_size   INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT,
    status      TEXT NOT NULL,            -- done | orphan | unknown
    started_at  TEXT,
    finished_at TEXT
);
CREATE TABLE IF NOT EXISTS seen_hash (
    hash     TEXT NOT NULL,
    feed_id  INTEGER NOT NULL,
    entry_id INTEGER NOT NULL,
    PRIMARY KEY (hash, feed_id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS issues (
    issue_id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind     TEXT NOT NULL,
    feed_id  INTEGER,
    entry_id INTEGER,
    hash     TEXT,
    detail   TEXT,
    at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS issues_kind_idx ON issues(kind);
"""


class State:
    def __init__(self, path: str):
        self.path = path
        self.conn = sqlite3.connect(path)
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA synchronous=NORMAL")
        self.conn.executescript(SCHEMA)
        self.conn.commit()

    def meta_get(self, key: str) -> str | None:
        row = self.conn.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return row[0] if row else None

    def meta_set(self, key: str, value: str) -> None:
        self.conn.execute(
            "INSERT INTO meta(key,value) VALUES(?,?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (key, value))

    def done_feed_ids(self) -> set[int]:
        return {r[0] for r in self.conn.execute(
            "SELECT feed_id FROM feeds WHERE status IN ('done','orphan','unknown')")}

    def log_issue(self, kind: str, feed_id, entry_id, hsh, detail: str) -> None:
        self.conn.execute(
            "INSERT INTO issues(kind,feed_id,entry_id,hash,detail,at) "
            "VALUES(?,?,?,?,?,?)",
            (kind, feed_id, entry_id, hsh, detail, utcnow_iso()))

    def close(self) -> None:
        self.conn.commit()
        self.conn.close()


# --------------------------------------------------------------------------
# 数据源
# --------------------------------------------------------------------------
@dataclasses.dataclass
class FeedMeta:
    feed_id: int
    user_id: int | None
    title: str | None
    feed_url: str | None


class EntrySource:
    """统一接口：feeds() 给出在册订阅；entries() 按 feed_id 有序产出 dict 行。"""

    # PG 源由 SQL ORDER BY 保证有序；文件源必须做写盘前预检
    needs_order_preflight = False

    def feeds(self) -> list[FeedMeta]:
        raise NotImplementedError

    def entries(self):
        raise NotImplementedError

    def total_entries(self) -> int | None:
        """用于对账的源端总数；拿不到就返回 None。"""
        return None


class JsonlSource(EntrySource):
    needs_order_preflight = True
    def __init__(self, entries_path: str, feeds_path: str | None):
        self.entries_path = entries_path
        self.feeds_path = feeds_path

    def feeds(self) -> list[FeedMeta]:
        if not self.feeds_path:
            return []
        out = []
        with open(self.feeds_path, "r", encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                row = json.loads(line)
                out.append(FeedMeta(
                    int(row["id"]), row.get("user_id"),
                    row.get("title"), row.get("feed_url")))
        return out

    def entries(self):
        with open(self.entries_path, "r", encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if line:
                    yield json.loads(line)

    def total_entries(self) -> int | None:
        n = 0
        with open(self.entries_path, "rb") as fh:
            for _ in fh:
                n += 1
        return n


FEEDS_SQL = (
    "SELECT id, user_id, title, feed_url FROM feeds ORDER BY id"
)
ENTRIES_SQL = (
    "SELECT id, user_id, feed_id, hash, published_at, title, url, comments_url, "
    "author, share_code, content, status, starred, reading_time, created_at, "
    "changed_at, tags, language FROM entries ORDER BY feed_id, id"
)


class PgPsycopg2Source(EntrySource):
    def __init__(self, dsn: str):
        try:
            import psycopg2  # noqa: F401  (运行时才需要)
        except ImportError as exc:
            raise SystemExit("找不到 psycopg2，请改用 --source pg-psql 或安装驱动") from exc
        self.dsn = dsn

    def _connect(self):
        import psycopg2
        return psycopg2.connect(self.dsn)

    def feeds(self) -> list[FeedMeta]:
        conn = self._connect()
        try:
            cur = conn.cursor()
            cur.execute(FEEDS_SQL)
            return [FeedMeta(int(r[0]), r[1], r[2], r[3]) for r in cur.fetchall()]
        finally:
            conn.close()

    def entries(self):
        conn = self._connect()
        # 命名游标 = 服务端游标，结果集不一次性进客户端内存
        cur = conn.cursor(name="archive_export_stream", withhold=True)
        cur.itersize = 2000
        cur.execute(ENTRIES_SQL)
        cols = [d[0] for d in cur.description]
        try:
            for row in cur:
                yield dict(zip(cols, row))
        finally:
            cur.close()
            conn.close()

    def total_entries(self) -> int | None:
        conn = self._connect()
        try:
            cur = conn.cursor()
            cur.execute("SELECT count(*) FROM entries")
            return int(cur.fetchone()[0])
        finally:
            conn.close()


class PgPsqlSource(EntrySource):
    """无 Python 驱动时走 psql 子进程：COPY (row_to_json(t)) TO STDOUT。"""

    def __init__(self, dsn: str):
        self.dsn = dsn

    def _copy_json(self, sql: str):
        import subprocess
        copy_sql = f"COPY (SELECT row_to_json(t) FROM ({sql}) t) TO STDOUT"
        proc = subprocess.Popen(
            ["psql", self.dsn, "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "-c", copy_sql],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        assert proc.stdout is not None
        for line in proc.stdout:
            line = line.strip()
            if line:
                yield json.loads(line)
        stderr = proc.stderr.read()
        rc = proc.wait()
        if rc != 0:
            raise SystemExit(f"psql 取数失败 (exit={rc}):\n{stderr}")

    def feeds(self) -> list[FeedMeta]:
        out = []
        for row in self._copy_json(FEEDS_SQL):
            out.append(FeedMeta(int(row["id"]), row.get("user_id"),
                                row.get("title"), row.get("feed_url")))
        return out

    def entries(self):
        yield from self._copy_json(ENTRIES_SQL)

    def total_entries(self) -> int | None:
        for row in self._copy_json("SELECT count(*) AS count FROM entries"):
            return int(row["count"])
        return None


def build_source(args) -> EntrySource:
    if args.source == "jsonl":
        if not args.entries:
            raise SystemExit("jsonl 数据源需要 --entries 路径")
        return JsonlSource(args.entries, args.feeds)
    dsn = args.dsn or os.environ.get("DATABASE_URL")
    if not dsn:
        raise SystemExit("PG 数据源需要 --dsn 或 DATABASE_URL 环境变量")
    if args.source == "pg-psycopg2":
        return PgPsycopg2Source(dsn)
    if args.source == "pg-psql":
        return PgPsqlSource(dsn)
    raise SystemExit(f"未知数据源: {args.source}")


# --------------------------------------------------------------------------
# 导出
# --------------------------------------------------------------------------
class ShardWriter:
    """单个 feed 分片的增量写出器：恒定内存，顺带算 sha256/字节数/行数。"""

    def __init__(self, tmp_dir: str, feed_id: int):
        self.feed_id = feed_id
        self.tmp_path = os.path.join(tmp_dir, f"shard_feed_{feed_id}.jsonl.tmp")
        self.fh = open(self.tmp_path, "w", encoding="utf-8")
        self.hasher = hashlib.sha256()
        self.entry_count = 0
        self.byte_size = 0
        self.first_entry_id = None
        self.last_entry_id = None

    def write(self, entry: dict) -> None:
        line = json.dumps(entry, ensure_ascii=False, separators=(",", ":"),
                          default=json_default) + "\n"
        encoded = line.encode("utf-8")
        self.fh.write(line)
        self.hasher.update(encoded)
        self.byte_size += len(encoded)
        self.entry_count += 1
        eid = entry.get("id")
        if self.first_entry_id is None:
            self.first_entry_id = eid
        self.last_entry_id = eid

    def close(self) -> None:
        self.fh.close()


def validate_entry(entry: dict, known_feed_ids: set[int], state: State) -> list[str]:
    """片内/单行校验。返回本条命中的问题 kind 列表；全部落 issues 台账。"""
    problems: list[str] = []
    feed_id = entry.get("feed_id")
    entry_id = entry.get("id")
    hsh = entry.get("hash") or ""

    content = entry.get("content")
    if content is None or not str(content).strip():
        problems.append("MISSING_CONTENT")
    if not hsh:
        problems.append("EMPTY_HASH")
    if not (entry.get("title") or "").strip():
        problems.append("MISSING_TITLE")
    if not (entry.get("url") or "").strip():
        problems.append("MISSING_URL")
    if known_feed_ids and feed_id not in known_feed_ids:
        problems.append("ORPHAN_ENTRY")

    if hsh:
        # 同一 feed 内重复（等价于违反 unique(feed_id, hash)）
        cur = state.conn.execute(
            "INSERT OR IGNORE INTO seen_hash(hash,feed_id,entry_id) VALUES(?,?,?)",
            (hsh, feed_id, entry_id))
        if cur.rowcount == 0:
            problems.append("DUP_HASH_IN_FEED")
        else:
            # 插入成功：若该 hash 已存在于别的 feed，即跨订阅重复
            other = state.conn.execute(
                "SELECT feed_id, entry_id FROM seen_hash WHERE hash=? AND feed_id<>?",
                (hsh, feed_id)).fetchall()
            if other:
                problems.append("DUP_HASH_CROSS_FEED")
                state.log_issue("DUP_HASH_CROSS_FEED", feed_id, entry_id, hsh,
                                "同一 hash 还见于 feed_id=" +
                                ",".join(f"{f}(entry={e})" for f, e in other))

    for kind in problems:
        if kind == "DUP_HASH_CROSS_FEED":
            continue  # 上面已带 detail 记录，避免重复
        state.log_issue(kind, feed_id, entry_id, hsh, "")
    return problems


def cmd_export(args) -> int:
    out = args.output
    shards_dir = os.path.join(out, "shards")
    orphan_dir = os.path.join(out, "orphans")
    unknown_dir = os.path.join(out, "unknown")
    tmp_dir = os.path.join(out, "tmp")
    for d in (shards_dir, orphan_dir, unknown_dir, tmp_dir):
        os.makedirs(d, exist_ok=True)

    state = State(os.path.join(out, "state.db"))
    source = build_source(args)

    known_feeds = source.feeds()
    known_feed_ids = {f.feed_id for f in known_feeds}
    state.meta_set("tool_version", TOOL_VERSION)
    state.meta_set("source_kind", args.source)
    state.meta_set("started_at", utcnow_iso())
    if args.source == "jsonl":
        state.meta_set("source_entries", os.path.abspath(args.entries))
        if args.feeds:
            state.meta_set("source_feeds", os.path.abspath(args.feeds))
    for feed in known_feeds:
        state.conn.execute(
            "INSERT OR IGNORE INTO feeds(feed_id,user_id,title,feed_url,status,started_at) "
            "VALUES(?,?,?,?, 'pending', ?)",
            (feed.feed_id, feed.user_id, feed.title, feed.feed_url, utcnow_iso()))
    state.conn.commit()

    source_total = source.total_entries()
    if source_total is not None:
        state.meta_set("source_total", str(source_total))

    done = state.done_feed_ids()
    if done:
        print(f"[resume] 已完成 {len(done)} 个分片，跳过", file=sys.stderr)

    # 崩溃恢复清理：tmp 永远不可恢复；未登记 done 的分片残留一律删除重做
    for name in os.listdir(tmp_dir):
        os.remove(os.path.join(tmp_dir, name))
        print(f"[resume] 清理崩溃临时文件 tmp/{name}", file=sys.stderr)
    for base in (shards_dir, orphan_dir, unknown_dir):
        for name in os.listdir(base):
            m = re.match(r"shard_feed_(\d+)\.(jsonl|manifest\.json)$", name)
            if m and int(m.group(1)) not in done:
                os.remove(os.path.join(base, name))
                print(f"[resume] 清理未完成分片残留 {base}/{name}", file=sys.stderr)

    def reset_feed_state(feed_id: int) -> None:
        """重处理一个未完成片前，清掉它的旧台账，保证重跑结果幂等。"""
        state.conn.execute("DELETE FROM seen_hash WHERE feed_id=?", (feed_id,))
        state.conn.execute("DELETE FROM issues WHERE feed_id=?", (feed_id,))
        state.conn.commit()

    if getattr(source, "needs_order_preflight", False):
        print("[preflight] 写盘前校验 (feed_id, id) 有序性…", file=sys.stderr)
        prev_key = None
        for raw in source.entries():
            feed_id = raw.get("feed_id")
            if feed_id is None:
                continue
            key = (feed_id, raw.get("id") or 0)
            if prev_key is not None and key < prev_key:
                raise SystemExit(
                    "输入未按 (feed_id, id) 有序，无法单遍流式分片；"
                    "请在数据源 ORDER BY feed_id, id 后重跑（预检失败，未写出任何分片）")
            prev_key = key

    scanned = 0
    exported = 0
    skipped_rows = 0
    current_feed_id = None
    writer: ShardWriter | None = None
    t0 = time.monotonic()

    def classify(feed_id: int) -> str:
        """分片去向：done→shards/，orphan→orphans/，无清单可查→unknown/。"""
        if not known_feed_ids:
            return "unknown"
        return "done" if feed_id in known_feed_ids else "orphan"

    status_to_dir = {"done": shards_dir, "orphan": orphan_dir,
                     "unknown": unknown_dir}

    def finalize(w: ShardWriter, status: str) -> None:
        w.close()
        target_dir = status_to_dir[status]
        final_name = f"shard_feed_{w.feed_id}.jsonl"
        final_path = os.path.join(target_dir, final_name)
        os.replace(w.tmp_path, final_path)  # 原子改名
        digest = w.hasher.hexdigest()
        manifest = {
            "shard": final_name,
            "feed_id": w.feed_id,
            "entry_count": w.entry_count,
            "byte_size": w.byte_size,
            "sha256": digest,
            "first_entry_id": w.first_entry_id,
            "last_entry_id": w.last_entry_id,
            "exported_at": utcnow_iso(),
            "tool_version": TOOL_VERSION,
            "status": status,
            "orphan": status == "orphan",
        }
        manifest_path = os.path.join(target_dir, f"shard_feed_{w.feed_id}.manifest.json")
        with open(manifest_path, "w", encoding="utf-8") as mf:
            json.dump(manifest, mf, ensure_ascii=False, indent=2)
        state.conn.execute(
            "INSERT INTO feeds(feed_id,shard,entry_count,byte_size,sha256,status,started_at,finished_at) "
            "VALUES(?,?,?,?,?,?,'',?) ON CONFLICT(feed_id) DO UPDATE SET "
            "shard=excluded.shard, entry_count=excluded.entry_count, "
            "byte_size=excluded.byte_size, sha256=excluded.sha256, "
            "status=excluded.status, finished_at=excluded.finished_at",
            (w.feed_id, os.path.join(os.path.basename(target_dir), final_name),
             w.entry_count, w.byte_size, digest, status, utcnow_iso()))
        state.conn.commit()
        print(f"[shard] feed_id={w.feed_id} entries={w.entry_count} "
              f"bytes={w.byte_size} sha256={digest[:12]}… {status.upper()}",
              file=sys.stderr)

    def ordered_rows():
        """产出前先校验 (feed_id, id) 非递减：乱序在首个分片落盘前即报错。"""
        prev_key = None
        for raw in source.entries():
            entry = canonical_entry(raw)
            feed_id = entry.get("feed_id")
            if feed_id is None:
                yield entry  # 主循环记账后跳过
                continue
            key = (feed_id, entry.get("id") or 0)
            if prev_key is not None and key < prev_key:
                raise SystemExit(
                    "输入未按 (feed_id, id) 有序，无法单遍流式分片；"
                    "请在数据源 ORDER BY feed_id, id 后重跑（本次未写出任何分片）")
            prev_key = key
            yield entry

    try:
        for entry in ordered_rows():
            feed_id = entry.get("feed_id")
            entry_id = entry.get("id")
            if feed_id is None:
                state.log_issue("ORPHAN_ENTRY", None, entry_id,
                                entry.get("hash"), "行缺少 feed_id，无法分片")
                scanned += 1
                continue

            scanned += 1
            if feed_id in done:
                # 续跑时已完成片的行不再重写，但其出现仍计入 scanned 对账
                skipped_rows += 1
                if scanned % 10000 == 0:
                    print(f"[progress] scanned={scanned} exported={exported} "
                          f"elapsed={time.monotonic()-t0:.0f}s", file=sys.stderr)
                continue

            if current_feed_id != feed_id:
                if writer is not None:
                    finalize(writer, classify(writer.feed_id))
                reset_feed_state(feed_id)
                writer = ShardWriter(tmp_dir, feed_id)
                current_feed_id = feed_id

            assert writer is not None
            validate_entry(entry, known_feed_ids, state)
            writer.write(entry)
            exported += 1

            if scanned % 10000 == 0:
                state.conn.commit()
                print(f"[progress] scanned={scanned} exported={exported} "
                      f"elapsed={time.monotonic()-t0:.0f}s", file=sys.stderr)

        if writer is not None:
            finalize(writer, classify(writer.feed_id))
    finally:
        state.meta_set("finished_at", utcnow_iso())
        state.meta_set("scanned", str(scanned))
        state.meta_set("exported", str(exported))
        state.meta_set("resumed_skipped_rows", str(skipped_rows))
        state.close()

    # 清掉残留 tmp（正常路径下应为空）
    for name in os.listdir(tmp_dir):
        print(f"[warn] 残留临时文件: {name}", file=sys.stderr)

    print(f"[done] scanned={scanned} exported={exported} "
          f"resumed_skipped={skipped_rows} elapsed={time.monotonic()-t0:.0f}s",
          file=sys.stderr)
    return 0


# --------------------------------------------------------------------------
# 校验（不信任 state.db，重读分片文件做独立对账）
# --------------------------------------------------------------------------
def iter_shard_files(out: str):
    for sub in ("shards", "orphans", "unknown"):
        base = os.path.join(out, sub)
        if not os.path.isdir(base):
            continue
        for name in sorted(os.listdir(base)):
            if name.endswith(".jsonl"):
                yield sub, os.path.join(base, name)


def cmd_verify(args) -> int:
    out = args.output
    state_path = os.path.join(out, "state.db")
    if not os.path.exists(state_path):
        print(f"FATAL: 找不到 {state_path}", file=sys.stderr)
        return 2
    state = State(state_path)
    hard_failures: list[str] = []
    total_entries = 0
    total_bytes = 0
    shard_records = []
    global_hash_locations: dict[str, list[tuple[int, int]]] = {}

    for sub, path in iter_shard_files(out):
        fname = os.path.basename(path)
        manifest_path = path[:-len(".jsonl")] + ".manifest.json"
        problems = []
        if not os.path.exists(manifest_path):
            hard_failures.append(f"{fname}: 缺少 manifest")
            manifest = {}
        else:
            with open(manifest_path, encoding="utf-8") as mf:
                manifest = json.load(mf)

        hasher = hashlib.sha256()
        line_count = 0
        in_shard_seen: dict[str, int] = {}
        feed_id = None
        with open(path, "rb") as fh:
            for lineno, raw_line in enumerate(fh, 1):
                hasher.update(raw_line)
                line_count += 1
                try:
                    entry = json.loads(raw_line.decode("utf-8"))
                except json.JSONDecodeError as exc:
                    problems.append(f"第 {lineno} 行 JSON 解析失败: {exc}")
                    continue
                feed_id = entry.get("feed_id")
                eid = entry.get("id")
                hsh = entry.get("hash") or ""
                if hsh:
                    if hsh in in_shard_seen:
                        problems.append(
                            f"片内 hash 重复: entry_id={eid} 与 "
                            f"entry_id={in_shard_seen[hsh]} hash={hsh[:16]}…")
                    else:
                        in_shard_seen[hsh] = eid
                    global_hash_locations.setdefault(hsh, []).append((feed_id, eid))

        digest = hasher.hexdigest()
        size = os.path.getsize(path)
        total_entries += line_count
        total_bytes += size

        if manifest.get("sha256") and manifest["sha256"] != digest:
            hard_failures.append(
                f"{fname}: sha256 不符 manifest={manifest['sha256'][:12]} actual={digest[:12]}")
        if manifest.get("entry_count") is not None and manifest["entry_count"] != line_count:
            hard_failures.append(
                f"{fname}: 行数不符 manifest={manifest['entry_count']} actual={line_count}")
        if manifest.get("byte_size") is not None and manifest["byte_size"] != size:
            hard_failures.append(
                f"{fname}: 字节数不符 manifest={manifest['byte_size']} actual={size}")

        # 与 state.db 交叉核对
        if feed_id is not None:
            row = state.conn.execute(
                "SELECT entry_count, sha256, status FROM feeds WHERE feed_id=?",
                (feed_id,)).fetchone()
            if row is None:
                hard_failures.append(f"{fname}: state.db 无 feed_id={feed_id} 记录")
            elif row[0] != line_count or row[1] != digest:
                hard_failures.append(f"{fname}: 与 state.db 记录不一致")
            expected_status = {"orphans": "orphan", "unknown": "unknown",
                               "shards": "done"}.get(sub)
            if row and expected_status and row[2] != expected_status:
                hard_failures.append(
                    f"{fname}: 状态应为 {expected_status}，实际 {row[2]}")

        shard_records.append({
            "shard": fname, "location": sub, "feed_id": feed_id,
            "entry_count": line_count, "byte_size": size, "sha256": digest,
            "integrity_problems": problems,
        })
        for p in problems:
            print(f"[warn] {fname}: {p}", file=sys.stderr)

    # 跨片重复 hash
    cross_feed_dups = {h: locs for h, locs in global_hash_locations.items()
                       if len({f for f, _ in locs}) > 1}

    # 源端总数对账
    src_total = state.meta_get("source_total")
    count_check = None
    if src_total is not None:
        count_check = (int(src_total), total_entries)
        if int(src_total) != total_entries:
            hard_failures.append(
                f"总数对不上: 源端={src_total} 归档分片合计={total_entries}")

    # 台账统计（源数据缺陷，属告警而非硬失败）
    issue_counts = dict(state.conn.execute(
        "SELECT kind, count(*) FROM issues GROUP BY kind").fetchall())
    db_dup_cross = state.conn.execute(
        "SELECT count(*) FROM issues WHERE kind='DUP_HASH_CROSS_FEED'").fetchone()[0]
    if len(cross_feed_dups) != db_dup_cross:
        hard_failures.append(
            f"跨片重复数台账不符: 实扫={len(cross_feed_dups)} 台账={db_dup_cross}")

    result = {
        "verified_at": utcnow_iso(),
        "tool_version": TOOL_VERSION,
        "shard_count": len(shard_records),
        "total_entries": total_entries,
        "total_bytes": total_bytes,
        "source_total_vs_archive": count_check,
        "cross_feed_duplicate_hashes": len(cross_feed_dups),
        "issue_ledger_counts": issue_counts,
        "hard_failures": hard_failures,
        "shards": shard_records,
    }
    report_path = os.path.join(out, "verify_report.json")
    with open(report_path, "w", encoding="utf-8") as fh:
        json.dump(result, fh, ensure_ascii=False, indent=2)
    state.close()

    print(f"[verify] 分片={result['shard_count']} 条目={total_entries} "
          f"字节={total_bytes} 跨片重复hash={len(cross_feed_dups)}",
          file=sys.stderr)
    print(f"[verify] 数据缺陷台账: {json.dumps(issue_counts, ensure_ascii=False)}",
          file=sys.stderr)
    if hard_failures:
        for f_ in hard_failures:
            print(f"[FAIL] {f_}", file=sys.stderr)
        print(f"[verify] 结果: 失败（{len(hard_failures)} 项完整性问题），报告 {report_path}",
              file=sys.stderr)
        return 1
    if args.fail_on_defects and issue_counts:
        print(f"[verify] 结果: 完整性通过，但存在 {sum(issue_counts.values())} "
              f"条源数据缺陷（--fail-on-defects 视为失败），报告 {report_path}",
              file=sys.stderr)
        return 1
    print(f"[verify] 结果: 通过（数据缺陷见 issues 台账/报告），报告 {report_path}",
          file=sys.stderr)
    return 0


# --------------------------------------------------------------------------
# 对账报告
# --------------------------------------------------------------------------
def cmd_report(args) -> int:
    out = args.output
    state = State(os.path.join(out, "state.db"))
    conn = state.conn

    feed_rows = conn.execute(
        "SELECT feed_id, user_id, title, feed_url, shard, entry_count, byte_size, "
        "sha256, status, finished_at FROM feeds ORDER BY feed_id").fetchall()
    issue_rows = conn.execute(
        "SELECT kind, count(*) FROM issues GROUP BY kind ORDER BY kind").fetchall()
    meta = dict(conn.execute("SELECT key, value FROM meta").fetchall())
    cross = conn.execute(
        "SELECT hash, feed_id, entry_id, detail FROM issues "
        "WHERE kind='DUP_HASH_CROSS_FEED' ORDER BY feed_id").fetchall()
    orphans = conn.execute(
        "SELECT feed_id, entry_count, shard, sha256 FROM feeds "
        "WHERE status='orphan' ORDER BY feed_id").fetchall()
    unknowns = conn.execute(
        "SELECT feed_id, entry_count, shard, sha256 FROM feeds "
        "WHERE status='unknown' ORDER BY feed_id").fetchall()
    in_feed_dups = conn.execute(
        "SELECT feed_id, hash, count(*) FROM issues "
        "WHERE kind='DUP_HASH_IN_FEED' GROUP BY feed_id, hash ORDER BY feed_id"
    ).fetchall()
    missing_content = conn.execute(
        "SELECT count(*) FROM issues WHERE kind='MISSING_CONTENT'").fetchone()[0]

    report = {
        "generated_at": utcnow_iso(),
        "tool_version": TOOL_VERSION,
        "run_meta": meta,
        "totals": {
            "feeds_total": len(feed_rows),
            "feeds_done": sum(1 for r in feed_rows if r[8] == "done"),
            "feeds_orphan": len(orphans),
            "feeds_unknown": len(unknowns),
            "entries_archived": sum(r[5] for r in feed_rows),
            "bytes_archived": sum(r[6] for r in feed_rows),
            "entries_missing_content": missing_content,
            "duplicate_hash_in_feed_groups": len(in_feed_dups),
            "duplicate_hash_cross_feed": len(cross),
        },
        "issue_counts": dict(issue_rows),
        "orphan_shards": [
            {"feed_id": r[0], "entry_count": r[1], "shard": r[2], "sha256": r[3]}
            for r in orphans],
        "unknown_shards": [
            {"feed_id": r[0], "entry_count": r[1], "shard": r[2], "sha256": r[3]}
            for r in unknowns],
        "in_feed_duplicate_groups": [
            {"feed_id": r[0], "hash": r[1], "occurrences": r[2]} for r in in_feed_dups],
        "cross_feed_duplicates": [
            {"hash": r[0], "feed_id": r[1], "entry_id": r[2], "detail": r[3]}
            for r in cross],
        "shards": [
            {"feed_id": r[0], "user_id": r[1], "title": r[2], "feed_url": r[3],
             "shard": r[4], "entry_count": r[5], "byte_size": r[6],
             "sha256": r[7], "status": r[8], "finished_at": r[9]}
            for r in feed_rows],
    }
    state.close()

    if args.json:
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return 0

    t = report["totals"]
    print("=" * 72)
    print("Miniflux 归档对账清单")
    print("=" * 72)
    print(f"工具版本: {TOOL_VERSION}    生成时间: {report['generated_at']}")
    print(f"源: {meta.get('source_kind')}  扫描行: {meta.get('scanned')} "
          f"导出: {meta.get('exported')}  续跑跳过: {meta.get('resumed_skipped_rows', 0)}")
    if meta.get("source_total"):
        print(f"源端条目总数: {meta['source_total']}")
    print("-" * 72)
    print(f"订阅分片: {t['feeds_done']} 个正常 + {t['feeds_orphan']} 个孤儿"
          f" + {t['feeds_unknown']} 个未知（无 feeds 清单）")
    print(f"归档条目: {t['entries_archived']}  归档字节: {t['bytes_archived']}")
    print(f"缺正文条目: {t['entries_missing_content']}")
    print(f"片内 hash 重复组: {t['duplicate_hash_in_feed_groups']}  "
          f"跨订阅 hash 重复: {t['duplicate_hash_cross_feed']}")
    print("-" * 72)
    print("问题分类计数:")
    for kind, cnt in report["issue_counts"].items():
        print(f"  {kind:24s} {cnt}")
    if orphans:
        print("-" * 72)
        print("孤儿分片（订阅已删、条目仍挂着；已隔离到 orphans/，未混入正常归档）:")
        for r in orphans:
            print(f"  feed_id={r[0]:<10d} entries={r[1]:<7d} {r[2]}")
    if unknowns:
        print("-" * 72)
        print("未知分片（未提供 feeds 清单，无法判定孤儿；落在 unknown/，补清单后可重跑定性）:")
        for r in unknowns:
            print(f"  feed_id={r[0]:<10d} entries={r[1]:<7d} {r[2]}")
    print("-" * 72)
    print("分片明细:")
    for r in feed_rows:
        print(f"  feed_id={r[0]:<8d} status={r[8]:<6s} entries={r[5]:<7d} "
              f"sha256={(r[7] or '')[:12]:<12s} {r[4] or ''}")
    print("=" * 72)
    return 0


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------
def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="按订阅分片的条目归档导出/校验/对账工具")
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_exp = sub.add_parser("export", help="流式扫描并按 feed_id 分片导出")
    p_exp.add_argument("--source", required=True,
                       choices=["jsonl", "pg-psycopg2", "pg-psql"])
    p_exp.add_argument("--entries", help="jsonl 数据源的条目文件（按 feed_id,id 有序）")
    p_exp.add_argument("--feeds", help="jsonl 数据源的订阅文件（用于孤儿检测，可选）")
    p_exp.add_argument("--dsn", help="PostgreSQL DSN，缺省读 DATABASE_URL")
    p_exp.add_argument("--output", required=True, help="归档输出目录")
    p_exp.set_defaults(func=cmd_export)

    p_ver = sub.add_parser("verify", help="独立重读分片做完整性校验与对账")
    p_ver.add_argument("--output", required=True)
    p_ver.add_argument("--fail-on-defects", action="store_true",
                       help="存在源数据缺陷台账时也以非零码退出（CI 门禁用）")
    p_ver.set_defaults(func=cmd_verify)

    p_rep = sub.add_parser("report", help="输出可对账清单（文本或 JSON）")
    p_rep.add_argument("--output", required=True)
    p_rep.add_argument("--json", action="store_true", help="输出 JSON 而非文本")
    p_rep.set_defaults(func=cmd_report)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
