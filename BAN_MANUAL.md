# Twitter 封禁操作手册

## 服务器信息

- **BWH 地址**: `bwh.moonchan.xyz`
- **SSH**: `~/script/ssh/bwh.sh`
- **服务目录**: `/root/twitter/`
- **数据库**: `/root/twitter/twitter.db`
- **封禁列表**: `/root/twitter/bans.txt`
- **封禁JSON**: `/root/twitter/banned.json`

---

## 一、Ban IP（封禁IP）

### 1.1 手动添加单个IP

```bash
# SSH到服务器
~/script/ssh/bwh.sh

# 添加IP到第一行
sed -i '1s/^/IP地址\n/' /root/twitter/bans.txt

```

### 1.2 验证IP是否已封禁

```bash
# 搜索IP
grep "IP地址" /root/twitter/bans.txt

# 查看IP归属信息
curl -s https://ipinfo.io/IP地址/json
```

---

## 二、Ban User（封禁用户）

> - `status='BANNED'` → 立即生效（代码每次请求都检查）
> - 查询关联IP → 仅用于记录，不添加到 bans.txt

### 2.1 完整流程

```bash
USERNAME="目标用户名"

# 1. 设置用户status为BANNED（立即生效）
sqlite3 /root/twitter/twitter.db \
  "INSERT OR IGNORE INTO users (username, status) VALUES ('$USERNAME', 'BANNED');"

sqlite3 /root/twitter/twitter.db \
  "UPDATE users SET status='BANNED' WHERE username='$USERNAME';"

# 2. 查询关联IP（只查不封！）
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT DISTINCT SUBSTR(ip, 1, INSTR(ip, ',') - 1) as real_ip, tags, created_at 
   FROM request_logs WHERE username='$USERNAME' ORDER BY created_at DESC;"

```

### 2.2 查询IP影响了哪些用户

```bash
IP="目标IP"

# 查询该IP添加过tag的所有用户
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT DISTINCT username, tags, created_at 
   FROM request_logs 
   WHERE SUBSTR(ip, 1, INSTR(ip, ',') - 1) = '$IP' 
   ORDER BY created_at DESC;"

# 查询这些用户的状态
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT DISTINCT u.username, u.status, u.last_modify, r.tags, r.created_at 
   FROM request_logs r 
   LEFT JOIN users u ON r.username = u.username 
   WHERE SUBSTR(r.ip, 1, INSTR(r.ip, ',') - 1) = '$IP' 
   ORDER BY r.created_at DESC;"
```

### 2.3 解封用户

```bash
# 恢复status为SUCCESS（立即生效）
sqlite3 /root/twitter/twitter.db \
  "UPDATE users SET status='SUCCESS' WHERE username='目标用户名';"

# 如果需要从bans.txt中删除IP：
# 1. 先找到IP所在行号
grep -n "IP地址" /root/twitter/bans.txt

# 2. 按行号删除（如第5行）
sed -i '5d' /root/twitter/bans.txt
```

---

## 三、查询流程

### 3.1 查询用户基本信息

```bash
# 查看用户状态
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT * FROM users WHERE username='目标用户名';"

# 查看用户tags
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT * FROM user_tags WHERE username='目标用户名';"
```

### 3.2 查询用户被哪些IP添加过tag

```bash
# 完整记录（IP + tags + 时间）
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT created_at, SUBSTR(ip, 1, INSTR(ip, ',') - 1) as real_ip, tags FROM request_logs WHERE username='目标用户名' ORDER BY created_at DESC;"

# 按IP和tags分组统计
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT DISTINCT SUBSTR(ip, 1, INSTR(ip, ',') - 1) as real_ip, tags, COUNT(*) as times FROM request_logs WHERE username='目标用户名' GROUP BY real_ip, tags ORDER BY times DESC;"

# 只查所有关联IP
sqlite3 /root/twitter/twitter.db \
  "SELECT DISTINCT SUBSTR(ip, 1, INSTR(ip, ',') - 1) FROM request_logs WHERE username='目标用户名';"
```

### 3.3 查询特定tags的用户

```bash
# 查找tags包含某关键词的用户
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT username, tags FROM user_tags WHERE tags LIKE '%关键词%';"

# 查找这些用户的IP
sqlite3 /root/twitter/twitter.db \
  "SELECT DISTINCT SUBSTR(r.ip, 1, INSTR(r.ip, ',') - 1) as ip, r.username FROM request_logs r INNER JOIN user_tags t ON r.username = t.username WHERE t.tags LIKE '%关键词%';"
```

### 3.4 查询所有NAN tag的requests

```bash
# 查询所有包含NAN tag的请求记录
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT id, username, tags, SUBSTR(ip, 1, INSTR(ip, ',') - 1) as real_ip, created_at 
   FROM request_logs WHERE tags LIKE '%NAN%' ORDER BY created_at DESC;"

# 查询包含NAN tag的用户及状态
sqlite3 -header -column /root/twitter/twitter.db \
  "SELECT DISTINCT r.username, u.status, u.last_modify, r.tags, r.created_at 
   FROM request_logs r 
   LEFT JOIN users u ON r.username = u.username 
   WHERE r.tags LIKE '%NAN%' 
   ORDER BY r.created_at DESC;"
```

---

## 四、故障排查

### 4.1 访问用户json.gz返回404

```bash
# 检查banned.json是否存在
ls -lh /root/twitter/banned.json

# 检查用户文件是否存在
ls -lh /root/twitter/用户名.json.gz

# 检查用户status
sqlite3 /root/twitter/twitter.db "SELECT username, status FROM users WHERE username='用户名';"

# 如果用户不在users表，添加
sqlite3 /root/twitter/twitter.db "INSERT OR IGNORE INTO users (username, status) VALUES ('用户名', 'SUCCESS');"
# 不需要重启，status修改立即生效
```

---

## 五、关键文件说明

| 文件 | 说明 |
|------|------|
| `/root/twitter/bans.txt` | IP封禁列表，支持单IP和CIDR，#开头为注释 |
| `/root/twitter/banned.json` | 返回给BANNED用户的JSON文件 |
| `/root/twitter/twitter.db` | SQLite数据库 |
| `/root/twitter/caller.out` | 用户数据抓取日志 |
| `/root/twitter/twitter.bin` | 主服务二进制文件 |

### 数据库表结构

```
users:         username, nick, status, last_modify
user_tags:     username, tags, last_modify
request_logs:  id, username, tags, ip, ua, created_at
```

### status 状态值

| 状态 | 说明 |
|------|------|
| SUCCESS | 正常用户，返回实际数据 |
| BANNED | 被封禁用户，返回banned.json |
| FAILED | 抓取失败，返回banned.json |
| 其他非SUCCESS | 同样返回banned.json |

---

## 六、快速命令速查

```bash
# 封禁一个用户（不需要重启）
# 1. 查询关联IP
U="用户名"; sqlite3 -header -column /root/twitter/twitter.db "SELECT DISTINCT SUBSTR(ip,1,INSTR(ip,',')-1) as ip, tags, created_at FROM request_logs WHERE username='$U' ORDER BY created_at DESC;"
# 2. 设置status为BANNED（立即生效）
sqlite3 /root/twitter/twitter.db "INSERT OR IGNORE INTO users (username,status) VALUES ('$U','BANNED'); UPDATE users SET status='BANNED' WHERE username='$U';"

# 查看某用户的全部信息
U="用户名"; echo "=== users ==="; sqlite3 -header -column /root/twitter/twitter.db "SELECT * FROM users WHERE username='$U';"; echo "=== user_tags ==="; sqlite3 -header -column /root/twitter/twitter.db "SELECT * FROM user_tags WHERE username='$U';"; echo "=== request_logs ==="; sqlite3 -header -column /root/twitter/twitter.db "SELECT created_at, SUBSTR(ip,1,INSTR(ip,',')-1) as ip, tags FROM request_logs WHERE username='$U' ORDER BY created_at DESC;"; echo "=== json.gz ==="; ls -lh /root/twitter/${U}.json.gz 2>&1
```

