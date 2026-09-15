#!/usr/bin/env python3
"""从 bwh 拉回的 twitter.db 快照构建 gallery 用的 tags.db（新表 account_tags）。

用法：
  # bwh 快照（含 WAL 一致性）：
  #   sqlite3 /root/twitter/twitter.db "VACUUM INTO '/tmp/twitter_snap.db'"
  #   scp -P 26275 root@bwh...:/tmp/twitter_snap.db ../../data/twitter.db
  python3 build_tags_db.py [twitter.db=../../data/twitter.db] [tags.db=../../data/tags.db]

twitter.db.user_tags.tags 是 JSON 对象 {"tag": weight, ...}，拍平成
account_tags(username, tag, weight) 规范表，按 (username, weight DESC, tag) 有序。
"""
import json
import sqlite3
import sys

src = sys.argv[1] if len(sys.argv) > 1 else "../../data/twitter.db"
dst = sys.argv[2] if len(sys.argv) > 2 else "../../data/tags.db"

s = sqlite3.connect(src)
try:
    rows = list(s.execute("SELECT username, tags FROM user_tags"))
finally:
    s.close()

out = sqlite3.connect(dst)
out.isolation_level = None  # VACUUM 需要 autocommit
out.executescript("""
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS account_tags (
    username TEXT NOT NULL,
    tag      TEXT NOT NULL,
    weight   REAL NOT NULL DEFAULT 1,
    PRIMARY KEY (username, tag)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_account_tags_tag ON account_tags(tag);
DELETE FROM account_tags;
""")

# 批插 + 单次 commit：/mnt/d (drvfs) 上逐条 autocommit 会被 fsync 拖死。
# 建议 dst 先建在 Linux 本地 fs（/tmp），完成后 cp 到 Windows 目录。
pairs = []
bad = 0
for username, tags in rows:
    if not tags:
        continue
    try:
        obj = json.loads(tags)
    except Exception:
        bad += 1
        continue
    if not isinstance(obj, dict):
        bad += 1
        continue
    for tag, w in obj.items():
        tag = str(tag).strip()
        if not tag:
            continue
        try:
            w = float(w)
        except Exception:
            w = 1.0
        pairs.append((username, tag, w))

out.executemany("INSERT OR REPLACE INTO account_tags VALUES (?,?,?)", pairs)
out.commit()
n = len(pairs)

out.execute("VACUUM")
out.commit()
users = out.execute("SELECT COUNT(DISTINCT username) FROM account_tags").fetchone()[0]
tags = out.execute("SELECT COUNT(DISTINCT tag) FROM account_tags").fetchone()[0]
out.close()
print(f"tags.db: {n} rows, {users} accounts, {tags} distinct tags, {bad} bad json")
