# Ban 操作核心要点

## 1. Ban IP

```bash
# 添加到第一行
sed -i '1s/^/IP地址\n/' /root/twitter/bans.txt

# 验证
grep "IP地址" /root/twitter/bans.txt
head -5 /root/twitter/bans.txt
```

- **生效时间**：10分钟内自动加载（BanManager定时reload）
- **不需要重启服务**

---

## 2. Ban User

```bash
# 设置status为BANNED
sqlite3 /root/twitter/twitter.db \
  "UPDATE users SET status='BANNED' WHERE username='用户名';"

# 查询关联IP（仅记录，不封禁）
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT DISTINCT SUBSTR(ip, 1, INSTR(ip, ',') - 1) as real_ip, tags, created_at 
   FROM request_logs WHERE username='用户名' ORDER BY created_at DESC;"
```

- **生效时间**：立即生效（代码每次请求检查 `user.Status != "SUCCESS"`）
- **不封IP**：只改status，不操作bans.txt
- **不需要重启服务**

---

## 3. 查询NAN tag用户

```bash
# 查所有含NAN tag的用户
sqlite3 /root/twitter/twitter.db \
  "SELECT DISTINCT username FROM request_logs WHERE tags LIKE '%NAN%';"

# 查这些用户的IP
sqlite3 /root/twitter/twitter.db \
  "SELECT DISTINCT SUBSTR(ip, 1, INSTR(ip, ',') - 1) FROM request_logs WHERE tags LIKE '%NAN%';"
```

---

## 4. 预览MD写法

### 图片格式
```markdown
![](https://pbs.moonchan.xyz/media/HRaqdcLbAAABRkG?format=jpg&name=orig)
```

### 视频格式
```markdown
[视频](https://pbs.moonchan.xyz/amplify_video/2093583657336528897/vid/avc1/720x1280/8WLO-K8mHbpA-0j4.mp4?tag=29)
```

### 替换规则
| 原域名 | 代理域名 |
|--------|----------|
| `pbs.twimg.com` | `pbs.moonchan.xyz` |
| `video.twimg.com` | `pbs.moonchan.xyz` |

**注意**：
- 保留完整路径和参数（`?format=jpg&name=orig`、`?tag=29`等）
- 图片用 `![]()` Markdown语法
- 视频用 `[文字](URL)` Markdown语法

---

## 5. 批量封禁示例（NAN tag）

```bash
# 1. 封禁所有NAN tag用户
sqlite3 /root/twitter/twitter.db \
  "UPDATE users SET status='BANNED' WHERE username IN (SELECT DISTINCT username FROM request_logs WHERE tags LIKE '%NAN%');"

# 2. 封禁关联IP
sqlite3 /root/twitter/twitter.db \
  "SELECT DISTINCT SUBSTR(ip, 1, INSTR(ip, ',') - 1) FROM request_logs WHERE tags LIKE '%NAN%';" \
  | while read ip; do sed -i "1s/^/${ip}\\n/" /root/twitter/bans.txt; done
```
