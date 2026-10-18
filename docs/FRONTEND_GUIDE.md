# Gallery 前端开发与频繁迭代指南 (`docs/FRONTEND_GUIDE.md`)

`gallery` 包采用 **Go SSR 首屏直出 + 原生零依赖模块化 CSS/JS + 磁盘实时热重载** 架构，专为**高频更新前端 UI、样式与交互**而设计。

---

## 1. 前端目录结构与职责划分

```text
gallery/
├── assets.go                    # 双模资源加载器（自动监控磁盘修改热更新 + //go:embed 单二进制兜底）
├── pages.go                     # 首页 (/) 与账号页 (/u/{account}) SSR 视图模型与模板渲染
├── templates/                   # HTML 模板目录（修改后保存即生效）
│   ├── partials.html            # 全局骨架：head（注入合并后的 AppCSS）、header（顶栏）、footer（底栏）
│   ├── home.html                # 首页模板：Hero 区、搜索工具栏、标签云、账号网格、屏蔽/Gay模式弹窗
│   └── account.html             # 账号页模板：Banner/头像、标签投票栏、媒体瀑布流、全屏灯箱与视频 HUD
└── static/                      # 静态资源目录（既可内联注入 HTML，也可通过 /static/* 直接访问）
    ├── css/                     # 模块化样式表（按 cssModuleOrder 顺序自动合并注入 <style>{{.AppCSS}}</style>）
    │   ├── base.css             # 全局变量 (:root)、Reset、顶栏导航、底栏、分页按钮、通用弹窗 (.modal-*)
    │   ├── home.css             # 首页 Hero、公告条、搜索栏、标签云 (.tagbar)、账号卡片 (.agrid / .acct)
    │   ├── account.css          # 账号页头图 (.ubanner / .uhead)、标签投票胶囊 (.atags)、瀑布流 (.grid / .card)
    │   └── lightbox.css         # 手机式全屏查看器 (.lb)、手势缩放舞台、右侧点赞栏、视频 HUD (.lb-vhud)、响应式断点
    ├── home.js                  # 首页交互逻辑（搜索、标签多选筛选、Gay模式、流式解压 .json.gz 渲染卡片首图）
    └── app.js                   # 账号页交互逻辑（瀑布流分页、标签投票、全屏手势灯箱、双指缩放、视频播放器控制）
```

---

## 2. 本地开发：零编译实时热更新工作流

`gallery/assets.go` 实现了基于文件修改时间（`mtime`）的自动热重载机制：

1. **启动统一服务**：
   ```bash
   make dev
   # 等价于：LISTEN_ADDR=:8080 GALLERY_DEV=1 GALLERY_FRONTEND_DIR=./gallery go run ./server
   ```
   （注：即使直接运行编译好的 `./server/server`，只要工作目录下存在 `./gallery/templates` 与 `./gallery/static` 且未显式设置 `GALLERY_DEV=0`，服务端也会自动开启磁盘热重载监控。）

2. **直接编辑前端文件**：
   - 修改 `gallery/static/css/*.css` 中的样式；
   - 修改 `gallery/templates/*.html` 中的 DOM 结构；
   - 修改 `gallery/static/home.js` 或 `gallery/static/app.js` 中的交互脚本。

3. **浏览器直接刷新（F5）**：
   - 服务端检测到磁盘上任意 `.html` / `.css` / `.js` 的 `mtime` 更新后，会在微秒级内自动重新解析模板、合并 `css/*.css`、读取 `home.js` / `app.js`，并**自动使首页 SSR 缓存（`homeCache`）失效重建**。
   - **全程无需重新执行 `go build`，也无需重启 Go 进程。**

---

## 3. 生产环境：前端秒级热部署（免编译 Go）

在生产服务器上（如 `vps.moonchan.xyz:~/twitter`），如果本次只修改了 Gallery 的前端模板、CSS 或 JS：

```bash
make deploy-fe
# 或直接执行：
./build.sh --frontend
```

该命令只会通过 SCP 将 `gallery/templates` 和 `gallery/static` 同步到服务器的 `~/twitter/gallery/` 目录。线上正在运行的 Go 服务会在下一次请求到来时自动检测文件变更并完成热重载，实现**零停机、免编译秒级更新前端**。

---

## 4. 新增 CSS 模块或修改 DOM 钩子的注意事项

1. **新增独立 CSS 文件**：
   - 在 `gallery/static/css/` 下新建 `.css` 文件后，在 `gallery/assets.go` 的 `cssModuleOrder` 切片中按优先级追加文件名即可自动纳入内联注入与热重载监控。
2. **SSR 与前端 JS 的数据桥梁（切勿破坏 ID）**：
   - **首页 (`home.html` ↔ `home.js`)**：
     - `#a-grid`（`data-base` 注入媒体代理域名）
     - `#a-data`（JSON 数组，含全部可见账号的 `{n: 账号名, t: 标签数组, u: 更新时间}`）
     - `#a-meta`（离线预烘焙缓存占位，默认 `{}`）
   - **账号页 (`account.html` ↔ `app.js`)**：
     - `#g-grid`（`data-slug`, `data-per`, `data-base`）
     - `#g-data`（该账号 `.json.gz` 的完整 `account_info` 与 `timeline` JSON）
     - `#g-atags-data`（该账号当前的 `{tag: weight}` 标签权重字典）
3. **测试栅栏**：
   - 修改 `home.js` 或 `app.js` 后，请运行 `make check-js`（调用 `node --check` 检查语法）以及 `make test`（校验 `tags/gay_tags_contract_test.go` 与 `gallery/` 集成测试）。
