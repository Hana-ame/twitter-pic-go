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
from datetime import datetime, timedelta, timezone  # 引入 timezone

load_dotenv()


def auth_token():
    return os.getenv("AUTH_TOKEN")



username = sys.argv[1] if len(sys.argv) > 1 else "lulu463098"


def should_skip_processing(username):
    """
    检查是否应该跳过处理（最后修改在一天内）
    """
    conn = sqlite3.connect("twitter.db", timeout=5)
    try:
        conn.execute("PRAGMA journal_mode=WAL;")
        cursor = conn.cursor()

        cursor.execute("SELECT last_modify FROM users WHERE username = ?", (username,))
        result = cursor.fetchone()

        if result:
            # 关键修复：SQLite 存的是 UTC 字符串，解析时需指定 UTC
            # SQLite 默认格式是 YYYY-MM-DD HH:MM:SS
            last_modify = datetime.strptime(result[0], "%Y-%m-%d %H:%M:%S").replace(
                tzinfo=timezone.utc
            )

            # 获取当前的 UTC 时间进行对比
            now_utc = datetime.now(timezone.utc)

            return (now_utc - last_modify) <= timedelta(days=1)

        return False

    except Exception as e:
        print(f"查询错误: {e}")
        return False
    finally:
        conn.close()


if should_skip_processing(username):
    print("直接返回，不执行后续操作")
    exit(1)
    # 直接退出或返回


def get_media_data_by_username(username: str):
    url = f"https://x.com/{username}/media"

    # is url valid?
    match = re.match(twitter.TwitterMediaExtractor.pattern, url)
    if not match:
        raise ValueError(f"Invalid URL for {url}: {match}")

    extractor = twitter.TwitterMediaExtractor(match)

    config_dict = {"cookies": {"auth_token": auth_token()}}

    extractor.config = lambda key, default=None: config_dict.get(key, default)

    try:
        extractor.initialize()

        api = twitter.TwitterAPI(extractor)
        try:
            if username.startswith("id:"):
                user = api.user_by_rest_id(username[3:])
            else:
                user = api.user_by_screen_name(username)

            if "legacy" in user and user["legacy"].get("withheld_scope"):
                raise ValueError("withheld")

        except Exception as e:
            error_msg = str(e).lower()
            if "withheld" in error_msg or (
                # hasattr(e, "response") and "withheld" in str(e.response.text).lower()
            ):
                raise ValueError("withheld")
            raise

        structured_output = {"account_info": {}, "total_urls": 0, "timeline": []}

        iterator = iter(extractor)

        # dunno what to skip, commeted.
        # if batch_size > 0 and page > 0:
        #     items_to_skip = page * batch_size

        #     if hasattr(extractor, "_cursor") and extractor._cursor:
        #         pass
        #     else:
        #         skipped = 0
        #         try:
        #             for _ in range(items_to_skip):
        #                 next(iterator)
        #                 skipped += 1
        #         except StopIteration:
        #             pass

        new_timeline_entries = []

        # items_to_fetch = batch_size if batch_size > 0 else float("inf")
        items_to_fetch = float("inf")
        items_fetched = 0

        try:
            while items_fetched < items_to_fetch:
                item = next(iterator)
                items_fetched += 1

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
                            "statuses_count": user.get("statuses_count", 0),
                        }

                    if "pbs.twimg.com" in media_url or "video.twimg.com" in media_url:
                        tweet_date = tweet_data.get("date", datetime.now())
                        if isinstance(tweet_date, datetime):
                            tweet_date = tweet_date.strftime("%Y-%m-%d %H:%M:%S")

                        timeline_entry = {
                            "url": media_url,
                            "date": tweet_date,
                            "tweet_id": tweet_data.get("tweet_id", 0),
                        }

                        if "type" in tweet_data:
                            timeline_entry["type"] = tweet_data["type"]

                        new_timeline_entries.append(timeline_entry)
                        structured_output["total_urls"] += 1
        except StopIteration:
            pass

        structured_output["timeline"].extend(new_timeline_entries)

        cursor_info = None
        if hasattr(extractor, "_cursor") and extractor._cursor:
            cursor_info = extractor._cursor

        structured_output["metadata"] = {
            "new_entries": len(new_timeline_entries),
            # "page": page,
            # "batch_size": batch_size,
            # "has_more": batch_size > 0 and items_fetched == batch_size,
            "cursor": cursor_info,
        }

        if not structured_output["account_info"]:
            raise ValueError(
                "Failed to fetch account information. Please check the username and auth token."
            )

    # except Exception as e:
    #     print(e)
    #     raise e

    # not a function
    # is a function
    except Exception as e:
        error_msg = str(e).lower()
        if (
            "withheld" in error_msg
            or e.__class__.__name__ == "ValueError"
            and str(e) == "withheld"
        ):
            return {
                "error": "To download withheld accounts, use this userscript version: https://www.patreon.com/exyezed"
            }
        else:
            error_str = traceback.format_exc()
            if error_str == "None":
                return {
                    "error": "Failed to authenticate. Please verify your auth token is valid and not expired."
                }
            else:
                return {"error": str(e)}

    print(structured_output)
    return structured_output


output = get_media_data_by_username(username)
info: Any = output.get("account_info")

if not info:
    print("未能获取到用户信息")
    exit(1)

with gzip.open(
    f"{info.get('name')}.json.gz", "wt", encoding="utf-8", compresslevel=9
) as f:
    f.write(json.dumps(output))

# --- 数据库操作部分 ---
conn = sqlite3.connect("twitter.db", timeout=5)
try:
    conn.execute("PRAGMA journal_mode=WAL;")
    cursor = conn.cursor()

    # 1. 更新 users 表
    # 使用 CURRENT_TIMESTAMP，SQLite 会自动存入当前 UTC 时间
    user_query = """
    INSERT INTO users (username, nick, status, last_modify)
    VALUES (?, ?, ?, CURRENT_TIMESTAMP)
    ON CONFLICT(username)
    DO UPDATE SET
        nick = excluded.nick,
        last_modify = CURRENT_TIMESTAMP
    """
    cursor.execute(user_query, (info.get("name"), info.get("nick"), "SUCCESS"))

    # # 2. 确保 user_tags 表有对应记录（配合 Go 的 User 结构体）
    # # 如果该用户在 tags 表没记录，则插入一条空的
    # tags_query = """
    # INSERT INTO user_tags (username, tags, last_modify)
    # VALUES (?, r'{}', CURRENT_TIMESTAMP)
    # ON CONFLICT(username) DO NOTHING
    # """
    # cursor.execute(tags_query, (info.get("name"),))

    conn.commit()
    print(f"用户 {info.get('name')} 数据已更新")

except Exception as e:
    print(f"数据库操作失败: {e}")
    conn.rollback()
finally:
    conn.close()
