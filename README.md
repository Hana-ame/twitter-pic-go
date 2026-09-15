# twitter-pic-go

Twitter 媒体抓取与图库浏览。

## 组件

| 组件 | 路径 | 说明 |
|------|------|------|
| TCP 服务器 | `caller.py` | 接收 Go 端发来的 username，调用 `get2.py` 抓取元数据 |
| 队列处理 | `deamon.py` | 从 pending 文件读取 URL，翻译成抓取命令，排队执行 |
| Go API | `twitter_handlers.go` | REST API：创建/查询元数据、标签管理、Emoji 投票 |
| 图库 | `gallery/` | 直接服务 HTML 的图站后端，读取 json.gz 渲染媒体列表 |
| 标签存储 | `tags/tags.go` | **账号标签的唯一实现**：`account_tags` 读写 + `request_logs` 流水，Go API 与 gallery 共用同一份语义 |
| IP 封禁 | `ipban/ipban.go` | **封禁的唯一实现**：bans.txt 编译 trie + 链上任一命中 + 热重载 + 统一 IP 口径，Go API 与 gallery 共用同一个单例 |
| twimg 反代 | `twimg/main.go` | 反向代理 pbs.twimg.com 图片 |

## 标签（账号级）的存储

**只有一个库、只有一张表**：`twitter.db` 的 `account_tags(username, tag, weight)`。

- 两层入口共用 `tags` 包：Go API 的 `GET /api/twitter/tags/:username` /
  `POST /api/twitter/:username` / `GET /api/twitter/?by=tag&search=<tag>`，与 gallery 的
  `GET /api/tags?keys=` / `POST /api/tag`（别名 `/api/account-tags`、`/api/account-tag`）。
- 写语义：见下一节「一 IP 一票」——请求里的 `d` 是**该 IP 的目标值**，不是变化量；
  `account_tags.weight` 是**物化滚动值** = 历史底数 + Σ票；恰好归零删行、负权重保留；
  **每次写请求记一行 `request_logs`**（username / tags / ip / ua，两层都记，幂等的是
  权重不是审计）。
- 写失败**报 500**，不再吞掉错误回 `200 {"message":"ok"}`；gallery 侧对应 400/500/503。
  抓取排队（`curlMetaData`）失败不算写失败，只记日志——否则 caller.py 不在时会把
  一次成功的标签写入判成失败。
- 读语义：直接按 `account_tags` 现场聚合，不再读旧的 `user_tags` JSON 大字段，
  也不再有独立的 `tags.db` 快照或 `account_votes.json` 投票文件。
  两层的过滤口径**逐字一致**：`weight != 0` 都返回（含负权重）、`weight = 0` 都不返回。
  根 API 侧由 `userSelectQuery` 的 `AND a.weight != 0` 保证，与 `tags.Store.Weights` 对齐，
  并有测试钉住（`sql_tags_readparity_test.go`）——否则库上一旦出现零权行（外部 sqlite3
  运维直写、历史脏数据），同一账号两层会给出不同标签集。
- `by=tag` 的反查只取**正权重**、按权重降序、精确匹配（非 LIKE）、`status='SUCCESS'`、
  `LIMIT 15`；空结果返回 `[]` 而不是 `null`。
- gallery 的库路径由 `GALLERY_DB` 指定，默认 `./twitter.db`——与 Go API 同一个文件。
- 旧的 `user_tags` 表只作为历史遗留存在，服务端启动时一次性回填进
  `account_tags`（`migrateAccountTags`），此后不再作为数据源被读取。

## 一 IP 一票（标签计票契约）

**给客户端看的就这一节。** 两个入口同一个模型，只是包壳不同。

### 请求与语义

| 入口 | 方法/路径 | 请求体 | `d`/value 的含义 |
|---|---|---|---|
| 网页端（图站） | `POST /api/tag`（别名 `/api/account-tag`） | `{"user":"<账号>","tag":"<标签>","d":1}` | **该访客的目标值**：`1` 投 / `-1` 减 / `0` 撤票。`d` **必填** |
| App（根 API） | `POST /api/twitter/:username?do_not_renew=true` | `{"<标签>":1,"<另一个>":-1}` | 同上，按标签逐项给目标值；`0` = 撤掉该 IP 在这个标签上的票 |

### 客户端最短路径：发「完整期望态」，不要发差分

**这条决定另外两个客户端怎么写。** 客户端**读不到**"我这个 IP 之前投了什么"
（它只看得见全站聚合权重），所以任何"差分/增量"式的实现都必须自己记账
（localStorage 或内存里的上一次提交），一丢状态就错。而"目标值 = 我这个 IP 想要的值"
这个语义**不要求它知道**：撤销就是发 `0`，服务端自己把该 IP 的票清掉。于是正确的实现是

> **一次提交 = 送一份 `{标签: -1 | 0 | +1}` 的完整期望态；取消一个标签 = 发 `0`，
> 不是把这个 key 删掉。**

- react 侧现有的形状**本来就是全量 map**（`TagSelectorModal.jsx` 把完整 `tagScores`
  交给 `App.jsx` 的 `handleConfirmTags`，再经 `createMetaData(username, tags, false, true)`
  打到 `POST /api/twitter/:username?do_not_renew=true`），所以它**不需要结构性重构**，
  要改的只有一处：`TagSelectorModal.jsx` 里 `if (nextScore === 0) delete newState[tag]`
  改成**保留 key、值为 0**。
- Flutter 侧同理：不要做差分，直接把 UI 上的期望态整体发出去。
- 服务端按 `(账号,标签,IP)` 替换该 IP 的票并**重算**滚动值，因此同一 IP 重复提交同一份
  期望态天然幂等——**客户端不需要记 diff，localStorage 也不参与任何判定**。

### 「键缺失」与「键为 0」是两件事（必须都能表达）

| 表达 | 含义 | 服务端行为 |
|---|---|---|
| map 里**没有**这个标签的 key | 这次不想碰它 | **不动**该标签的任何票（既有票与权重原样保留） |
| `{"<标签>":0}` | 我要撤掉**我自己**在这个标签上的票 | 删掉 `(账号,标签,该IP)` 的票行并重算权重 |

两者缺一不可：只有"缺 key = 不动"，客户端才能只改一个标签而不影响其它标签；
只有"值为 0 = 撤票"，客户端才能表达"取消"而不必知道历史。
图站那条是**逐标签**的 target API（`d` 必填、`d=0` 即撤票），所以它没有"缺 key"这一维，
但也因此**不能省 `d`**（缺 `d` 是 400，不是"不动"）。

一条票的键是 **(账号, 标签, IP)**，IP 取 `ipban.Principal`（与限流分桶、
`request_logs.ip` **同一个值**）。所以：

- **幂等**：同一 IP 对同一 (账号,标签) 连发 100 次 `d:1`，权重只动一次。
  服务端不读任何 cookie / localStorage，换浏览器、开无痕都只是重复投同一张票。
- **反向改票是一笔**：`+1 → -1` 对权重的影响是 **±2**（旧实现把输入夹到 ±1，
  于是"从减分改成加分"会被夹成 0 分——那个死结就是这次改语义要消掉的）。
  夹取的位置在**目标值**上，不在输入差值上。
- **归一化差别**：图站对越界的 `d`（`2`、`-5`…）直接 **400**（一个把 `d` 当分数发的
  客户端是有 bug 的，静默夹成 1 会让它以为投了 5 票）；根 API 保留原有的归一化到 ±1
  （它一次收一组标签的批量体，为一个越界值退回整批不友好）。
- **`d` 缺失**：图站 400（`0` 现在是有效值，不能拿缺省值当撤票）。

### 样例：同一个访客从 `+1` 改成 `-1`（差值 ±2 是合法的）

底数 `tag_weight_base('A','女性') = 37`，访客 IP 为 `183.34.64.0`：

| # | 客户端发的 | 该 IP 的票行 `tag_votes.value` | `account_tags.weight` | 本次对权重的影响 |
|---|---|---|---|---|
| 0 | —（迁移后的初始态） | 无行 | `37` | — |
| 1 | `{"user":"A","tag":"女性","d":1}` | `+1`（新建） | `38` | `+1` |
| 2 | `{"user":"A","tag":"女性","d":1}`（重复提交） | `+1`（**连 updated_at 都不动**） | `38` | `0`（幂等） |
| 3 | `{"user":"A","tag":"女性","d":-1}`（反向改票，**一笔**） | `-1`（替换） | `36` | **`-2`** |
| 4 | `{"user":"A","tag":"女性","d":0}`（撤票） | 行被删除 | `37` | `+1` |

另一个访客投 `+1` 时，第 3 步的影响依然是 `-2`（权重 = 底数 + Σ所有票）。
上面这张表就是端到端实测跑过的那条链（见下节兼容实验同批验证）。

### 响应与错误码

- 图站成功：`200 {"user":"<账号>","tags":{<标签>:<权重>, …}}` —— **该账号的权威全量**
  （含负权重、不含归零行）。客户端应整份覆盖本地缓存，**不要**自己按 ±1 累加显示值。
- App 成功：`200 {"message":"ok"}`（形态未变；要看权威值用 `GET /api/twitter/tags/:username`）。
- 图站守卫阶梯（顺序即优先级）：封禁 IP `403` → 库不可写 `503` → 配额超限 `429` →
  入参非法 `400` → 目标账号被封 `404` → 写失败 `500` → 成功 `200`。
  4xx 全部**不落库、不落流水**。`429` 按 IP 计（默认 25 次/小时，`GALLERY_TAG_RATE_MAX`），
  与票数**分开计数**：同一人发 3 次请求扣 3 点配额，但只有 1 票。
- 拿不到客户端 IP 时**拒绝写入**（500）而不是退化成空串：票桶键变成 `""` 会把全站
  访客塌成同一个桶，一 IP 一票当场变成"全站共一票"。

### 兼容性：两种错配都会**静默**出错（以下全部实测复现，非推理）

用 `9f1c8d0`（旧，累加语义）与改版后（新，目标值语义）两台服务端各起一个等位旧库形状
的现场（`女性` 底数 10、`COS` 底数 -3），互打对方的请求：

| 错配 | 现象（实测数字） | 是否报错 |
|---|---|---|
| **新客户端 + 旧服务端** | react 那份"完整期望态"被旧服务端当**增量累加**：同一份 `{"女性":1,"COS":1}` 连存 3 次 → `女性 10→11→12→13`，而 `COS` 从 `-3` 被一路"存"到 `-2→-1→`**恰好归零删行**（一个减分标签被反复保存刷到消失） | 图站发 `d:0` 撤票会撞上旧校验 **400**（响亮）；react 走根 API 全是 **200**（静默） |
| **旧客户端 + 新服务端** | ① 旧网页端"取消 +1"发的是差值 `d:-1` → 新服务端当目标值 → 权重 `10→9`：**取消变成了踩一票**。② 旧 react 端用"删 key"表达取消 → 该 IP 的旧票**撤不掉**：`COS` 停在 `-2` 不动。③ 反向改票拆两笔 `d:-1` → 第一笔生效、第二笔幂等空转（结果正确，属歪打正着） | 全部 **200**，静默 |

**由此得出两条发布硬约束**：
1. **服务端先行**，且 react 的「删 key 改成发 0」与它同批上线——因为两类 react 错配都是
   静默 200，灰度期没有任何报警会替你发现它。
2. 网页端反过来：新 `app.js` 打到旧服务端是**响亮 400**（撤票请求被拒），旧 `app.js`
   打到新服务端是**静默把取消变成踩票**。所以 `app.js` 与服务端**必须同一份产物**部署，
   不能分开热更新（静态文件与二进制分别发布时特别注意）。

顺带三点：
- 新服务端对旧客户端的**破坏是有界的**：幂等使"重复保存刷分"这条路被堵死（实测第 3 次
  保存权重不再变），只剩"取消撤不掉"与"取消变踩票"两处语义错。
- 底数不受任何错配影响（快照后不可变），所以**改回正确客户端后权重会自愈回真值**：
  滚动值每票重算，不累加。

### 读标签时不要把"读不到"当"没有标签"（react 必读）

实测三个响应形态（`GET /api/twitter/tags/:username`）：

| 情况 | 状态码 | body |
|---|---|---|
| 有标签 | 200 | `{"username":"legacy","last_modify":"…","tags":{"COS":-3,"女性":11},"status":"SUCCESS"}` |
| **有账号但零标签** | 200 | `{"username":"alice",…,"tags":{},…}` ← 是 `{}`，**不是 `null`** |
| **账号不存在** | **500** | `{"error":"查询用户失败: 没有进入 rows.Next()"}` ← **没有 `tags` 字段** |

react 的 `src/api/getTags.ts` **不检查 `res.ok`**（同目录的 `createMetaData.ts` 检查了），
所以第三行那个 500 会被 resolve 成一个没有 `tags` 的对象；再经 `App.jsx:206` / `App.jsx:247`
的 `data.tags || {}` 兜底，**"查无此人"和"任何一次 5xx/网络抖动"都会变成"这个账号没有标签"**。

在"完整期望态"语义下必须改掉：`getTags` 里 `if (!res.ok) throw`，调用方**不要把读失败
兜成空表**。今天它只是显示错误（缺 key = 不动，误提交的空表不会撤销任何人的票）；
但一旦哪天有人把"缺 key"实现成"清空该账号全部标签"，这个假空表就是一次删库操作。
**这条比"删 key 改成发 0"更该先做**——它决定误发时会不会造成不可逆后果。

### 存储与恒等式

```
tag_votes(username, tag, ip, value, updated_at)   PK(username,tag,ip) WITHOUT ROWID  ← 票账本（真源）
tag_weight_base(username, tag, base)              PK(username,tag)    WITHOUT ROWID  ← 历史底数，快照后不可变
account_tags(username, tag, weight)               PK(username,tag)    WITHOUT ROWID  ← 物化滚动值
```

**恒等式：`weight = IFNULL(base,0) + IFNULL(Σ votes.value, 0)`**（只对"被票系统管过"的行成立；
从没被投过的行没有底数，权重就是历史值）。现有权重在一次幂等迁移里整体快照成底数，
**不清零、不重算**。

票行生命周期：投 / 改 → upsert（值相同则连 `updated_at` 都不动）；撤票 → **删行**
（不留 `value=0` 的噪声行，且该 IP 之后仍可重投）；**归零删的是 `account_tags` 行，
票行必须保留**——删了票行，该 IP 以后再也投不了这个标签，权重也无法从底数重算。

一致性校验（随时可跑，两条都该返回 0 行）：

```sql
-- ① 不自洽的滚动值
SELECT a.username, a.tag, a.weight, IFNULL(b.base,0) AS base, IFNULL(v.s,0) AS votes
FROM account_tags a
LEFT JOIN tag_weight_base b ON b.username=a.username AND b.tag=a.tag
LEFT JOIN (SELECT username,tag,SUM(value) s FROM tag_votes GROUP BY username,tag) v
       ON v.username=a.username AND v.tag=a.tag
WHERE (b.username IS NOT NULL OR v.username IS NOT NULL)
  AND a.weight <> IFNULL(b.base,0) + IFNULL(v.s,0);

-- ② 有票却该有权重行而缺失的（孤儿票）
SELECT k.username, k.tag, IFNULL(b.base,0)+IFNULL(k.s,0) AS should_be
FROM (SELECT username,tag,SUM(value) s FROM tag_votes GROUP BY username,tag) k
LEFT JOIN tag_weight_base b ON b.username=k.username AND b.tag=k.tag
LEFT JOIN account_tags a ON a.username=k.username AND a.tag=k.tag
WHERE IFNULL(b.base,0)+IFNULL(k.s,0) <> 0 AND a.username IS NULL;
```

同一份 SQL 也编在代码里（`tags.InvariantCheckSQL` / `tags.OrphanVotesSQL`，
`Store.InvariantViolations()` 一次跑两条），并有测试证明它**真能抓到**绕过写路径的篡改。

规模：底数表与 `account_tags` 同行数（线上 2.3 万 → 约 1.0 MB）。票行按实测 ~57 B/行
（`WITHOUT ROWID`，含 `updated_at`）：账号×标签×1 个 IP = 2.3 万行 ≈ 2.3 MB；
×10 IP ≈ 14 MB；×100 IP ≈ 132 MB。涨到 GB 级需要"每个标签平均上百个不同访客投过"。
读路径**不查**票账本（只看 `account_tags`），所以计票对读性能零影响；票行只在写入时
按单行主键点查/聚合。

⚠️ `tag_votes.ip` 记**原始 IP（不哈希，用户已定）**，是隐私敏感表，导出/备份按
`bans.txt` 同等待遇。它也是一张"谁给谁投了什么"的关系表，能反查同一 IP 投过哪些账号。

## IP 封禁

**只有一个实现、一个实例**：`ipban/` 包（只依赖 `net/http` + `net/netip` + `go-iptrie`，
不吃 gin）。根 API 的 `StrictIPBanMiddleware`（`banManager.go`）与 gallery 的
`handlePostAccountTag` 各自薄薄包一层，调同一个 `ipban.Shared()`。

- **两条 IP 口径，别混用**：
  - `ipban.Chain(r)` / `Manager.Decide`：**封禁判定**。RemoteAddr host + XFF **全部**条目，
    **任一命中即封**。只看 XFF 首项会被 `X-Forwarded-For: <好人IP>, <被封IP>` 绕过。
  - `ipban.Principal(r)`：**「这个请求是谁」**，用于限流分桶与 `request_logs.ip`。
    退化顺序 **`CF-Connecting-IP` → XFF 从右往左第 `TRUSTED_PROXY_HOPS` 个 → RemoteAddr**。
    原先根 API 记整个 XFF 头串、gallery 记首项，两层流水根本对不上，现统一。
- **`CF-Connecting-IP` 优先**（`CF_CONNECTING_IP` 可关，默认开）：Cloudflare 总是**覆写**
  这个头，所以经 CF 进来的请求伪造不了它，比数 XFF 跳数可靠。
  ⚠️ 它可信的**前提**是「源站只允许 CF 回源」。只要存在绕过 CF 直连源站的通路
  （源站 IP 泄露、别的域名/端口直回源、IPv6 没纳入限制），这个头和 XFF 一样可被任意伪造，
  那时唯一可靠做法是**防火墙只放行 CF 网段**。bwh 的 ufw 是否已经这么做：**未验证**，
  所以这里是「写明依赖」，不是「已经安全」。
- **`TRUSTED_PROXY_HOPS` 默认 2**（CF + nginx 两跳）。判据——**两种配错都会错一格**：

  | nginx 行为 | 到达源站的 XFF | 真实客户端位置 | 该配 |
  |---|---|---|---|
  | `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for`（**追加**） | `<自报…>, <真实客户端>, <CF 边缘>` | 右数第 **2** | `2` |
  | 原样**透传**（没有那一行） | `<自报…>, <真实客户端>` | 右数第 **1** | `1` |

  本站实测为**追加**：线上 `request_logs.ip` 的形态是 `183.34.64.0, 104.22.109.48`
  （左为真实客户端、右为 CF 边缘段），故默认 2。
- **配错要能看见，不静默假绿**：启动时 `ipban.LogEffectiveConfig()` 打印实际生效口径与
  封禁表加载条数；链长撑不起配置跳数时归属会退化到「客户端可自报的最左项」，该退化按次
  计入 `xff-clamped`，并在每次热重载时由 `warnPrincipalAnomaly` 打日志（含来源分布）。
- **同一份实例**：`Shared()` 用 `sync.Once` 给出进程级单例，热重载协程（`BAN_RELOAD_MINUTES`
  默认 10 分钟）也只挂这一个。两份内存副本各自 reload 会出现「API 侧已封、gallery 侧没封」。
- 清单路径 `BANS_FILE`（默认 `bans.txt`）；文件缺失=空表放行（封禁清单丢了不该变成全站 403）；
  非法行跳过并告警，不因一行脏数据丢掉整张表。
- 403 响应体统一为 `ipban.Denied{error, reason, ip}`，两层逐字相同。

⚠️ 两个已知权衡：① `TRUSTED_PROXY_HOPS` 配错时 25/IP/h 配额仍可被伪造 XFF 换桶绕过
（封禁不受影响，它看整条链）；② 「链上任一命中」允许攻击者把别人的 IP 塞进 XFF
来陷害其被封——这是既有严格策略的固有权衡，收紧就得只查右数 N 项，代价是
真实 IP 不在链上时漏封。

## 被封账号的可见性

`users.status` 是唯一真源，判定口径**只有一条**：`status = 'SUCCESS'` 才算可见。

- 根 API：`GET /:fn` 对非 SUCCESS 回 200 + `banned.json` 占位（该文件缺失时退化 404）、
  账号列表与 `by=tag` 都带 `u.status = 'SUCCESS'`。
- 图站（gallery）：原先**只看磁盘上有没有 `<name>.json.gz`**，完全不看 status，
  实测置 BANNED 后根侧 `by=tag` 回 `[]` 而图站反查仍列出该账号。现已在全部读路径收口，
  统一 **404**（不确认「存在但被封」），细节与 URL 影响面见 `gallery/README.md`。
- 封禁生效时限：图站的 `users.status` 视图按 60s TTL 惰性刷新，不等重启进程。
- 降级方向是 **fail-open**（读不到 `users` 就不隐身）：把整站藏起来等于自我 DoS，
  而"谁被封"这件事本身没坏，坏的只是可见性——与 `bans.txt` 缺失=空表放行同向。

## 媒体来源

媒体文件（图片/视频）通过以下方式获取：

### 1. twimg 反代（内置）

`twimg/main.go` 是一个反向代理服务器，监听 `TWIMG_ADDR` 环境变量指定的地址。
它代理 `pbs.twimg.com` 的图片请求，同时支持 `video.twimg.com` 的 iframe 播放器。

gallery 通过 `GALLERY_MEDIA_BASE` 或 `TWIMG_ADDR` 环境变量来配置媒体 URL 前缀。

### 2. peerfs-proxy（WebRTC 方式）

`peerfs-proxy` 是一个独立的 ECH 代理节点，通过 WebRTC DataChannel 提供 twimg 媒体文件。
它运行在 `https://peerfs.moonchan.xyz/`，节点 ID 为 `twimg-proxy`。

**作用：** 当 twimg 反代无法直接访问（如网络限制）时，peerfs-proxy 通过 ECH 域前置加密连接到 `video-cf.twimg.com`，绕过封锁获取图片/视频，然后通过 WebRTC 传给浏览器。

**如何使用：**

```bash
# 浏览器打开 https://peerfs.moonchan.xyz/
# 连接信令 → 点击 twimg-proxy 节点 → 发送 read 请求读取文件

# 请求格式：
# {"type":"read","path":"twimg/media/xxx.jpg","offset":0,"size":-1,"reqId":"r1"}
# 响应：meta → 二进制数据块 → done
```

详细协议见 `peerfs-chat/README.md`。

## 数据流

```
Twitter API / X
    │
    ▼
get2.py (Python)  →  生成 <username>.json.gz
    │
    ├──→ gallery (图站浏览)
    │
    └──→ twimg 反代 / peerfs-proxy (获取实际图片/视频)
```