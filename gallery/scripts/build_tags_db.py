#!/usr/bin/env python3
"""【已停用 · 2026-09-15】从 bwh 拉回的 twitter.db 快照构建独立的 tags.db。

标签已收敛为**单一 twitter.db 的 account_tags 表**：gallery 直接读写线上
twitter.db（env `GALLERY_DB`，默认 `./twitter.db`），与 twitter REST API 共用
`tags` 包的同一套读写语义。快照/拍平这条管线在代码里已无任何引用，本脚本仅
留在仓库备查，不要再跑、不要再往机器上推 tags.db。

历史行为（保留说明）：
  account_tags(username, tag, weight)  —— user_tags 的 JSON 拍平（按账号查标签 / 按标签反查账号）
  accounts(username, last_modify)      —— users 的更新时间（首页按 update 从新到旧排序）
时间排序源现在直接读 `users.last_modify`；旧 user_tags JSON 的一次性回填仍由
服务端启动时的 migrateAccountTags 负责（见 sql_tags.go）。
"""
import json
import os
import sqlite3
import sys

_here = os.path.dirname(os.path.abspath(__file__))
# 默认路径按脚本位置解析（twitter-pic/data/），dst 建议先指 /tmp 再 cp 到 Windows 目录（见上）
src = sys.argv[1] if len(sys.argv) > 1 else os.path.join(_here, "..", "..", "..", "data", "twitter.db")
dst = sys.argv[2] if len(sys.argv) > 2 else os.path.join(_here, "..", "..", "..", "data", "tags.db")

s = sqlite3.connect(src)
try:
    tabs = {r[0] for r in s.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    pairs = []
    if "account_tags" in tabs:
        # 新版服务已直接维护规范表，原样拷贝
        for u, t, w in s.execute("SELECT username, tag, weight FROM account_tags"):
            u, t = str(u or "").strip(), str(t or "").strip()
            if not u or not t:
                continue
            try:
                w = float(w)
            except Exception:
                w = 1.0
            pairs.append((u, t, w))
        rows = []
    else:
        # 旧快照：从 user_tags 的 JSON 拍平
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
CREATE TABLE IF NOT EXISTS accounts (
    username    TEXT PRIMARY KEY,
    last_modify TEXT NOT NULL
) WITHOUT ROWID;
DELETE FROM account_tags;
DELETE FROM accounts;
""")


# 批插 + 单次 commit：/mnt/d (drvfs) 上逐条 autocommit 会被 fsync 拖死。
# 建议 dst 先建在 Linux 本地 fs（/tmp），完成后 cp 到 Windows 目录。
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

# accounts：全部账号的更新时间（含未打标签的），首页排序用
s = sqlite3.connect(src)
try:
    accts = [(u, lm or "") for u, lm in s.execute("SELECT username, last_modify FROM users")]
finally:
    s.close()
out.executemany("INSERT OR REPLACE INTO accounts VALUES (?,?)", accts)
out.commit()

out.execute("VACUUM")
out.commit()
users = out.execute("SELECT COUNT(DISTINCT username) FROM account_tags").fetchone()[0]
tags = out.execute("SELECT COUNT(DISTINCT tag) FROM account_tags").fetchone()[0]
ac = out.execute("SELECT COUNT(*) FROM accounts").fetchone()[0]
out.close()
print(f"tags.db: {n} tag rows, {users} tagged accounts, {tags} distinct tags, {bad} bad json, {ac} accounts w/ last_modify")
