# B1 后端骨架 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 搭出一个能在本机启动的空后端：配置加载与启动自检、结构化日志、PostgreSQL 迁移（核心表）、`/healthz` 与 `/readyz`，并带本地任务脚本与 CI。

**Architecture:** 模块化单体，B1 只有 `cmd/api` 一个进程。`internal/config` 负责读环境变量并校验；`internal/platform/{db,redis}` 封装连接；`internal/httpx` 放中间件与统一响应；`internal/health` 放探活；`internal/server` 装配路由。迁移用 goose 嵌入 SQL，通过 `cmd/tools/migrate` 执行。

**Tech Stack:** Go（本机 1.26.2，`go.mod` 写 `go 1.22`）、Gin、`jackc/pgx/v5`、`pressly/goose/v3`、`redis/go-redis/v9`、`knadh/koanf/v2`、`joho/godotenv`、标准库 `log/slog`、go-task。

**Spec:** `后端技术设计文档（TRD）：PetBridge.md` v1.2（第 3、5、6、7.1、12、14、15 章、第 17 章 B1 行）；PRD v1.4 第 9 章数据模型。

## Global Constraints

- Go module 路径：`github.com/jinheng0301/petbridge`；`go.mod` 的 `go` 指令为 `1.22`。
- 目录结构以 TRD 第 5 章为准：`cmd/api`、`cmd/tools`、`internal/{config,platform,httpx,health,server}`、`db/migrations`、`api/openapi.yaml`、`deploy/`。
- 响应格式：成功 `{ "data": ... }`，失败 `{ "error": { "code", "message", "request_id" } }`；`/api/v1` 为业务路径前缀，探活路径 `/healthz`、`/readyz` 不带前缀。
- 日志用 `log/slog` JSON 输出；日志里不得出现请求体、密钥、数据库连接串里的密码。
- 配置只通过环境变量注入，本地用 `.env`（进 `.gitignore`），仓库只放 `.env.example`；缺必填项启动直接退出并列出缺哪些。
- 必填项：`DATABASE_URL`、`REDIS_URL`、`JWT_SIGNING_KEY`。`APP_ENV=production` 时自检：`DEV_LOGIN_ENABLED` 必须为 false、`AI_PROVIDER` 必须为 `live`、`JWT_SIGNING_KEY` 长度 ≥ 32，任一不满足拒绝启动。`APP_ENV` 不是 `local` 时 `DEV_LOGIN_ENABLED=true` 也拒绝启动。
- 本地环境（见记忆 `local-dev-environment`）：PostgreSQL `localhost:5432`，库 `petbridge`（开发）、`petbridge_test`（测试），用户/密码均为 `petbridge`；Redis 为手动启动的 Memurai，`127.0.0.1:6379`。**没有 Docker，不用 testcontainers**。
- 数据库测试用 `petbridge_test`；`testing.Short()` 时跳过数据库测试。`task test` = `go test -short ./...`，`task test-int` = `go test ./...`。
- Windows 无 `make`，任务入口用 go-task（`Taskfile.yml`，命令名与 TRD 15.5 的 make 命令一致）。
- 提交信息用 semantic commit（`feat:`、`test:`、`chore:`、`ci:`、`docs:`），每条以 `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>` 结尾。
- 本计划**不做**：`cmd/worker`（asynq 到 B5 才有任务可消费）、sqlc（到 B2 有第一批查询时再引入）、`compose.dev.yml`（不用 Docker）、业务接口。

## Review Focus

- `DATABASE_URL`、`JWT_SIGNING_KEY` 的值出现在配置校验错误或启动日志里（调试时最容易泄露）：错误信息只能出现变量名。
- 环境变量存在但为空串或纯空白：必须当作缺失，不能当成有效值通过。
- Redis 不可用但 PostgreSQL 正常：`/readyz` 返回 200 且标明 `degraded`（TRD 12.2：Redis 故障不得影响消息收发）；PostgreSQL 不可用才返回 503。
- 客户端传入的 `X-Request-ID` 含换行、超长或乱码：不得原样写进日志与响应头，改用服务端生成的 ID（防日志注入）。
- 迁移被重复执行、或执行到一半失败再重跑：`up` 必须幂等；`down` 能回到空库。
- 收到 SIGINT/SIGTERM 时仍有进行中的请求：先让请求完成再退出，超时 10 秒强制关闭。
- 端口被占用：进程以非零退出码退出并打印原因，不能静默挂起。

---

### Task 1: 仓库骨架与工具链文件

**Files:**
- Create: `go.mod`, `.gitignore`, `.env.example`, `README.md`

**Interfaces:**
- Produces: 可编译的空 module；`.env.example` 里的变量名是后续所有任务读取配置的唯一来源。

- [ ] **Step 1: 初始化 module**

Run: `go mod init github.com/jinheng0301/petbridge`，然后把 `go.mod` 的 `go` 指令改为 `1.22`。
Expected: `go.mod` 存在，`go build ./...` 无输出成功。

- [ ] **Step 2: 写 `.gitignore`**

忽略：`.env`、`*.exe`、`bin/`、`tmp/`、`*.db`、`coverage.out`、`.idea/`、`.vscode/`。**不要**忽略 `.env.example` 与 `docs/`。

- [ ] **Step 3: 写 `.env.example`**

内容取自后端 TRD 15.4 的 env 块，但：`DATABASE_URL` 为 `postgres://petbridge:petbridge@localhost:5432/petbridge?sslmode=disable`；新增 `TEST_DATABASE_URL`（指向 `petbridge_test`）；`S3_*` 整段保留但注释说明“B1 不使用，留空即可”；只保留 B1 会读取的变量加上 TRD 15.4 里标明将来使用的（AI/FCM/Sentry），后者值留空。

- [ ] **Step 4: 写 `README.md`**

只写：项目一句话、本地依赖（PostgreSQL 16、Memurai、Go、go-task）、首次启动步骤（Task 7 完成后才可用，先写占位标题），不复制 TRD 内容。

- [ ] **Step 5: Commit**

```bash
git add go.mod .gitignore .env.example README.md
git commit -m "chore: initialize go module and project scaffolding"
```

---

### Task 2: 配置加载与启动自检

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { AppEnv string; AppPort int; LogLevel string; DatabaseURL string; RedisURL string; JWTSigningKey string; AccessTokenTTL time.Duration; RefreshTokenTTL time.Duration; DevLoginEnabled bool; AIProvider string }`
  - `func Load(getenv func(string) string) (*Config, error)`：只读 `getenv`，不碰 `os.Environ`，便于测试。
  - `func LoadFromProcess() (*Config, error)`：若存在 `.env` 文件且 `APP_ENV` 为空或 `local`，先用 godotenv 载入（不覆盖已有环境变量），再 `Load(os.Getenv)`。
  - `type ValidationError struct { Problems []string }`，实现 `error`；`Problems` 每条只含变量名与原因，**不含变量值**。

- [ ] **Step 1: Write the failing tests**（表驱动，`getenv` 用 map 构造）

测试名与断言：
  - `TestLoad_Defaults`：只给三个必填项 → `AppEnv=="local"`、`AppPort==8080`、`LogLevel=="info"`、`AccessTokenTTL==15*time.Minute`、`RefreshTokenTTL==720*time.Hour`、`AIProvider=="fake"`、`DevLoginEnabled==false`。
  - `TestLoad_MissingRequired_ListsAllNames`：什么都不给 → 错误是 `*ValidationError`，`Problems` 同时包含 `DATABASE_URL`、`REDIS_URL`、`JWT_SIGNING_KEY` 三个名字。
  - `TestLoad_BlankIsMissing`：三个必填项分别设为 `""` 与 `"   "` → 都按缺失处理。
  - `TestLoad_ProductionSelfCheck`：`APP_ENV=production` 时分别触发 `DEV_LOGIN_ENABLED=true`、`AI_PROVIDER=fake`、`JWT_SIGNING_KEY` 31 位，各自被拒；32 位 + `AI_PROVIDER=live` + `DEV_LOGIN_ENABLED=false` 通过。
  - `TestLoad_DevLoginOnlyInLocal`：`APP_ENV=staging` 且 `DEV_LOGIN_ENABLED=true` → 被拒。
  - `TestLoad_ErrorNeverContainsSecrets`：`JWT_SIGNING_KEY="super-secret-value"`（过短且在 production）、`DATABASE_URL="postgres://u:hunter2@h/db"` 触发错误 → `err.Error()` 不含 `super-secret-value` 与 `hunter2`。
  - `TestLoad_InvalidValues`：`APP_PORT=abc`、`APP_PORT=70000`、`ACCESS_TOKEN_TTL=zzz`、`APP_ENV=prod`（非 local/staging/production）、`LOG_LEVEL=loud` 各自报错并点名变量。

- [ ] **Step 2: Run to verify it fails**

Run: `go test -short ./internal/config/...`
Expected: FAIL（`Load` 未定义）。

- [ ] **Step 3: Implement `Load` / `LoadFromProcess` / `ValidationError`**

用 koanf 的 env provider 读值（按 `.env.example` 的变量名），其余校验手写；先把所有问题收集进 `Problems` 再一次性返回，不要遇到第一个就退出。

- [ ] **Step 4: Run tests to verify pass**

Run: `go test -short ./internal/config/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config go.mod go.sum
git commit -m "feat: add config loading with startup self-check"
```

---

### Task 3: 日志与 HTTP 基础件

**Files:**
- Create: `internal/httpx/logging.go`, `internal/httpx/middleware.go`, `internal/httpx/response.go`, `internal/httpx/httpx_test.go`

**Interfaces:**
- Consumes: `config.Config.LogLevel`。
- Produces:
  - `func NewLogger(level string, w io.Writer) *slog.Logger`（JSON handler）。
  - `func RequestID() gin.HandlerFunc`：把 ID 放进 `c.Set("request_id", id)` 与响应头 `X-Request-ID`。
  - `func RequestIDFrom(c *gin.Context) string`。
  - `func AccessLog(l *slog.Logger) gin.HandlerFunc`：每个请求一行，字段 `method`、`path`、`status`、`duration_ms`、`request_id`；不记录 query string、请求体。
  - `func Recovery(l *slog.Logger) gin.HandlerFunc`：panic → 记录 → 500 + `INTERNAL`。
  - `func OK(c *gin.Context, status int, data any)`、`func Fail(c *gin.Context, status int, code, message string)`；常量 `CodeInternal = "INTERNAL"`、`CodeNotReady = "NOT_READY"`。

- [ ] **Step 1: Write the failing tests**

  - `TestRequestID_AcceptsValidInbound`：入站 `X-Request-ID: abc12345-DEF_67` → 响应头原样返回。
  - `TestRequestID_ReplacesUnsafeInbound`：入站值含 `\n`、长度 65、含空格、为空 → 响应头是服务端生成的新 ID（匹配 `^[A-Za-z0-9_-]{8,64}$` 且不等于入站值）。
  - `TestFail_Envelope`：`Fail(c, 503, "NOT_READY", "x")` → JSON 为 `{"error":{"code":"NOT_READY","message":"x","request_id":"<同响应头>"}}`。
  - `TestOK_Envelope`：`OK(c, 200, map[string]string{"a":"b"})` → `{"data":{"a":"b"}}`。
  - `TestAccessLog_NoQueryNoBody`：请求 `/x?token=secret` 带 body → 日志输出（bytes.Buffer）里不含 `secret` 与 body 文本，含 `"path":"/x"` 与 `"status"`。
  - `TestRecovery_Returns500Envelope`：handler panic → 500，错误码 `INTERNAL`，进程不崩。

- [ ] **Step 2: Run to verify it fails**

Run: `go test -short ./internal/httpx/...`
Expected: FAIL（符号未定义）。

- [ ] **Step 3: Implement the interfaces above**

请求 ID 生成用 16 字节 `crypto/rand` 的十六进制；`Recovery` 的日志包含 panic 值与堆栈，但响应体不含。

- [ ] **Step 4: Run tests to verify pass**

Run: `go test -short ./internal/httpx/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/httpx go.mod go.sum
git commit -m "feat: add structured logging and http middleware"
```

---

### Task 4: PostgreSQL 与 Redis 连接封装

**Files:**
- Create: `internal/platform/db/db.go`, `internal/platform/redis/redis.go`, `internal/platform/db/db_test.go`, `internal/platform/redis/redis_test.go`

**Interfaces:**
- Consumes: `config.Config.DatabaseURL`、`RedisURL`。
- Produces:
  - `db.Open(ctx context.Context, url string) (*pgxpool.Pool, error)`：建池后立刻 `Ping`，失败返回错误（错误信息不含 URL 里的密码）。
  - `redis.Open(ctx context.Context, url string) (*goredis.Client, error)`：`redis.ParseURL` + `Ping`；同样不泄露密码。
  - 两个包都导出 `func Ping(ctx context.Context, x ...) error`：`db.Ping(ctx, pool)`、`redis.Ping(ctx, client)`，内部带 2 秒超时，供探活复用。

- [ ] **Step 1: Write the failing tests**

  - `TestOpen_Succeeds`（`-short` 跳过）：用 `TEST_DATABASE_URL` / 本地 Redis URL → 无错误，`Ping` 无错误。
  - `TestOpen_BadURL_ErrorHidesPassword`：`postgres://u:hunter2@127.0.0.1:1/db`（端口不可达）→ 返回错误且 `err.Error()` 不含 `hunter2`；Redis 同理用 `redis://:hunter2@127.0.0.1:1/0`。
  - `TestPing_RespectsContextTimeout`：对不可达地址，`Ping` 在约 2 秒内返回错误而不是无限阻塞。

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/platform/...`
Expected: FAIL（符号未定义）。

- [ ] **Step 3: Implement**

`pgxpool.ParseConfig` 后设 `MaxConns=20`（先给一个够用的值，不引入配置项）；错误用 `errors.New` 重新包装并只保留“连接失败/地址”信息，丢弃原始错误里可能含的 DSN。

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/platform/...`（需要本机 PostgreSQL 与 Redis 在运行）
Expected: PASS；加 `-short` 时只跑不依赖外部服务的两个用例。

- [ ] **Step 5: Commit**

```bash
git add internal/platform go.mod go.sum
git commit -m "feat: add postgres and redis connection helpers"
```

---

### Task 5: 数据库迁移（核心表）

**Files:**
- Create: `db/migrations/0001_core_schema.sql`, `db/migrations/embed.go`, `internal/platform/db/migrate.go`, `internal/platform/db/dbtest/dbtest.go`, `internal/platform/db/migrate_test.go`, `cmd/tools/migrate/main.go`

**Interfaces:**
- Consumes: `db.Open`（Task 4）。
- Produces:
  - `package migrations`：`var FS embed.FS`（含 `*.sql`）。
  - `db.MigrateUp(ctx context.Context, databaseURL string) error`、`db.MigrateDown(ctx context.Context, databaseURL string) error`（回退一步）、`db.MigrateStatus(ctx, databaseURL string) (string, error)`。
  - `dbtest.Reset(t *testing.T) *pgxpool.Pool`：连 `TEST_DATABASE_URL`（缺省为本地 `petbridge_test`），`DROP SCHEMA public CASCADE; CREATE SCHEMA public;` 后跑 `MigrateUp`，`t.Cleanup` 关闭连接池；`testing.Short()` 时 `t.Skip`。
  - `cmd/tools/migrate`：`go run ./cmd/tools/migrate up|down|status`，从 `LoadFromProcess` 取 `DATABASE_URL`。

**迁移内容（`0001_core_schema.sql`，goose `-- +goose Up/Down`，`gen_random_uuid()` 为内置函数无需扩展）：**

| 表 | 列与约束（TRD 6.1/6.2、PRD v1.4 第 9 章） |
| :-- | :-- |
| `users` | `id uuid pk default gen_random_uuid()`；画像列 `nickname, native_lang, mbti, pet_type, age_band, tz` **可为 NULL**（登录时先建行，onboarding 才填，`onboarded_at timestamptz` 非空表示已完成）；`region text`；`mbti_changed_at timestamptz`；`status text not null default 'active' check in ('active','banned','deleted')`；`deleted_at`、`created_at timestamptz not null default now()`。CHECK：`native_lang in ('ja','ko','zh-Hant','zh-Hans','en')`、`age_band in ('13_17','18_plus')`、`mbti ~ '^[EI][NS][TF][JP]$'`（均允许 NULL） |
| `auth_identities` | `provider text check in ('apple','google','dev')`、`subject text`、`user_id uuid references users`；`unique (provider, subject)` |
| `refresh_tokens` | `id uuid pk`、`user_id`、`token_hash text unique not null`、`device_id text`、`expires_at`、`revoked_at`、`created_at` |
| `devices` | `id uuid pk`、`user_id`、`platform check in ('ios','android')`、`push_token`、`locale`、`widget_installed bool not null default false`、`last_seen_at` |
| `rooms` | `id uuid pk`、`owner_id uuid not null unique references users`（每人一间自己的小屋）、`capacity int not null default 5 check (capacity > 0)`、`member_count int not null default 0`、`status text not null default 'open' check in ('open','full','closed')`、`created_at`；`constraint ck_member_cap check (member_count <= capacity)` |
| `room_members` | `id bigserial pk`、`room_id`、`user_id`、`seat_no smallint not null check (seat_no between 1 and 5)`、`role text check in ('owner','member')`、`joined_at`、`left_at`、`left_reason text check in ('left','kicked','dissolved')`；`check ((left_at is null) = (left_reason is null))`；`uq_member_active unique (room_id,user_id) where left_at is null`；`uq_seat_active unique (room_id,seat_no) where left_at is null` |
| `room_bans` | `pk (room_id, user_id)`、`banned_by uuid`、`created_at` |
| `invite_codes` | `code text constraint pk_code primary key check (code ~ '^[2-9A-HJ-NP-Z]{8}$')`、`room_id references rooms`、`issued_to uuid null references users`（种子码可空）、`source check in ('zero','signup','join_reward','owner_regen')`、`status not null default 'unused' check in ('unused','used','expired','void')`、`used_by`、`used_at`、`expires_at not null`、`issued_at default now()`；索引 `(room_id, status)`、`(issued_to)` |
| `reward_grants` | `pk (user_id, room_id)`、`granted_count int not null`、`created_at` |
| `invite_edges` | `id bigserial pk`、`inviter_id`、`invitee_id`、`code text unique references invite_codes`、`created_at` |
| `app_config` | `key text pk`、`value jsonb not null`、`updated_at` |

`messages`、`deliveries`、`checkins`、`room_progress`、`room_decor`、`reports`、`blocks`、`ai_usage_daily` 不在 B1，各自在用到它们的周次新增迁移（只增不改，向前兼容）。`Down` 必须按外键反向顺序 `DROP TABLE`。

- [ ] **Step 1: Write the failing tests**（都用 `dbtest.Reset(t)`）

  - `TestMigrateUp_Idempotent`：连续调用 `MigrateUp` 两次均无错误。
  - `TestMigrateDownThenUp_RoundTrip`：`MigrateDown` 后 `public` 里无业务表（查 `information_schema.tables`），再 `MigrateUp` 成功。
  - `TestRoomMembers_ActiveUniqueness`：同一 `(room,user)` 插两条在席记录 → SQLSTATE `23505`；把第一条设 `left_at=now(), left_reason='left'` 后再插新记录 → 成功。
  - `TestRoomMembers_SeatUniqueAmongActive`：同房间两个不同用户占同一 `seat_no` → `23505`；`seat_no=6` → `23514`（check）。
  - `TestRoomMembers_LeftReasonConsistency`：只有 `left_at` 无 `left_reason` → `23514`。
  - `TestRooms_MemberCapCheck`：`UPDATE rooms SET member_count = 6`（capacity 5）→ `23514`。
  - `TestRooms_OneRoomPerOwner`：同一 `owner_id` 建第二间 → `23505`。
  - `TestInviteCodes_FormatCheck`：`'DEVA2345'`、`'FULL2345'` 插入成功；`'ABC0O1I2'`（含 0/O/1/I）、`'SHORT'`、小写 `'deva2345'` → `23514`。
  - `TestUsers_ProfileColumnsNullableBeforeOnboarding`：只给 `id` 插入 users 成功；`native_lang='fr'` → `23514`。

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/platform/db/...`
Expected: FAIL（迁移与符号不存在）。

- [ ] **Step 3: Implement**

写 `0001_core_schema.sql`；`MigrateUp/Down/Status` 用 `pgx/v5/stdlib` 打开 `*sql.DB` 交给 goose（`goose.SetBaseFS(migrations.FS)`，方言 postgres）；`cmd/tools/migrate` 只做参数解析与调用。

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/platform/db/...`
Expected: PASS；再对开发库执行 `go run ./cmd/tools/migrate up`，输出表示已应用 0001。

- [ ] **Step 5: Commit**

```bash
git add db internal/platform/db cmd/tools go.mod go.sum
git commit -m "feat: add core database schema migration"
```

---

### Task 6: 探活接口与 API 进程

**Files:**
- Create: `internal/health/health.go`, `internal/health/health_test.go`, `internal/server/server.go`, `internal/server/server_test.go`, `cmd/api/main.go`

**Interfaces:**
- Consumes: `httpx.*`（Task 3）、`db.Ping`、`redis.Ping`（Task 4）、`config.Config`（Task 2）。
- Produces:
  - `health.Probe` 即 `type Probe func(ctx context.Context) error`。
  - `health.New(dbProbe, redisProbe Probe) *Handler`；`(*Handler).Healthz(c *gin.Context)`、`(*Handler).Readyz(c *gin.Context)`。
  - `server.NewRouter(l *slog.Logger, h *health.Handler) *gin.Engine`：挂 `RequestID → AccessLog → Recovery`，注册 `GET /healthz`、`GET /readyz`。
  - `server.Run(ctx context.Context, addr string, handler http.Handler, l *slog.Logger) error`：监听、收到 ctx 取消后 `Shutdown`（超时 10 秒），端口被占用时返回错误。

**行为（TRD 12.2、14）：**
- `/healthz`：进程活着即 200，`{"data":{"status":"ok"}}`，不检查依赖。
- `/readyz`：PostgreSQL 失败 → 503 + `Fail(NOT_READY)`；PostgreSQL 正常、Redis 失败 → **200**，`{"data":{"status":"degraded","redis":"down"}}`；两者都正常 → 200，`{"data":{"status":"ok"}}`。

- [ ] **Step 1: Write the failing tests**

  - `TestHealthz_AlwaysOK`：两个探针都返回错误，`/healthz` 仍 200。
  - `TestReadyz_AllUp`：→ 200，`status=="ok"`。
  - `TestReadyz_RedisDown_Degraded`：Redis 探针失败 → 200，`status=="degraded"`、`redis=="down"`。
  - `TestReadyz_DBDown_503`：DB 探针失败 → 503，`error.code=="NOT_READY"`。
  - `TestReadyz_ProbeTimeout`：探针阻塞 → `/readyz` 在 3 秒内返回（探针 ctx 带超时），不挂起。
  - `TestRouter_RequestIDOnEveryResponse`：任意路径（含 404）响应都有 `X-Request-ID`。
  - `TestRun_GracefulShutdown`：启动一个处理耗时 300ms 的请求，请求进行中取消 ctx → 该请求仍返回 200，`Run` 随后返回 nil。
  - `TestRun_PortInUse_ReturnsError`：先占住端口再 `Run` → 返回非 nil 错误且不阻塞。

- [ ] **Step 2: Run to verify it fails**

Run: `go test -short ./internal/health/... ./internal/server/...`
Expected: FAIL（符号未定义）。

- [ ] **Step 3: Implement**

`cmd/api/main.go` 装配顺序：`LoadFromProcess` → 失败则把 `ValidationError.Problems` 逐行打到 stderr 并 `os.Exit(1)` → `NewLogger` → `db.Open`（失败退出）→ `redis.Open`（失败只记 warn，仍启动，对应 TRD 降级策略）→ 用 `signal.NotifyContext(SIGINT, SIGTERM)` 调 `server.Run`。启动日志只记 `app_env`、`port`，不记任何连接串。

- [ ] **Step 4: Run tests to verify pass**

Run: `go test -short ./internal/health/... ./internal/server/...`
Expected: PASS

- [ ] **Step 5: 端到端手测**

Run: `go run ./cmd/api`（另开终端 `curl http://localhost:8080/readyz`）。
Expected: 200 `{"data":{"status":"ok"}}`；停掉 Redis 后再请求为 `degraded`；停掉 PostgreSQL 服务后为 503。

- [ ] **Step 6: Commit**

```bash
git add internal/health internal/server cmd/api go.mod go.sum
git commit -m "feat: add health endpoints and api server entrypoint"
```

---

### Task 7: 本地任务脚本、OpenAPI 初稿与 CI

**Files:**
- Create: `Taskfile.yml`, `scripts/dev-redis.ps1`, `scripts/smoke.ps1`, `api/openapi.yaml`, `.github/workflows/ci.yml`
- Modify: `README.md`

**Interfaces:**
- Consumes: 前面所有任务的命令行入口。
- Produces: `task` 命令集合：`up`（启动 Redis，检查 PostgreSQL 服务在运行）、`down`（停掉 Redis 进程）、`migrate`、`dev`（`go run ./cmd/api`）、`test`（`go test -short ./...`）、`test-int`（`go test ./...`）、`smoke`、`lint`（`go vet ./...`）。

- [ ] **Step 1: 写 `scripts/dev-redis.ps1`**

行为：若 `memurai-cli ping` 已返回 `PONG` 则什么也不做并提示“已在运行”；否则按记忆 `local-dev-environment` 里的参数后台启动 `memurai.exe`，轮询至多 5 秒直到 `PONG`，失败则非零退出。

- [ ] **Step 2: 写 `Taskfile.yml`**

任务按上面 Interfaces 的命令集合；`up` 调 `dev-redis.ps1` 并用 `Get-Service postgresql-x64-16` 检查数据库服务状态；`migrate` 调 `go run ./cmd/tools/migrate up`。

- [ ] **Step 3: 写 `scripts/smoke.ps1`**

请求 `/healthz` 与 `/readyz`，任一非 200 则非零退出；脚本头部注明“B2 起扩展为 登录 → onboarding → 入房 → 发消息”。

- [ ] **Step 4: 写 `api/openapi.yaml`（OpenAPI 3.0 初稿）**

只包含 `/healthz`、`/readyz` 两条路径、统一错误响应 schema（`error.code/message/request_id`），顶部 `info.version: 0.1.0`；不引入 `oapi-codegen`（B2 有第一批真实接口时再接入）。

- [ ] **Step 5: 写 `.github/workflows/ci.yml`**

触发：push 与 pull_request 到 `main`。一个 job：`ubuntu-latest`，`services` 起 `postgres:16`（库 `petbridge_test`，用户/密码 `petbridge`）与 `redis:7`；步骤：checkout → `actions/setup-go`（`go-version-file: go.mod`）→ `go vet ./...` → `go test ./...`；环境变量 `TEST_DATABASE_URL` 指向 service。

- [ ] **Step 6: 更新 `README.md` 首次启动步骤**

`task up` → `task migrate` → `task dev` → 另一个终端 `task smoke`；注明 Redis 重启电脑后需要重新 `task up`。

- [ ] **Step 7: Verify**

Run: `task up && task migrate && task test-int && task lint`，然后 `task dev` 与另一终端 `task smoke`。
Expected: 全部成功，smoke 输出两个 200。

- [ ] **Step 8: Commit**

```bash
git add Taskfile.yml scripts api .github README.md
git commit -m "chore: add task runner, openapi draft and ci workflow"
```

---

## Self-Review

- **Spec coverage（TRD 第 17 章 B1 行）**：仓库骨架（T1）、CI（T7）、OpenAPI 初稿（T7）、数据库迁移（T5）、配置与日志（T2、T3），产出“能部署的空服务”（T6 的 `cmd/api`）。TRD 15.4 的启动自检规则（必填项、production 三条、`DEV_LOGIN_ENABLED` 仅 local）在 T2 全覆盖；15.5 的 `/healthz`、`/readyz` 在 T6；6.1/6.2 的表与约束（含 v1.2 新增的 `room_bans`、`left_reason`、`owner_regen`、`widget_installed`）在 T5。
- **有意不做并已写明**：`cmd/worker`、sqlc、`compose.dev.yml`、业务接口、`messages` 等后续周次表。
- **一处 TRD 没写清、本计划做的决定**：`users` 行在登录时创建，画像列可空，`onboarded_at` 标记完成。TRD 7.1 写“onboarding 事务里写 users”，但 `auth_identities.user_id` 需要先有用户行，两者矛盾；B2 实现登录时以此为准。
- **类型一致性**：`db.Open` / `db.Ping` / `redis.Open` / `redis.Ping`、`httpx.OK/Fail/RequestID/AccessLog/Recovery`、`health.Probe`、`server.NewRouter/Run`、`config.Load/LoadFromProcess/ValidationError` 在各任务的 Interfaces 与后续引用中名称一致。
- **Review Focus 覆盖**：密码不泄露（T2、T4 测试）、空白视为缺失（T2）、Redis 降级与 DB 503（T6）、X-Request-ID 注入（T3）、迁移幂等与回退（T5）、优雅关闭（T6）、端口占用（T6）七条均有对应测试。
