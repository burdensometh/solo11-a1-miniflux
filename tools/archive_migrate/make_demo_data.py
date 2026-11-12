#!/usr/bin/env python3
"""make_demo_data.py — 生成带已知缺陷的 Miniflux 风格演示数据（仅测试用）。

造数规则（便于对账）：
  - feeds: 5 个在册订阅（id 1..5）；feed 99 已删除但条目仍在 → 孤儿
  - entries: 按 feed_id,id 有序，共 N 条
  - 缺陷:
      MISSING_CONTENT   feed1 中每 7 条有 1 条 content 为空
      EMPTY_HASH        feed2 中每 11 条有 1 条 hash 为空
      DUP_HASH_IN_FEED  feed3 中 2 个 hash 各出现 2 次
      DUP_HASH_CROSS_FEED feed4 与 feed5 共享 3 个 hash
      ORPHAN_ENTRY      feed99 的 4 条（feeds 文件里没有 99）
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os


def make_hash(feed_id: int, seq: int) -> str:
    return hashlib.sha256(f"feed{feed_id}-entry{seq}".encode()).hexdigest()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="输出目录（建议为空目录）")
    ap.add_argument("--per-feed", type=int, default=3000,
                    help="每个在册订阅的条目数（默认 3000，总计约 1.5 万）")
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)

    feeds_path = os.path.join(args.out, "feeds.jsonl")
    entries_path = os.path.join(args.out, "entries.jsonl")

    with open(feeds_path, "w", encoding="utf-8") as ff:
        for fid in range(1, 6):
            ff.write(json.dumps({
                "id": fid, "user_id": 1,
                "title": f"演示订阅 {fid}",
                "feed_url": f"https://example.invalid/feed/{fid}/feed.xml",
            }, ensure_ascii=False) + "\n")

    entry_id = 0
    dup_in_feed_hashes = {}
    cross_hashes = [make_hash(4, i) for i in range(3)]  # feed4/feed5 共享

    with open(entries_path, "w", encoding="utf-8") as fe:
        def emit(feed_id: int, seq: int, *, hsh=None, content=None,
                 title=None, url=None):
            nonlocal entry_id
            entry_id += 1
            row = {
                "id": entry_id,
                "user_id": 1,
                "feed_id": feed_id,
                "hash": hsh if hsh is not None else make_hash(feed_id, seq),
                "published_at": f"2026-01-{(seq % 28) + 1:02d}T08:00:00Z",
                "title": title if title is not None else f"feed{feed_id} 标题 {seq}",
                "url": url if url is not None else
                       f"https://example.invalid/feed/{feed_id}/entry/{seq}",
                "comments_url": "",
                "author": f"作者 {feed_id}",
                "share_code": "",
                "content": content if content is not None else
                           f"<p>feed{feed_id} 正文内容 {seq}</p>",
                "status": "read" if seq % 2 else "unread",
                "starred": seq % 5 == 0,
                "reading_time": 1,
                "created_at": "2026-01-01T00:00:00Z",
                "changed_at": "2026-01-02T00:00:00Z",
                "tags": [f"tag{seq % 4}"],
                "language": "zh",
            }
            fe.write(json.dumps(row, ensure_ascii=False) + "\n")

        for feed_id in range(1, 6):
            dup_in_feed_hashes[feed_id] = make_hash(feed_id, 100)
            for seq in range(1, args.per_feed + 1):
                kw = {}
                if feed_id == 1 and seq % 7 == 0:
                    kw["content"] = ""                      # 缺正文
                if feed_id == 2 and seq % 11 == 0:
                    kw["hsh"] = ""                          # 空 hash
                if feed_id == 3 and seq in (50, 51):
                    kw["hsh"] = dup_in_feed_hashes[3]       # 片内重复（2 次）
                if feed_id == 3 and seq in (120, 122):
                    kw["hsh"] = make_hash(3, 999)           # 第二组片内重复
                if feed_id == 4 and seq < 3:
                    kw["hsh"] = cross_hashes[seq]           # 跨片重复（feed5 侧）
                if feed_id == 5 and seq in (7, 8, 9):
                    kw["hsh"] = cross_hashes[seq - 7]       # 与 feed4 撞 hash
                emit(feed_id, seq, **kw)

        # 孤儿：feed 99 不在 feeds.jsonl 里
        for seq in range(1, 5):
            emit(99, seq)

    print(f"feeds   -> {feeds_path}")
    print(f"entries -> {entries_path} (共 {entry_id} 条)")


if __name__ == "__main__":
    raise SystemExit(main())
