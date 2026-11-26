# Skilld

> **个人多设备 AI Agent Skills 云端同步与高并发单机控制中心**（原生 Go + 静态编译 SQLite 单二进制 + Mac / Linux 轻量级守护进程）

Skilld 通过统一的云端控制面 + 每台设备上的无头 Daemon，帮助你盘点并同步 **macOS** 与 **Linux 开发机**上各个 Coding CLI（**Claude Code**、**OpenAI Codex**、**CodeBuddy**、**Cursor**、**Gemini CLI**、**WorkBuddy**、**`.agents/skills`**）的 Skills，并基于 **三方内容指纹（3-Way Hash）** 安全兼容本机外部或 AI 顺手修改造成的漂移（Drift）。

---

## 核心架构（原生 Go + 静态编译 SQLite 单机高并发骨架）

- **单二进制，无 PostgreSQL / Redis / ORM**：运行时只有这个 Go 进程和它内嵌的 SQLite，使用标准库 `net/http`、`database/sql` 与 `github.com/mattn/go-sqlite3`。根包 [`embed.go`](embed.go) 把前端（`packages/web/*`）和 SQL 迁移（`migrations/*.sql`）打进同一个可执行文件。这个二进制可以直接跑，也可以装进 [`Dockerfile`](Dockerfile) 跑；Docker 只是打包方式，不是数据库或别的中间件。
- **显式拆分 `ReadDB` 与 `WriteDB` 连接池**：
  - 底层开启：`PRAGMA journal_mode = WAL`、`PRAGMA synchronous = NORMAL`、`PRAGMA mmap_size = 134217728`（128 MiB）、`PRAGMA cache_size = -32000`（~32 MiB）、`PRAGMA wal_autocheckpoint = 0`。
  - `ReadDB`：连接串强制附带 `mode=ro&_query_only=on`，并发读连接池限制在 `4~6` 个（默认 `6`，可通过 `DB_READ_CONNS` 调节）。
  - `WriteDB`：锁定 `SetMaxOpenConns(1)` 并强制 `_txlock=immediate`，同时在 Go 层加 `sync.Mutex` 严格串行化所有写事务，彻底杜绝 `SQLITE_BUSY`。
- **热路径全内存化（零磁盘穿透）**：
  - `atomic.Pointer[ConfigSnapshot]` 无锁 Copy-on-Write 动态配置快照与 Token 索引表。
  - 路由状态（开关熔断、在途数、请求/错误/延迟统计）全部使用 `atomic` 维护。
  - 账号限流令牌桶、余额/额度预扣（`AdmitRequest` 预扣 -> `CommitReservation` 结算 / `RollbackReservation` 回滚）及设备无变更心跳摘要快判全部在进程内完成，按账号持有细粒度 `sync.Mutex`。
- **异步组提交落盘与优雅停机 Drain**：
  - 容量 `8192` 的带缓冲 Channel 接收扣费流水、访问审计日志、设备心跳更新时间等事件。
  - 后台单一写协程按 **每 `15ms` 或攒满 `64` 条** 合并成**单个 SQLite 事务**批量落盘。
  - 收到 `SIGINT` / `SIGTERM` 后先优雅关闭 HTTP 服务，再 **Drain 排空 `8192` 队列中所有剩余事件**落盘后才关闭数据库，防止丢数据。
- **容灾配置（可选 Litestream + Cloudflare R2）**：
  - 初始化显式设置 `PRAGMA wal_autocheckpoint = 0`，把 WAL checkpoint 控制权完全交给 Litestream，防止增量同步链断裂。
  - 配套 [`deploy/litestream/litestream.yml`](deploy/litestream/litestream.yml)（每 10 秒增量同步至 Cloudflare R2）与一键恢复脚本 [`deploy/litestream/restore.sh`](deploy/litestream/restore.sh)。

```text
+-------------------------------------------------------------------+
|  Web 控制面板 (1Password 风格三栏 UI + Engine & Quotas 监控)        |
|  - 设备矩阵 × Agent 状态 / 行级 Diff / API 额度与连接池监控           |
+-------------------------------------------------------------------+
                                  ^
                                  | HTTPS + Bearer Token
                                  v
+-------------------------------------------------------------------+
|  skilld-server (Single Go Binary, embed.FS + Static SQLite)       |
|  - Hot Path Memory: atomic ConfigSnapshot + per-account Mutex     |
|  - Async GroupCommit: chan(8192) -> 15ms / 64 batch -> WriteDB    |
|  - ReadDB (4..6 conns, mode=ro&_query_only=on)                    |
|  - WriteDB (MaxOpen=1, _txlock=immediate, Go sync.Mutex)          |
+-------------------------------------------------------------------+
         ^                                         ^
         |                                         | Optional DR
+--------------------------+             +--------------------------+
| Mac / Linux Agent CLI    |             | Litestream (sync: 10s)   |
| - ~/.claude/skills 等    |             | -> Cloudflare R2 / S3    |
+--------------------------+             +--------------------------+
```

---

## 快速构建与启动

两种部署跑的是同一个 `skilld-server`：前端和 SQL 迁移都已经嵌在二进制里。设备上的 Agent 仍然装在每台 Mac / Linux 上，不放进这个服务容器。

### 二进制

```bash
# 1. 编译单个自包含服务端二进制（前端与 SQL 迁移已通过 embed.FS 打包在内）
make build

# 2. 启动服务（数据持久化在 ./.data/skilld.db；未指定 Token 时自动生成至 ./.data/token）
export SKILLD_TOKEN="sd_$(openssl rand -hex 24)"
./bin/skilld-server

# 3. 浏览器打开
open http://127.0.0.1:3737
```

编译需要 CGO（`github.com/mattn/go-sqlite3`）。要拷到另一台 x86_64 Linux 上跑，可以用 `make build-linux-static`，构建机上需要 `x86_64-linux-musl-gcc`。

### Docker Compose

容器里的进程监听 `3737`，数据在命名卷 `skilld-data` 的 `/var/lib/skilld`。入口脚本先把数据目录交给用户 `skilld`（uid `3737`），再启动服务，这样卷是 root 创建的也能写。新建的数据库和 token 文件权限是 `0600`。

```bash
# 1. 准备环境变量（仓库根目录的 .env 已被 git 忽略）
cp deploy/docker/skilld.env.example .env
# 编辑 .env，填入 SKILLD_TOKEN；留空则首次启动自动生成

# 2. 构建并后台启动（等价于 make docker-up）
docker compose up -d --build

# 3. 浏览器打开
open http://127.0.0.1:3737
```

没有预设 token 时，到日志或数据卷里取自动生成的那一把：

```bash
docker compose logs skilld
docker compose exec skilld cat /var/lib/skilld/token
```

`docker compose down` 会停掉容器并保留数据卷。`docker compose down -v` 会连数据卷一起删掉。只构建镜像可以用 `make docker-build` 或：

```bash
docker build -t skilld:local .
docker run -d --name skilld -p 3737:3737 \
  -e SKILLD_TOKEN="sd_$(openssl rand -hex 24)" \
  -v skilld-data:/var/lib/skilld \
  skilld:local
```

二进制和容器读同一组变量：`SKILLD_TOKEN`、`DB_READ_CONNS`、`GITHUB_TOKEN`、`UPSTREAM_CHECK_INTERVAL_MIN`。容器内的 `HOST`、`PORT`、`DATA_DIR` 由 [`docker-compose.yml`](docker-compose.yml) 固定；想换宿主机端口，改 `.env` 里的 `SKILLD_PUBLISH_PORT`。

### Linux Systemd 生产部署与 Litestream 容灾（可选 CF R2）

```bash
# 安装二进制与配置文件
sudo install -m 0755 bin/skilld-server /usr/local/bin/skilld-server
sudo mkdir -p /etc/skilld /var/lib/skilld
sudo cp deploy/systemd/skilld.env.example /etc/skilld/skilld.env
sudo cp deploy/systemd/skilld-server.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now skilld-server

# （可选）启用 Litestream 每 10 秒增量同步至 Cloudflare R2
sudo cp deploy/litestream/litestream.yml /etc/skilld/litestream.yml
sudo cp deploy/systemd/litestream.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now litestream

# 一键灾备恢复（自动停服 -> 恢复校验 -> 原子替换 -> 重启）
sudo ./deploy/litestream/restore.sh --config /etc/skilld/litestream.yml --db-path /var/lib/skilld/skilld.db
```

### 可选项：Cloudflare Workers + D1 部署

项目保留了可选的 Serverless 入口（[`packages/server/src/entry-worker.mjs`](packages/server/src/entry-worker.mjs)）和 D1 适配层（[`packages/server/src/store/d1-store.mjs`](packages/server/src/store/d1-store.mjs)）：

```bash
npm run build:web
cd packages/server && npx wrangler d1 execute skilld-db --remote --file=./schema.sql && npx wrangler deploy
```

---

## 连接你的 Mac 和 Linux 开发机

```bash
# 1. 注册设备并执行首次扫描上报
node packages/agent/bin/skilld.mjs init \
  --server http://your-skilld-host:3737 \
  --token "你的-SKILLD_TOKEN" \
  --name "macbook-pro"

# 2. 前台或后台启动守护进程
node packages/agent/bin/skilld.mjs daemon
node packages/agent/bin/skilld.mjs service launchd   # macOS
node packages/agent/bin/skilld.mjs service systemd   # Linux
```

---

## 运行测试与本地体验 Demo

```bash
# 1. Go 全量单元测试 + Race 竞态检测（覆盖 SplitDB/PRAGMA、并发预扣、8192 组提交与优雅 Drain、全量 API 契约）
make test-go

# 2. 编译二进制并运行 Node 跨语言协议测试 + 可选 CF Worker 测试 + 真二进制多机 E2E 闭环测试
make build && npm test

# 3. 注入双机模拟数据体验面板
npm run demo
```
