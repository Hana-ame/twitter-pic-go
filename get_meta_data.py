import re
import os
import sys
from dotenv import load_dotenv
import json
import gzip
import sqlite3
import traceback
from datetime import datetime, timedelta
from gallery_dl.extractor import twitter
from typing import Any

load_dotenv()

def auth_token():
    return  os.getenv("AUTH_TOKEN") 

username =  sys.argv[1] if len(sys.argv) > 1 else "lulu463098"


# print(username)
# print(auth_token())

def should_skip_processing(username):
    """
    检查是否应该跳过处理（最后修改在一天内）
    """
    conn = sqlite3.connect('twitter.db', timeout=5)
    try:
        conn.execute('PRAGMA journal_mode=WAL;')
        cursor = conn.cursor()

        cursor.execute("SELECT last_modify FROM users WHERE username = ?", (username,))
        result = cursor.fetchone()

        if result:
            last_modify = datetime.strptime(result[0], "%Y-%m-%d %H:%M:%S")
            return (datetime.now() - last_modify) <= timedelta(days=1)

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


def get_media_data_by_username(username:str):
    url = f"https://x.com/{username}/media"

    # is url valid?
    match = re.match(twitter.TwitterMediaExtractor.pattern, url)
    if not match:
        raise ValueError(f"Invalid URL for {url}: {match}")

    extractor = twitter.TwitterMediaExtractor(match)

    config_dict = {"cookies": {"auth_token":auth_token()}}

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
# print(output)

#with open(f"{info.get('name')}.json", "w") as f:
#    json.dump(output, f)

with gzip.open(f"{info.get('name')}.json.gz", "wt", encoding="utf-8", compresslevel=9) as f:
    f.write(json.dumps(output))

# 存储入"twitter.db",是一个sqlite3数据库
# 要求  query = `INSERT OR REPLACE INTO users (username, nick, status, last_modify)
#                                      VALUES (?, ?, ?, CURRENT_TIMESTAMP)`
# 在建立连接后启用 WAL 模式和设置繁忙超时
conn = sqlite3.connect('twitter.db', timeout=5)
try:
    conn.execute('PRAGMA journal_mode=WAL;')  # 启用 WAL 模式
    cursor = conn.cursor()

    # 使用 ON CONFLICT DO UPDATE SET 来精确控制冲突时的更新行为
    query = """
    INSERT INTO users (username, nick, status, last_modify)
    VALUES (?, ?, ?, CURRENT_TIMESTAMP)
    ON CONFLICT(username)
    DO UPDATE SET
        nick = excluded.nick,
        last_modify = CURRENT_TIMESTAMP
    -- 注意：这里没有更新 status 字段，因此冲突时会保留原有的 status 值
    """

    # info: Any = output.get("account_info")
    cursor.execute(query, (info.get('name'), info.get('nick'), "SUCCESS")) # 新插入的行 status 为 "SUCCESS"
    conn.commit()  # 及时提交事务

except Exception as e:
    print(f"An error occurred: {e}") # 建议至少打印异常信息
    conn.rollback()
finally:
    conn.close()

# print(output)