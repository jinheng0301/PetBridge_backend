# B2 登录、令牌、onboarding 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 前端可以用 dev 登录拿 token、完成 onboarding 得到宠物 + 自己的小屋 + signup 码，并用 `task seed` 得到 6 个种子用户和零号码 `DEVA2345`；所有接口写进 `api/openapi.yaml`。

**Architecture:** 在 B1 骨架上新增三个包：`internal/auth`（JWT、refresh 轮换、登录 handler、鉴权中间件）、`internal/user`（onboarding 事务、`GET /me`、宠物映射）、`internal/invite`（验证码生成）。各业务包通过 `server.Module` 接口把路由挂到 `/api/v1`。SQL 用手写 pgx，不引入 sqlc。

**Tech Stack:** Go、Gin、pgx/v5、`golang-jwt/jwt/v5`（新增）、`getkin/kin-openapi`（新增，仅测试用）。

**Spec:** `docs/superpowers/specs/2026-11-14-b2-auth-onboarding-design.md`（下称"设计"）；上游：后端 TRD 7.1、8、13 章，PRD 5.1。

## Global Constraints

- **不要 `git commit`、不要 `git push`。** 用户审查后自己提交（见记忆 `feedback-no-auto-commit`）。直接在 `main` 上工作，不建分支、不开 PR。
- 响应格式：成功 `{ "data": ... }`，失败 `{ "error": { "code", "message", "request_id" } }`；业务路径前缀 `/api/v1`；探活路径 `/healthz`、`/readyz` 不带前缀。
- 日志与错误信息里不得出现：access/refresh token、`JWT_SIGNING_KEY`、`id_token`、请求体。
- 不修改 `db/migrations/0001_core_schema.sql`；表结构变更只走 `0002`。
- 本地环境：PostgreSQL `localhost:5432`（开发库 `petbridge`、测试库 `petbridge_test`，用户/密码均为 `petbridge`），Redis 为手动启动的 Memurai。没有 Docker。
- 数据库测试用 `dbtest.Reset(t)`（会清空整个测试库）；`testing.Short()` 时自动跳过。`task test` = `go test -short ./...`；`task test-int` 必须串行（`-p 1`），否则多个包同时重置同一个测试库会互相破坏。
- 邀请码字符集固定为 `23456789ABCDEFGHJKLMNPQRSTUVWXYZ`（32 个字符），8 位，与表约束 `^[2-9A-HJ-NP-Z]{8}$` 一致。
- 宠物 slug、错误码、HTTP 状态码以设计第 5.4、5.7 节为准，不得改名。

## Review Focus

按最可能踩到的顺序；每条对应的测试写在括号里的任务中。

1. **同一个 refresh token 并发使用**：两个请求同时刷新，只能有一个成功，另一个得到 `REFRESH_REUSED`，且该 token 所在 family 全部作废（Task 5）。
2. **dev 登录邮箱大小写与空白**：`" Dev-A@Local "` 与 `dev-a@local` 必须是同一个用户；空串、缺 `@` 返回 `VALIDATION_FAILED`（Task 5、6）。
3. **`Authorization` 头各种畸形写法**：缺失、`bearer x`（小写 scheme 允许）、`Bearer`（无 token）、`Basic xxx`、多余空格、过期、篡改、`alg=none`，一律 401 `UNAUTHORIZED`，不能 panic 或 500（Task 4、6）。
4. **onboarding 的 `tz` 与 `nickname` 边界**：`tz` 为空串或 `"Local"` 必须拒绝（`time.LoadLocation` 会把它们当合法值）；Windows 上没有系统时区库，必须内嵌 `time/tzdata`，否则 `Asia/Tokyo` 会被误判非法；昵称按字符（rune）计长度，纯空白、超 20 个字符、含 emoji 的昵称都要有明确结果（Task 7）。
5. **被封禁/已删除用户持有未过期的 access token**：`GET /me` 与 `POST /me/onboarding` 必须返回 `ACCOUNT_DISABLED`（403），不能因为 JWT 还有效就放行（Task 7）。
6. 请求体不是 JSON、字段类型错、超过 64 KB：返回 `VALIDATION_FAILED`（400），不是 500（Task 6、7）。

---

## File Structure

| 文件 | 职责 |
| :-- | :-- |
| `internal/config/config.go`（改） | 新增 `CodeTTLDays` |
| `db/migrations/0002_refresh_token_family.sql`（新） | `refresh_tokens` 加 `family_id`、`replaced_by` |
| `internal/invite/code.go`（新） | 邀请码生成 |
| `internal/user/pet.go`（新） | MBTI → 宠物 slug |
| `internal/user/onboarding.go`（新） | 入参校验、`Service.Onboard`、`Service.Me` |
| `internal/user/handler.go`（新） | `GET /me`、`POST /me/onboarding` |
| `internal/auth/token.go`（新） | access JWT 签发/校验，refresh 生成/哈希 |
| `internal/auth/service.go`（新） | 登录（找或建用户）、refresh 轮换、logout |
| `internal/auth/verifier.go`（新） | `IDTokenVerifier` 接口与 B2 的占位实现 |
| `internal/auth/middleware.go`（新） | Bearer 鉴权中间件 |
| `internal/auth/handler.go`（新） | 登录/刷新/登出路由 |
| `internal/httpx/response.go`（改） | 新增错误码常量与 `FailDetails` |
| `internal/server/server.go`（改） | `Module` 接口，`NewRouter` 接受 `...Module` |
| `cmd/api/main.go`（改） | 装配 auth、user 模块 |
| `cmd/tools/seed/main.go`（新） | 种子用户与 `DEVA2345` |
| `api/openapi.yaml`（改） | 补全所有 B2 接口 |
| `internal/server/contract_test.go`（新） | 路由与 openapi 文档逐条对账 |
| `Taskfile.yml`、`.github/workflows/ci.yml`（改） | `seed` 任务；集成测试串行 |
| `scripts/smoke.ps1`（改） | 登录 → onboarding → `GET /me` |

---

### Task 1: 配置项与依赖

**Files:**
- Modify: `internal/config/config.go`、`internal/config/config_test.go`、`.env.example`
- Modify: `go.mod`、`go.sum`

**Interfaces:**
- Produces: `config.Config.CodeTTLDays int`（环境变量 `CODE_TTL_DAYS`，默认 7，范围 1–365）。

- [ ] **Step 1: 写失败测试**，加到 `config_test.go`（沿用该文件已有的 `Load(getenv)` 测试写法）：
  - `TestLoad_CodeTTLDaysDefault`：未设置时 `CodeTTLDays == 7`。
  - `TestLoad_CodeTTLDaysInvalid`：`"0"`、`"366"`、`"abc"` 各自返回的 `ValidationError.Problems` 中包含 `CODE_TTL_DAYS`，且问题文本里不含所设的值。
- [ ] **Step 2: 运行确认失败**：`go test -short ./internal/config -run CodeTTL -v`，期望编译失败或 FAIL。
- [ ] **Step 3: 在 `Load` 中读取 `CODE_TTL_DAYS`**：用 `strconv.Atoi`，错误信息只写变量名与范围；`.env.example` 的"业务默认值"段已有 `CODE_TTL_DAYS=7`，确认保留即可。
- [ ] **Step 4: 加依赖**：`go get github.com/golang-jwt/jwt/v5 github.com/getkin/kin-openapi@latest`，然后 `go mod tidy`。
- [ ] **Step 5: 运行** `go test -short ./...`，期望全部 PASS。

---

### Task 2: 迁移 0002 与串行集成测试

**Files:**
- Create: `db/migrations/0002_refresh_token_family.sql`
- Modify: `internal/platform/db/migrate_test.go`、`Taskfile.yml`、`.github/workflows/ci.yml`

**Interfaces:**
- Produces: 表 `refresh_tokens` 新增 `family_id uuid not null`、`replaced_by uuid null`；索引 `idx_refresh_tokens_family(family_id)`。

- [ ] **Step 1: 写失败测试** `TestRefreshTokens_FamilyColumns`（加到 `migrate_test.go`）：先 `newUser`，再向 `refresh_tokens` 插入不带 `family_id` 的行，断言 SQLSTATE 为 `23502`（not null）；插入带 `family_id` 的行成功。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/platform/db -run RefreshTokens -v`，期望 FAIL（列不存在）。
- [ ] **Step 3: 写迁移**：goose 格式。Up 用 `alter table refresh_tokens add column family_id uuid not null default gen_random_uuid()`，随后 `alter column family_id drop default`（表在 B1 后是空的，无历史数据问题），再加 `replaced_by uuid`；Down 删索引与两列。
- [ ] **Step 4: 确认已有的 `TestMigrateDownThenUp_RoundTrip` 仍通过**。注意它断言一次 `MigrateDown` 之后库里没有业务表——加了 0002 后，一次 down 只回退 0002，该断言会失败。把测试改为循环 `MigrateDown` 直到 `MigrateStatus` 报 `current version: 0`，再做断言。
- [ ] **Step 5: 集成测试串行**：`Taskfile.yml` 的 `test-int` 改为 `go test -p 1 ./...`；打开 `.github/workflows/ci.yml`，把其中跑完整测试的 `go test` 命令同样加上 `-p 1`。
- [ ] **Step 6: 运行** `go test -p 1 ./...`，期望全部 PASS（需要本地 PostgreSQL 已启动）。

---

### Task 3: 邀请码生成与宠物映射

**Files:**
- Create: `internal/invite/code.go`、`internal/invite/code_test.go`、`internal/user/pet.go`、`internal/user/pet_test.go`

**Interfaces:**
- Produces: `invite.Alphabet string`（上面那 32 个字符）；`invite.NewCode() (string, error)`（`crypto/rand`，8 位）；`user.PetForMBTI(mbti string) (slug string, ok bool)`。

- [ ] **Step 1: 写失败测试**
  - `TestNewCode_FormatAndCharset`：生成 1000 个，每个都匹配 `^[2-9A-HJ-NP-Z]{8}$`。
  - `TestNewCode_NoObviousCollisions`：1000 个里互不相同。
  - `TestAlphabet_Has32DistinctChars`。
  - `TestPetForMBTI_All16`：设计 5.4 表中 16 条映射逐条断言，16 个 slug 互不相同；`PetForMBTI("XXXX")` 与 `PetForMBTI("intj")` 返回 `ok=false`。
- [ ] **Step 2: 运行确认失败**：`go test -short ./internal/invite ./internal/user -v`。
- [ ] **Step 3: 实现** `invite.NewCode`（每个字符用 `crypto/rand` 取模前要消除偏差：字符集长度 32 能整除 256，直接取低 5 位即可，不会有取模偏差）；`user.PetForMBTI` 用包级 `map[string]string`。
- [ ] **Step 4: 运行**同上命令，期望 PASS。

---

### Task 4: Access / refresh 令牌

**Files:**
- Create: `internal/auth/token.go`、`internal/auth/token_test.go`

**Interfaces:**
- Produces:
  - `auth.NewIssuer(key string, ttl time.Duration, now func() time.Time) *Issuer`
  - `(*Issuer).Sign(userID string) (token string, err error)`；`(*Issuer).Parse(token string) (userID string, err error)`；`(*Issuer).TTL() time.Duration`
  - 错误：`auth.ErrTokenInvalid`、`auth.ErrTokenExpired`（`Parse` 用 `errors.Is` 可判）
  - `auth.NewRefreshToken() (raw, hash string, err error)`；`auth.HashRefreshToken(raw string) string`（SHA-256，十六进制小写）

- [ ] **Step 1: 写失败测试**（`now` 用可控的假时钟）
  - `TestSignParse_RoundTrip`：`Parse(Sign("u1")) == "u1"`。
  - `TestParse_Expired`：时钟前进超过 ttl 后 `errors.Is(err, ErrTokenExpired)`。
  - `TestParse_TamperedSignature`、`TestParse_WrongKey`：`ErrTokenInvalid`。
  - `TestParse_AlgNoneRejected`：手工拼一个 `alg=none` 的 token，`ErrTokenInvalid`。
  - `TestParse_GarbageInputs`：`""`、`"a.b"`、`"a.b.c"`、超长字符串，均返回 `ErrTokenInvalid` 且不 panic。
  - `TestParse_WrongAlgHS512Rejected`：用同一密钥签的 HS512 token 也被拒绝（固定只接受 HS256）。
  - `TestNewRefreshToken`：两次结果的 raw 不同；`HashRefreshToken(raw) == hash`；raw 长度 ≥ 43（32 字节 base64url）；hash 为 64 个十六进制字符。
- [ ] **Step 2: 运行确认失败**：`go test -short ./internal/auth -v`。
- [ ] **Step 3: 实现**。JWT claims：`sub`、`iat`、`exp`；`jwt.WithValidMethods([]string{"HS256"})` 固定算法；`jwt.WithTimeFunc(now)` 使过期判断用注入的时钟；把库的过期错误映射成 `ErrTokenExpired`，其余映射成 `ErrTokenInvalid`。
- [ ] **Step 4: 运行**同上命令，期望 PASS。

---

### Task 5: 登录与刷新服务（数据库）

**Files:**
- Create: `internal/auth/service.go`、`internal/auth/service_test.go`
- Modify: `internal/httpx/response.go`（先加错误码常量，见下）

**Interfaces:**
- Consumes: Task 4 的 `Issuer`、`NewRefreshToken`、`HashRefreshToken`。
- Produces:
  - `httpx` 新增常量：`CodeUnauthorized="UNAUTHORIZED"`、`CodeValidationFailed="VALIDATION_FAILED"`、`CodeAccountDisabled="ACCOUNT_DISABLED"`、`CodeRefreshReused="REFRESH_REUSED"`、`CodeRefreshInvalid="REFRESH_INVALID"`、`CodeProviderNotConfigured="PROVIDER_NOT_CONFIGURED"`；以及 `httpx.FailDetails(c *gin.Context, status int, code, message string, details any)`（在 `error` 对象里多一个 `details` 字段）。
  - `auth.NewService(pool *pgxpool.Pool, iss *Issuer, refreshTTL time.Duration) *Service`
  - `type Tokens struct{ AccessToken, RefreshToken string; ExpiresIn int }`（`ExpiresIn` 为秒）
  - `type LoginResult struct{ Tokens; UserID string; Onboarded bool }`
  - `(*Service).LoginWithIdentity(ctx context.Context, provider, subject, deviceID string) (LoginResult, error)`
  - `(*Service).Refresh(ctx context.Context, rawRefresh string) (LoginResult, error)`
  - `(*Service).Logout(ctx context.Context, rawRefresh string) error`
  - 错误：`ErrAccountDisabled`、`ErrRefreshInvalid`、`ErrRefreshReused`

- [ ] **Step 1: 写失败的集成测试**（用 `dbtest.Reset`，假时钟可前进）
  - `TestLogin_CreatesUserAndIdentityOnce`：同一 `("dev","a@local")` 登录两次，`users` 与 `auth_identities` 各一行，两次 `UserID` 相同，`Onboarded=false`。
  - `TestLogin_ConcurrentFirstLoginCreatesOneUser`：16 个 goroutine 同时首次登录同一身份，最终 `users` 恰好一行，全部返回同一 `UserID`、无错误。
  - `TestLogin_BannedUserRejected`：把 `users.status` 改为 `banned`/`deleted` 后登录返回 `ErrAccountDisabled`。
  - `TestLogin_StoresOnlyTokenHash`：`refresh_tokens.token_hash` 等于 `HashRefreshToken(raw)`，且库里任何列都不含 raw 明文。
  - `TestRefresh_RotatesAndKeepsFamily`：刷新后得到新 token，旧行 `revoked_at` 非空且 `replaced_by` 指向新行，新旧 `family_id` 相同。
  - `TestRefresh_ReplayRevokesWholeFamily`：用旧 token 再刷新 → `ErrRefreshReused`；此后新 token 也不能用（`ErrRefreshReused` 或 `ErrRefreshInvalid` 均可，但必须是错误）。
  - `TestRefresh_ConcurrentSameToken`：同一 raw token 8 个 goroutine 同时刷新，恰好 1 个成功，其余全部 `ErrRefreshReused`。
  - `TestRefresh_ExpiredAndUnknown`：时钟超过 `refreshTTL` 后返回 `ErrRefreshInvalid`；随机字符串返回 `ErrRefreshInvalid`。
  - `TestRefresh_BannedUserRejected`：已发出 token 的用户被封后刷新返回 `ErrAccountDisabled`，且该 token 不再可用。
  - `TestLogout_RevokesFamilyAndIsIdempotent`：登出后刷新失败；对未知 token 登出返回 nil。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/auth -run 'Login|Refresh|Logout' -v`。
- [ ] **Step 3: 实现 `Service`**。要点（实现者无法自行得出的约定）：
  - 找或建用户：先查 `auth_identities`；没有就事务内 `insert users default values` + `insert auth_identities`，遇 `23505` 回滚并回读已存在的那条。
  - 刷新赢家判定：`UPDATE refresh_tokens SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>now() RETURNING id, user_id, family_id, device_id`；0 行时再按 hash 查：行存在且已作废 → 吊销该 family 全部未作废行并返回 `ErrRefreshReused`；行存在但已过期，或不存在 → `ErrRefreshInvalid`。新 token 的插入与旧行 `replaced_by` 的回填在同一事务内。
  - 过期判断一律用注入的 `now` 而不是数据库 `now()`，这样假时钟测试才成立（把 `now()` 参数化为 `$n`）。
  - `Logout` 吊销 family 内所有未作废行；找不到 token 返回 nil。登出之后再拿这个 token 刷新，会走"已作废"分支得到 `ErrRefreshReused`，这是预期行为。
  - `Onboarded` = `users.onboarded_at is not null`。
- [ ] **Step 4: 运行**同上命令，期望全部 PASS；再跑 `go test -p 1 ./...` 确认没有影响其他包。

---

### Task 6: 登录接口、鉴权中间件、路由装配

**Files:**
- Create: `internal/auth/verifier.go`、`internal/auth/middleware.go`、`internal/auth/handler.go`、`internal/auth/handler_test.go`
- Modify: `internal/server/server.go`、`internal/server/server_test.go`、`cmd/api/main.go`

**Interfaces:**
- Consumes: Task 5 的 `Service`、`httpx` 错误码。
- Produces:
  - `server.Module interface{ Routes(v1 *gin.RouterGroup) }`；`server.NewRouter(l *slog.Logger, h *health.Handler, mods ...Module) *gin.Engine`（`mods` 挂到 `/api/v1` 分组；原有两参数调用保持可用）。
  - `auth.IDTokenVerifier interface{ Verify(ctx context.Context, idToken string) (subject string, err error) }`；`auth.ErrProviderNotConfigured`；`auth.NotConfiguredVerifier{}` 恒返回该错误。
  - `auth.Middleware(iss *Issuer) gin.HandlerFunc`；`auth.UserID(c *gin.Context) string`。
  - `auth.NewHandler(svc *Service, devLogin bool, apple, google IDTokenVerifier) *Handler`，实现 `server.Module`。
  - 路由（均在 `/api/v1` 下）：`POST /auth/login/dev`（仅当 `devLogin`）、`POST /auth/login/apple`、`POST /auth/login/google`、`POST /auth/refresh`、`POST /auth/logout`。
  - 请求/响应：登录 body `{"email"}`（dev）或 `{"id_token"}`（apple/google）；refresh/logout body `{"refresh_token"}`；登录与刷新的 `data` 为 `{access_token, refresh_token, expires_in, user:{id, onboarded}}`；logout 成功返回 204 无 body。
  - 请求体上限 64 KB（用 `http.MaxBytesReader`），超限、非 JSON、字段类型错一律 `VALIDATION_FAILED` 400。

- [ ] **Step 1: 写失败测试**（`handler_test.go` 用 `httptest` + 真实 `dbtest.Reset` 数据库 + 假 verifier；`NewRouter` 带 `auth.Handler`）
  - `TestDevLogin_RouteAbsentWhenDisabled`：`devLogin=false` 时 `POST /api/v1/auth/login/dev` 返回 404。
  - `TestDevLogin_NormalizesEmail`：`" Dev-A@Local "` 与 `"dev-a@local"` 返回同一个 `user.id`；`{"email":""}`、`{"email":"nope"}` 返回 400 `VALIDATION_FAILED`。
  - `TestLogin_ResponseShape`：`data` 含 `access_token`、`refresh_token`、`expires_in`（等于 `ACCESS_TOKEN_TTL` 秒）、`user.id`、`user.onboarded=false`。
  - `TestAppleGoogle_NotConfigured`：两个路由返回 501 `PROVIDER_NOT_CONFIGURED`；空 `id_token` 返回 400。
  - `TestAppleGoogle_UsesVerifierSubject`：注入返回固定 subject 的假 verifier，登录成功且 `auth_identities.provider` 分别为 `apple`/`google`。
  - `TestRefresh_Endpoint`：成功轮换；旧 token 重放返回 401 `REFRESH_REUSED`；未知 token 返回 401 `REFRESH_INVALID`；被封用户返回 403 `ACCOUNT_DISABLED`。
  - `TestLogout_Returns204`：成功 204；之后刷新失败；未知 token 也是 204。
  - `TestMiddleware_RejectsBadAuthorizationHeaders`：表驱动，覆盖缺失、`Bearer`、`Bearer `、`Basic abc`、`Bearer a b`、过期 token、篡改 token、`alg=none`，期望全是 401 `UNAUTHORIZED`；`bearer <有效token>`（小写 scheme）通过。
  - `TestMiddleware_SetsUserID`：有效 token 通过后 `auth.UserID(c)` 等于 token 的 `sub`。
  - `TestBody_Oversized_And_NotJSON`：70 KB 的 body、`not json`、`{"email":123}` 均 400 `VALIDATION_FAILED`。
  - 响应与日志中不含 token：用一个 buffer 作 logger 输出，断言日志里不含刚签发的 access/refresh token 字符串。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/auth ./internal/server -v`。
- [ ] **Step 3: 实现**。dev 邮箱规范化 = `strings.ToLower(strings.TrimSpace(email))`，必须含 `@` 且长度 ≤ 254；`deviceID` 取可选请求头 `X-Device-ID`（≤ 128 字符，否则忽略）。中间件缺失/错误一律走 `httpx.Fail(..., 401, CodeUnauthorized, ...)`，不区分原因（不泄露细节）。
- [ ] **Step 4: 改 `cmd/api/main.go`**：用 `auth.NewIssuer(cfg.JWTSigningKey, cfg.AccessTokenTTL, time.Now)`、`auth.NewService(pool, iss, cfg.RefreshTokenTTL)`、`auth.NewHandler(svc, cfg.DevLoginEnabled, auth.NotConfiguredVerifier{}, auth.NotConfiguredVerifier{})`，传给 `server.NewRouter`。
- [ ] **Step 5: 运行** `go test -p 1 ./...`，期望全部 PASS；`go vet ./...` 无输出。

---

### Task 7: onboarding 与 `GET /me`

**Files:**
- Create: `internal/user/onboarding.go`、`internal/user/onboarding_test.go`、`internal/user/handler.go`、`internal/user/handler_test.go`
- Modify: `cmd/api/main.go`

**Interfaces:**
- Consumes: Task 3 的 `invite.NewCode`、`PetForMBTI`；Task 6 的 `auth.Middleware`、`auth.UserID`、`server.Module`、`httpx.FailDetails`。
- Produces:
  - `type OnboardingInput struct{ NativeLang, MBTI, Nickname, AgeBand, TZ string }`（JSON 字段 `native_lang`、`mbti`、`nickname`、`age_band`、`tz`）；`(OnboardingInput) Validate() []string` 返回不合法的**字段名**列表（空表示通过）。
  - `type Profile struct{ ID string; Nickname, NativeLang, MBTI, PetType, AgeBand, TZ *string; Onboarded bool }`
  - `type OnboardingResult struct{ User Profile; Pet struct{Type, MBTI string}; Room struct{ID string; MemberCount, Capacity int}; InviteCode struct{Code string; ExpiresAt time.Time} }`
  - `user.NewService(pool *pgxpool.Pool, codeTTLDays int, now func() time.Time) *Service`
  - `(*Service).Me(ctx, userID string) (Profile, error)`；`(*Service).Onboard(ctx, userID string, in OnboardingInput) (OnboardingResult, error)`
  - 错误：`user.ErrNotFound`（用户行不存在）、`user.ErrAccountDisabled`
  - `user.NewHandler(svc *Service, authn gin.HandlerFunc) *Handler`（实现 `server.Module`）；路由 `GET /me`、`POST /me/onboarding`，均挂 `authn`。
  - 包内 `import _ "time/tzdata"`。

- [ ] **Step 1: 写失败的校验单元测试**（`-short` 可跑）`TestValidate`，表驱动：
  - 合法样例（`ja`/`ENFP`/`"  Mimi "`/`18_plus`/`Asia/Tokyo`）返回空。
  - `native_lang` 取 `"fr"`、`""`；`mbti` 取 `"enfp"`、`"ENF"`、`"XXXX"`；`age_band` 取 `"12_under"`、`""`；`tz` 取 `""`、`"Local"`、`"Mars/Base"`；`nickname` 取 `"   "`、21 个字符、空串 —— 各自返回对应字段名。
  - 昵称长度按字符计：20 个汉字合法、21 个不合法；一个 emoji 计 1 个字符；合法昵称保存时已去首尾空白。
  - `Asia/Tokyo` 在 Windows 上也合法（依赖内嵌的 `time/tzdata`）。
- [ ] **Step 2: 写失败的集成测试**（`dbtest.Reset`；用户行直接 `insert into users default values` 造出）
  - `TestOnboard_CreatesProfileRoomSeatCode`：返回后 `users` 资料列与 `pet_type`（如 ENFP → `happy_shiba`）已写入、`onboarded_at` 非空；`rooms` 恰一行（`owner_id`=该用户、`member_count=1`、`capacity=5`）；`room_members` 恰一行（`seat_no=1`、`role='owner'`、`left_at is null`）；`invite_codes` 恰一行（`source='signup'`、`issued_to`=该用户、`status='unused'`、`expires_at` = `now + CodeTTLDays 天`、`code` 匹配 `^[2-9A-HJ-NP-Z]{8}$`）。
  - `TestOnboard_IdempotentIgnoresSecondBody`：第二次传不同的 `mbti`/`nickname`，返回与第一次完全相同（同一 room id、同一 code），库里仍只有 1 间房、1 个码，`users.mbti` 保持第一次的值。
  - `TestOnboard_ConcurrentCallsCreateOneRoom`：8 个 goroutine 同时 onboarding 同一用户，全部成功且返回同一个 room id，`rooms` 恰一行。
  - `TestOnboard_InvalidInputWritesNothing`：非法入参返回校验错误，`rooms`/`invite_codes` 行数为 0，`users.onboarded_at` 仍为空。
  - `TestOnboard_CodeCollisionRetries`：预先占用一个码并通过可注入的码生成函数先返回占用值、再返回新值（`Service` 上暴露未导出字段 `newCode func() (string, error)` 供测试替换），断言最终成功且使用了第二个值。
  - `TestOnboard_UnknownUserAndDisabledUser`：不存在的 `userID` → `ErrNotFound`；`status='banned'` → `ErrAccountDisabled`，且都不写任何行。
  - `TestMe_BeforeAndAfterOnboarding`：之前资料字段为 `nil`、`Onboarded=false`；之后字段齐全、`Onboarded=true`；被封用户 → `ErrAccountDisabled`。
- [ ] **Step 3: 写失败的 HTTP 测试**（`handler_test.go`，带真实中间件与数据库）
  - `TestMe_RequiresAuth`：无 token 401。
  - `TestOnboarding_HTTPFlow`：dev 登录 → `POST /me/onboarding` 200，响应含 `user`、`pet`、`room`、`invite_code`（`code` 为 8 位无连字符）→ `GET /me` 显示 `onboarded=true`。
  - `TestOnboarding_ValidationErrorListsFields`：多个字段非法时返回 400 `VALIDATION_FAILED`，`error.details.fields` 列出全部非法字段名。
  - `TestOnboarding_BadBody`：`not json`、70 KB body、`{"mbti":5}` 返回 400。
  - `TestDisabledUserWithValidToken`：封禁后凭未过期 access token 调 `GET /me` 与 `POST /me/onboarding` 均 403 `ACCOUNT_DISABLED`。
- [ ] **Step 4: 运行确认失败**：`go test -p 1 ./internal/user -v`。
- [ ] **Step 5: 实现 `Onboard`**，事务步骤按设计 5.3：`SELECT status, onboarded_at ... FOR UPDATE` 锁用户行 → 已 onboarding 则回读现有房间与未用 signup 码返回（不看 body）→ 否则校验 → UPDATE users → INSERT rooms → INSERT room_members → INSERT invite_codes。码插入用 `ON CONFLICT (code) DO NOTHING` 并检查影响行数，最多尝试 5 次（事务内一次唯一冲突会让事务失效，所以不能靠捕获 `23505` 重试）。校验放在拿到行锁之后、写入之前；幂等分支不校验 body。`Me` 与 `Onboard` 开头都检查 `status`（`banned`/`deleted` → `ErrAccountDisabled`）。
- [ ] **Step 6: 实现 handler**：解析 body（64 KB 上限）→ 调 service → 错误映射：校验失败 → 400 `VALIDATION_FAILED` + `details:{fields:[...]}`；`ErrAccountDisabled` → 403；`ErrNotFound` → 401 `UNAUTHORIZED`；其他 → 500 `INTERNAL`（日志记错误，不记 body）。
- [ ] **Step 7: 装配** `cmd/api/main.go`：`user.NewService(pool, cfg.CodeTTLDays, time.Now)`、`user.NewHandler(svc, auth.Middleware(iss))` 加入 `NewRouter`。
- [ ] **Step 8: 运行** `go test -p 1 ./...`，期望全部 PASS。

---

### Task 8: 种子数据工具

**Files:**
- Create: `cmd/tools/seed/main.go`、`cmd/tools/seed/seed.go`、`cmd/tools/seed/seed_test.go`
- Modify: `Taskfile.yml`

**Interfaces:**
- Consumes: `auth.Service.LoginWithIdentity`、`user.Service.Onboard`。
- Produces: `seed.Run(ctx context.Context, pool *pgxpool.Pool, now func() time.Time) error`（放在 `seed.go`，包名 `main`；测试在同包内直接调用）；`task seed`。

- [ ] **Step 1: 写失败的集成测试**
  - `TestRun_CreatesSixUsersAndZeroCode`：运行后 `users` 6 行且全部 `onboarded_at` 非空；设计 5.6 表里每个邮箱对应的 MBTI/母语/年龄段正确（A=ja/ENFP/18_plus … F=en/ESFP/13_17）；`DEVA2345` 存在、`source='zero'`、`status='unused'`、`issued_to is null`、`room_id` 是 `dev-a@local` 的小屋、`expires_at` 约 10 年后。
  - `TestRun_Idempotent`：连续运行两次，`users`=6、`rooms`=6、`invite_codes` = 6 个 signup 码 + 1 个 zero 码 = 7 行。
- [ ] **Step 2: 运行确认失败**：`go test -p 1 ./cmd/tools/seed -v`。
- [ ] **Step 3: 实现 `Run`**：对 6 个种子逐个调用 `LoginWithIdentity("dev", email, "")`（得到 `UserID`）再 `Onboard`（昵称 `Dev A`…`Dev F`，`tz` 依语言取 `Asia/Tokyo`、`Asia/Seoul`、`Asia/Taipei`、`Asia/Shanghai`、`Europe/London`、`Europe/London`）；然后 `insert into invite_codes(code, room_id, source, expires_at) values ('DEVA2345', <A 的房间>, 'zero', <now+10年>) on conflict (code) do nothing`。
- [ ] **Step 4: 写 `main.go`**：`config.LoadFromProcess()`；若 `cfg.AppEnv != "local"` 直接报错退出（种子数据绝不能进线上库）；连库后调用 `Run`，打印每个邮箱和 `DEVA2345`。`Taskfile.yml` 新增 `seed: go run ./cmd/tools/seed`。
- [ ] **Step 5: 运行** `go test -p 1 ./cmd/tools/seed -v` 期望 PASS；再对开发库实跑 `task migrate` 然后 `task seed` 两次，期望两次都成功、第二次不报错。

---

### Task 9: OpenAPI 补全、契约对账、冒烟脚本

**Files:**
- Modify: `api/openapi.yaml`、`scripts/smoke.ps1`、`docs/superpowers/specs/2026-11-14-b2-auth-onboarding-design.md`
- Create: `internal/server/contract_test.go`

**Interfaces:**
- Consumes: 全部 B2 路由。
- Produces: 文档中每条路径都以 `/api/v1/...` 完整书写（与 `/healthz`、`/readyz` 同级，不使用 `servers` 前缀），前端据此生成客户端。

- [ ] **Step 1: 写失败的契约测试** `contract_test.go`：
  - `TestOpenAPI_DocumentIsValid`：用 `kin-openapi` 加载 `../../api/openapi.yaml` 并 `Validate`。
  - `TestOpenAPI_RoutesMatchDocument`：用 `devLogin=true` 搭一个完整路由（不连真实依赖，可传假 service：对账只看路由表），取 `router.Routes()` 的 `(method, path)` 集合，与文档所有 `(method, path)` 集合**逐条相等**：路由缺文档、文档缺路由都失败，失败信息列出差集。
  - `TestOpenAPI_EveryOperationDeclaresErrorEnvelope`：除 204 外，每个 operation 至少声明一个 4xx/5xx 响应并 `$ref` 到 `Error` schema。
- [ ] **Step 2: 运行确认失败**：`go test -short ./internal/server -run OpenAPI -v`，期望差集非空。
- [ ] **Step 3: 补全 `openapi.yaml`**：加入全部 B2 接口（请求体、成功响应、`UNAUTHORIZED`/`VALIDATION_FAILED`/`ACCOUNT_DISABLED`/`REFRESH_REUSED`/`REFRESH_INVALID`/`PROVIDER_NOT_CONFIGURED` 错误响应）；`components.securitySchemes` 加 `bearerAuth`（HTTP bearer，JWT），`/me` 与 `/me/onboarding` 声明需要它；`/auth/login/dev` 的 description 注明"仅 `APP_ENV=local` 且 `DEV_LOGIN_ENABLED=true` 时存在"；`info.description` 里的 "Draft..." 改为当前状态；`info.version` 升为 `0.2.0`；`Error.error` 增加可选的 `details`（object）。
- [ ] **Step 4: 运行** `go test -short ./internal/server -v`，期望 PASS。
- [ ] **Step 5: 扩展 `scripts/smoke.ps1`**：在原有探活之后，若 `/api/v1/auth/login/dev` 可用，则用 `smoke-<随机数>@local` 登录 → `POST /me/onboarding`（`ja`/`ENFP`/`Smoke`/`18_plus`/`Asia/Tokyo`）→ `GET /me`，断言 `onboarded=true` 且 `pet_type=happy_shiba`，打印邀请码；任一步骤非 2xx 则 `exit 1`。更新文件顶部注释。
- [ ] **Step 6: 对设计文档做小更正**：第 1 行下的"分支"改为"直接在 `main` 上开发"；第 5.9 节关于 sqlc 的括号保留。
- [ ] **Step 7: 端到端验证**：启动本地 Postgres 与 Redis（`task up`），`task migrate`、`task seed`、`task dev`，另开终端 `task smoke`，期望退出码 0；再用 PowerShell 以 `dev-b@local` 登录，确认返回 `onboarded=true`。
- [ ] **Step 8: 最终检查**：`go vet ./...` 无输出；`go test -p 1 ./...` 全部 PASS；`git status` 只含本计划涉及的文件，**不提交**，把改动留给用户审查。

---

## Self-Review

- **设计覆盖**：5.1 登录 → Task 5、6；5.2 令牌与轮换 → Task 2、4、5；5.3 onboarding → Task 7；5.4 宠物 → Task 3；5.5 `GET /me` → Task 7；5.6 种子 → Task 8；5.7 错误码 → Task 5（常量）、6、7、9；5.8 配置 → Task 1；5.9 结构 → File Structure；第 6 节测试要求逐条落在各任务的测试里；OpenAPI 契约 → Task 9。无遗漏。
- **类型一致**：`Tokens`/`LoginResult`/`Service`（auth）、`OnboardingInput`/`OnboardingResult`/`Profile`/`Service`（user）在 Task 5–8 中名称一致；`server.Module` 在 Task 6 定义、Task 7 使用；`httpx.FailDetails` 在 Task 5 定义、Task 6–7 使用。
- **与设计的差异**（执行时需知道）：设计 5.7 的 `NOT_FOUND` 未实现为统一信封（未注册的 dev 路由由 Gin 默认返回 404），计划里按此处理。登出后再用同一 token 刷新得到 `REFRESH_REUSED` 而不是 `REFRESH_INVALID`，两者都是 401。
