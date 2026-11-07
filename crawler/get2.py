import re
import os
import sys
from dotenv import load_dotenv
import json
import gzip
import sqlite3
import traceback
from gallery_dl.extractor import twitter
from typing import Any
from datetime import datetime, timedelta, timezone
import requests
from requests.adapters import HTTPAdapter

# ---------- 日志 ----------
def log(msg, level="INFO"):
    timestamp = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    print(f"[{timestamp}] [{level}] {msg}")

load_dotenv()

# ==================== 出口 IP ====================
# BIND_IP = "117.55.237.217"   # 先使用 IPv4（已验证 IPv6 不通）
BIND_IP = os.getenv("BIND_IP") or None  # 如需默认路由，留空即可
# =================================================

if BIND_IP:
    log(f"当前配置的出口 IP 为: {BIND_IP}")
else:
    log("未绑定 IP，将使用系统默认出口", "INFO")

# ---------- 自定义 Adapter ----------
class SourceAddressAdapter(HTTPAdapter):
    def __init__(self, source_address, *args, **kwargs):
        self.source_address = source_address
        super().__init__(*args, **kwargs)

    def init_poolmanager(self, *args, **kwargs):
        kwargs['source_address'] = self.source_address
        return super().init_poolmanager(*args, **kwargs)

    def proxy_manager_for(self, *args, **kwargs):
        kwargs['source_address'] = self.source_address
        return super().proxy_manager_for(*args, **kwargs)

# ---------- 读取环境变量 ----------
def auth_token():
    token = os.getenv("AUTH_TOKEN")
    if not token:
        log("未找到 AUTH_TOKEN 环境变量", "ERROR")
    return token

def ct0_token():
    return os.getenv("CT0")  # 可能为 None

def should_skip_processing(username, db_path="twitter.db"):
    # 如果表不存在，直接返回 False 继续抓取
    conn = sqlite3.connect(db_path, timeout=5)
    try:
        conn.execute("PRAGMA journal_mode=WAL;")
        cursor = conn.cursor()
        cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='users'")
        if not cursor.fetchone():
            log("表 users 不存在，跳过检查", "WARN")
            return False
        cursor.execute("SELECT last_modify FROM users WHERE username = ?", (username,))
        result = cursor.fetchone()
        if result and result[0]:
            raw_ts = str(result[0]).strip()
            # 兼容 YYYY-MM-DD HH:MM:SS 与 ISO8601
            try:
                last_modify = datetime.strptime(raw_ts[:19], "%Y-%m-%d %H:%M:%S").replace(tzinfo=timezone.utc)
            except ValueError:
                last_modify = datetime.fromisoformat(raw_ts.replace("Z", "+00:00"))
            now_utc = datetime.now(timezone.utc)
            diff = now_utc - last_modify
            if diff <= timedelta(days=1):
                log(f"用户 {username} 上次更新在 {diff.total_seconds()/3600:.1f} 小时内，跳过处理", "INFO")
                return True
            else:
                log(f"用户 {username} 上次更新超过一天，继续抓取", "INFO")
                return False
        else:
            log(f"用户 {username} 不在数据库中，首次抓取", "INFO")
            return False
    except Exception as e:
        log(f"查询数据库出错: {e}", "ERROR")
        return False
    finally:
        conn.close()

# ---------- 核心抓取函数 ----------
def get_media_data_by_username(username: str):
    log(f"开始获取用户 {username} 的媒体数据")
    url = f"https://x.com/{username}/media"
    match = re.match(twitter.TwitterMediaExtractor.pattern, url)
    if not match:
        raise ValueError(f"Invalid URL for {url}: {match}")

    auth = auth_token()
    if not auth:
        raise ValueError("AUTH_TOKEN is empty!")

    ct0 = ct0_token()
    cookies = {"auth_token": auth}
    if ct0:
        cookies["ct0"] = ct0
    headers = {
        "Accept": "application/json, text/plain, */*",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Origin": "https://x.com",
        "Referer": "https://x.com/",
    }
    if ct0:
        headers["x-csrf-token"] = ct0

    config_dict = {
        "cookies": cookies,
        "headers": headers,
        "user_agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
        "previews": True,
        "cards": True,
    }

    extractor = twitter.TwitterMediaExtractor(match)
    extractor.config = lambda key, default=None: config_dict.get(key, default)

    if BIND_IP:
        log(f"正在为请求绑定源 IP: {BIND_IP}")
        session = requests.Session()
        adapter = SourceAddressAdapter(source_address=(BIND_IP, 0))
        adapter.max_retries = 3
        session.mount('http://', adapter)
        session.mount('https://', adapter)
        session.headers.update(headers)
        extractor.session = session
        log("源 IP 绑定完成")
    else:
        log("未绑定 IP，使用系统默认路由", "INFO")

    try:
        log("正在初始化 extractor...")
        extractor.initialize()
        log("extractor 初始化成功")

        api = twitter.TwitterAPI(extractor)
        try:
            if username.startswith("id:"):
                user = api.user_by_rest_id(username[3:])
            else:
                user = api.user_by_screen_name(username)
            if "legacy" in user and user["legacy"].get("withheld_scope"):
                raise ValueError("withheld")
            log(f"成功获取用户信息: {user.get('legacy', {}).get('name', '')}")
        except Exception as e:
            error_msg = str(e).lower()
            if "withheld" in error_msg:
                raise ValueError("withheld")
            raise

        structured_output = {
            "account_info": {}, "total_urls": 0, "timeline": [], "texts": {}
        }
        iterator = iter(extractor)
        new_timeline_entries = []
        poster_by_tweet = {}
        items_fetched = 0
        log("开始遍历媒体 timeline...")

        try:
            while True:
                item = next(iterator)
                items_fetched += 1
                if items_fetched % 50 == 0:
                    log(f"已处理 {items_fetched} 条推文，当前收集媒体 {len(new_timeline_entries)} 个")
                if isinstance(item, tuple) and len(item) >= 3:
                    media_url = item[1]
                    tweet_data = item[2]
                    if not structured_output["account_info"] and "user" in tweet_data:
                        user = tweet_data["user"]
                        user_date = user.get("date", "")
                        if isinstance(user_date, datetime):
                            user_date = user_date.strftime("%Y-%m-%d %H:%M:%S")
                        structured_output["account_info"] = {
                            "name": user.get("name", ""),
                            "nick": user.get("nick", ""),
                            "date": user_date,
                            "followers_count": user.get("followers_count", 0),
                            "friends_count": user.get("friends_count", 0),
                            "profile_image": user.get("profile_image", ""),
                            "profile_banner": user.get("profile_banner", ""),
                            "statuses_count": user.get("statuses_count", 0),
                        }
                        log(f"获取到账户信息: {structured_output['account_info']['name']} (@{structured_output['account_info']['nick']})")
                    if "pbs.twimg.com" in media_url or "video.twimg.com" in media_url:
                        ttype = tweet_data.get("type") or "card"
                        tid = tweet_data.get("tweet_id", 0)
                        if ttype == "preview":
                            poster_by_tweet.setdefault(tid, media_url)
                            continue
                        tweet_date = tweet_data.get("date", datetime.now())
                        if isinstance(tweet_date, datetime):
                            tweet_date = tweet_date.strftime("%Y-%m-%d %H:%M:%S")
                        new_timeline_entries.append({
                            "url": media_url,
                            "date": tweet_date,
                            "tweet_id": tid,
                            "type": ttype,
                        })
                        key = str(tid)
                        if tid and key not in structured_output["texts"]:
                            content = (tweet_data.get("content") or "").strip()
                            if content:
                                structured_output["texts"][key] = {
                                    "content": content,
                                    "conversation_id": tweet_data.get("conversation_id") or 0,
                                    "reply_id": tweet_data.get("reply_id") or 0,
                                    "quote_id": tweet_data.get("quote_id") or 0,
                                }
                        structured_output["total_urls"] += 1
        except StopIteration:
            log("遍历结束（到达末尾）", "INFO")

        covered = 0
        for entry in new_timeline_entries:
            if entry["type"] in ("video", "animated_gif"):
                cover = poster_by_tweet.get(entry["tweet_id"])
                if cover:
                    entry["cover"] = cover
                    covered += 1
        if poster_by_tweet:
            log(f"已回填 {covered} 条视频封面（poster {len(poster_by_tweet)} 张）", "INFO")

        structured_output["timeline"].extend(new_timeline_entries)
        cursor_info = None
        if hasattr(extractor, "_cursor") and extractor._cursor:
            cursor_info = extractor._cursor
        structured_output["metadata"] = {
            "new_entries": len(new_timeline_entries),
            "cursor": cursor_info,
        }
        log(f"抓取完成: 共获得 {structured_output['total_urls']} 个媒体文件")

        if not structured_output["account_info"]:
            raise ValueError("Failed to fetch account information. Please check the username and auth token.")

    except Exception as e:
        error_msg = str(e).lower()
        log(f"抓取过程中发生异常: {error_msg}", "ERROR")
        if "withheld" in error_msg or (isinstance(e, ValueError) and str(e) == "withheld"):
            return {
                "error": "To download withheld accounts, use this userscript version: https://www.patreon.com/exyezed"
            }
        else:
            error_str = traceback.format_exc()
            log(f"详细错误堆栈:\n{error_str}", "ERROR")
            return {"error": str(e)}

    return structured_output


def compute_media_counts(timeline):
    """按 URL 去重统计 total_urls, photo_count, video_count（与 Go 侧 loadUserDoc 口径一致）。"""
    seen = set()
    photo_cnt = 0
    video_cnt = 0
    for item in timeline or []:
        raw_url = (item.get("url") or "").strip()
        if not raw_url or raw_url in seen:
            continue
        seen.add(raw_url)
        ttype = (item.get("type") or "").strip().lower()
        if ttype in ("video", "animated_gif", "article:video"):
            video_cnt += 1
        else:
            photo_cnt += 1
    return len(seen), photo_cnt, video_cnt


def update_database(info: dict, timeline: list, db_path="twitter.db"):
    """将用户资料与媒体统计列（avatar/banner/total_urls/photo_count/video_count）直接写入 users 表。"""
    total_urls, photo_cnt, video_cnt = compute_media_counts(timeline)
    db_username = info.get("name") or ""
    nick = info.get("nick") or db_username
    avatar = (info.get("profile_image") or "").strip()
    banner = (info.get("profile_banner") or "").strip()

    conn = sqlite3.connect(db_path, timeout=5)
    try:
        conn.execute("PRAGMA journal_mode=WAL;")
        cursor = conn.cursor()
        cursor.execute("""
            CREATE TABLE IF NOT EXISTS users (
                username TEXT PRIMARY KEY,
                nick TEXT,
                status TEXT,
                last_modify TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                avatar TEXT DEFAULT '',
                banner TEXT DEFAULT '',
                total_urls INTEGER DEFAULT 0,
                photo_count INTEGER DEFAULT 0,
                video_count INTEGER DEFAULT 0
            )
        """)
        for col_ddl in (
            "ALTER TABLE users ADD COLUMN avatar TEXT DEFAULT ''",
            "ALTER TABLE users ADD COLUMN banner TEXT DEFAULT ''",
            "ALTER TABLE users ADD COLUMN total_urls INTEGER DEFAULT 0",
            "ALTER TABLE users ADD COLUMN photo_count INTEGER DEFAULT 0",
            "ALTER TABLE users ADD COLUMN video_count INTEGER DEFAULT 0",
        ):
            try:
                cursor.execute(col_ddl)
            except sqlite3.OperationalError:
                pass

        user_query = """
        INSERT INTO users (username, nick, status, last_modify, avatar, banner, total_urls, photo_count, video_count)
        VALUES (?, ?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?)
        ON CONFLICT(username)
        DO UPDATE SET
            nick = excluded.nick,
            status = excluded.status,
            last_modify = CURRENT_TIMESTAMP,
            avatar = excluded.avatar,
            banner = excluded.banner,
            total_urls = excluded.total_urls,
            photo_count = excluded.photo_count,
            video_count = excluded.video_count
        """
        cursor.execute(user_query, (db_username, nick, "SUCCESS", avatar, banner, total_urls, photo_cnt, video_cnt))
        conn.commit()
        log(f"用户 {db_username} 在 users 表更新成功 (total={total_urls}, photos={photo_cnt}, videos={video_cnt})")
    except Exception as e:
        log(f"数据库操作失败: {e}", "ERROR")
        conn.rollback()
    finally:
        conn.close()
        log("数据库连接已关闭")


def main():
    username = sys.argv[1] if len(sys.argv) > 1 else "lulu463098"
    log(f"目标用户名: {username}")

    if should_skip_processing(username):
        log("直接返回，不执行后续操作", "INFO")
        sys.exit(0)

    log("========== 开始执行主流程 ==========")
    output = get_media_data_by_username(username)
    info: Any = output.get("account_info")

    if not info:
        log("未能获取到用户信息，退出", "ERROR")
        if "error" in output:
            log(f"错误详情: {output['error']}", "ERROR")
        sys.exit(1)

    log(f"账户信息: {info.get('name')} (@{info.get('nick')}), 粉丝: {info.get('followers_count')}")

    timeline = output.get("timeline", [])
    filename = f"{info.get('name')}.json.gz"
    texts = output.pop("texts", {})
    log(f"正在将数据压缩保存到 {filename}")
    tmp_filename = f"{filename}.tmp"
    with gzip.open(tmp_filename, "wt", encoding="utf-8", compresslevel=9) as f:
        f.write(json.dumps(output))
    os.replace(tmp_filename, filename)
    log(f"数据已保存到 {filename}")

    texts_filename = f"{info.get('name')}.texts.json.gz"
    tmp_texts = f"{texts_filename}.tmp"
    with gzip.open(tmp_texts, "wt", encoding="utf-8", compresslevel=9) as f:
        f.write(json.dumps({"texts": texts}))
    os.replace(tmp_texts, texts_filename)
    log(f"配文已保存到 {texts_filename}（{len(texts)} 条）")

    log("开始更新数据库...")
    update_database(info, timeline)
    log("========== 主流程执行完毕 ==========")


if __name__ == "__main__":
    main()
