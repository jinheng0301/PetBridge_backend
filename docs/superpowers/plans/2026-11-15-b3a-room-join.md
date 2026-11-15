# B3a 验证码入房 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 已 onboarding 的用户凭验证码加入他人小屋并获得奖励码，输码有冷却与限流，并发下不超员、不重复发奖励、不死锁。

**Architecture:** 沿用现有分层 `handler → service → repository`。`JoinService.Join` 在一个事务内按固定加锁顺序完成入房（用户 → 码 → 两间房升序 → 成员）；限流是独立的 `internal/platform/ratelimit`（Redis + 进程内 + 自动退化）；事件通过空实现的 `Publisher` 预留给 B4。

**Tech Stack:** Go、Gin、pgx、go-redis v9（已在 go.mod）、goose 迁移、kin-openapi（契约测试，已在用）。

**Spec:** `docs/superpowers/specs/2026-11-15-b3-room-join-design.md`（本计划只覆盖其中 B3a：5.1–5.5、5.3 登录限流、5.7–5.10 中属于入房的部分；5.6 房主管理留给 B3b 计划）。TRD 的同步修改（7.2、第 8 章、15.5、17 章）已在写计划前完成，不在任务内。

## Global Constraints

- 接口前缀 `/api/v1`；成功 `{data}`，失败 `{error:{code,message,request_id,details?}}`；沿用 `handler.OK / Fail / FailDetails`
- 不修改 `0001`、`0002`；表结构改动只走 `0003_invite_reward_for.sql`
- 加锁顺序 `users → invite_codes → rooms → room_members`；同一事务锁多间房时按房间 id 升序一次锁定
- 码：8 位，字符集 `23456789ABCDEFGHJKLMNPQRSTUVWXYZ`；规范化 = 去空白与 `-`、转大写；格式不符按 `CODE_INVALID` 并计入失败
- 限流常量（代码常量，不进配置）：输码失败 5 次 / 10 分钟 → 冷却 30 分钟；同 IP 每小时 30 次（Redis 退化时 15 次）；登录同 IP 每分钟 10 次
- Redis 键：`rl:join:u:{uid}`、`cd:join:u:{uid}`、`rl:join:ip:{ip}`、`rl:login:ip:{ip}`
- 配置默认值：`REWARD_CODE_COUNT=3`（范围 0–5）、`REWARD_CODE_LIFETIME_CAP=20`（≥1）、`OWNER_REGEN_DAILY_LIMIT=3`（≥1）
- 日志、错误中不得出现完整验证码，只记前 2 位加 `****`
- **不提交**：用户先审查再提交（项目约定）。每个任务结束只验证测试通过，并给出建议的提交信息供用户使用
- 数据库测试用 `dbtest.Reset(t)`（`-short` 跳过）；Redis 测试沿用 `internal/platform/redis/redis_test.go` 的可用性判断与跳过方式

## Review Focus

1. 乱码输入：小写、带连字符/空格、全角字符、超长（10KB）、空字符串的 `code` → 一律 `CODE_INVALID` 且计入失败，不得 500（Task 4、Task 7）
2. 请求体异常：`{}`、`{"code":null}`、`{"code":123}`、非 JSON → `VALIDATION_FAILED`，不计入失败（Task 7）
3. 过期边界：`expires_at == now` 视为已过期（Task 4）
4. Redis 不可用：入房仍成功，不返回 5xx，限流退化到进程内且 IP 上限收紧（Task 2、Task 7）
5. 奖励为 0 时：响应里 `rewards` 是 `[]` 不是 `null`；自己的小屋已满或到终身上限时入房仍成功（Task 4）

---

### Task 1: 错误码、模型类型、配置

**Files:**
- Modify: `internal/model/errcode.go`、`internal/model/user.go`（或新建 `internal/model/room.go`）、`internal/service/errors.go`、`internal/config/config.go`、`.env.example`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces（`model`，错误码常量）：`CodeOnboardingRequired="ONBOARDING_REQUIRED"`、`CodeCodeInvalid="CODE_INVALID"`、`CodeRoomFull="ROOM_FULL"`、`CodeAlreadyMember="ALREADY_MEMBER"`、`CodeRoomLimit="ROOM_LIMIT"`、`CodeAgeRestricted="AGE_RESTRICTED"`、`CodeCodeCooldown="CODE_COOLDOWN"`、`CodeRateLimited="RATE_LIMITED"`、`CodeRoomNotFound="ROOM_NOT_FOUND"`（B3b 还会用 `NOT_ROOM_OWNER`、`OWNER_CANNOT_LEAVE`、`CODE_ISSUE_LIMIT`，一并加上）
- Produces（`model` 类型，JSON 字段名固定）：
  - `InviteCode{Code, RoomID string; IssuedTo, UsedBy *string; Source, Status string; ExpiresAt time.Time}`
  - `RoomState{ID, OwnerID string; MemberCount, Capacity int; Status string}`（内部用，不序列化）
  - `RoomSummary{ID string "id"; Status string "status"; MemberCount int "member_count"; Capacity int "capacity"}`
  - `MemberInfo{UserID string "user_id"; Nickname, PetType *string "nickname","pet_type"; SeatNo int "seat_no"; Role string "role"}`
  - `RoomDetail{RoomSummary 内嵌; IsOwner bool "is_owner"; Members []MemberInfo "members"}`
  - `JoinResult{Room RoomSummary "room"; SeatNo int "seat_no"; Members []MemberInfo "members"; Rewards []CodeInfo "rewards"}`
  - `CodeListItem{Code, Source, Status string; ExpiresAt time.Time "expires_at"; Room RoomInfo "room"}`
- Produces（`service`，错误）：`ErrOnboardingRequired`、`ErrCodeInvalid`、`ErrRoomFull`、`ErrRoomLimit`、`ErrAgeRestricted`、`ErrRoomNotFound`（哨兵）；`*AlreadyMemberError{RoomID string}`、`*CooldownError{RetryAfter time.Duration}`、`*RateLimitedError{RetryAfter time.Duration}`（均实现 `error`）
- Produces（`config.Config` 新字段）：`RewardCodeCount int`、`RewardCodeLifetimeCap int`、`OwnerRegenDailyLimit int`

- [ ] **Step 1: 写失败的配置测试** — `TestLoad_BusinessDefaults`：不设这三个变量，断言 3 / 20 / 3。`TestLoad_BusinessValuesInvalid`（表驱动）：`REWARD_CODE_COUNT` 取 `-1`、`6`、`x`；`REWARD_CODE_LIFETIME_CAP` 取 `0`；`OWNER_REGEN_DAILY_LIMIT` 取 `0`，断言 `problems()` 输出包含对应变量名且不含取值。`TestLoad_RewardCountZeroAllowed`：`REWARD_CODE_COUNT=0` 合法。
- [ ] **Step 2: 运行确认失败** — `go test ./internal/config -run "TestLoad_Business|TestLoad_RewardCount" -v`，预期编译失败或断言失败。
- [ ] **Step 3: 实现** 上述常量、类型、错误和配置解析（风格同 `CODE_TTL_DAYS`：先 `strconv.Atoi` 再范围检查，错误信息只含变量名与规则）；`.env.example` 追加三项并带注释。
- [ ] **Step 4: 运行确认通过** — `go test ./internal/config ./internal/model ./internal/service -short`，预期 PASS。
- [ ] **Step 5: 建议提交信息** `feat(config): add room-join error codes, model types and reward config`（不执行提交）

---

### Task 2: 限流包 `internal/platform/ratelimit`

**Files:**
- Create: `internal/platform/ratelimit/ratelimit.go`（接口、常量、`JoinGuard`）、`memory.go`、`redis.go`、`fallback.go`
- Create: `internal/middleware/ratelimit_middleware.go`
- Test: `internal/platform/ratelimit/ratelimit_test.go`、`redis_test.go`、`internal/middleware/ratelimit_middleware_test.go`

**Interfaces:**
- Produces:
  ```go
  type Hit struct{ Count int64; Resets time.Duration; Degraded bool }
  type Counter interface {
      Incr(ctx context.Context, key string, window time.Duration) (Hit, error)          // 固定窗口，窗口从首次命中起算
      SetFlag(ctx context.Context, key string, ttl time.Duration) error
      FlagTTL(ctx context.Context, key string) (time.Duration, error)                    // 不存在返回 0
      Delete(ctx context.Context, key string) error
  }
  func NewMemory(now func() time.Time) *Memory                       // 实现 Counter
  func NewRedis(c *goredis.Client) *Redis                            // 实现 Counter，INCR+设置过期用 Lua 保证原子
  func NewFallback(primary, secondary Counter, onFallback func(error)) *Fallback // primary 报错时转用 secondary，返回的 Hit.Degraded=true；自身从不返回错误
  func NewJoinGuard(c Counter) *JoinGuard
  func (g *JoinGuard) Cooldown(ctx context.Context, userID string) time.Duration              // >0 表示冷却中
  func (g *JoinGuard) Allow(ctx context.Context, ip string) (ok bool, retryAfter time.Duration)
  func (g *JoinGuard) RecordFailure(ctx context.Context, userID string)                       // 第 5 次失败时 SetFlag 冷却 30 分钟并清失败计数
  func (g *JoinGuard) RecordSuccess(ctx context.Context, userID string)                       // 清失败计数
  ```
- Produces（middleware）：`RateLimitIP(c ratelimit.Counter, name string, limit int, window time.Duration) gin.HandlerFunc`，键 `rl:{name}:ip:{ip}`；超限写 429 `RATE_LIMITED`、`Retry-After` 头（秒，向上取整）并 `Abort`；Counter 报错时放行并 `c.Error(err)`
- `JoinGuard` 满足 `service.JoinGuard` 接口（Task 4 在 `ports.go` 定义，签名与上面四个方法一致）

- [ ] **Step 1: 写失败的测试**（`ratelimit_test.go`，用 `NewMemory` + 可调时钟）
  - `TestMemory_IncrFixedWindow`：同键三次 Incr 得 1,2,3；时钟前进超过窗口后得 1；`Resets` 递减
  - `TestJoinGuard_CooldownAfterFiveFailures`：4 次失败后 `Cooldown==0`；第 5 次后 `Cooldown` 在 (29m, 30m]；前进 30 分钟后为 0
  - `TestJoinGuard_FailuresExpireAfterWindow`：4 次失败，前进 11 分钟，再失败 1 次 → 不冷却
  - `TestJoinGuard_SuccessResetsFailures`：4 次失败 + `RecordSuccess` + 4 次失败 → 不冷却
  - `TestJoinGuard_IPLimit30`：30 次 `Allow` 为 true，第 31 次 false 且 `retryAfter>0`
  - `TestFallback_DegradedUsesTighterIPLimit`：primary 为总是报错的假实现；`NewJoinGuard(NewFallback(...))` 下第 15 次 `Allow` 为 true，第 16 次 false；`onFallback` 被调用
  - `TestFallback_NeverReturnsError`
  - `redis_test.go`：`TestRedis_IncrSetsExpiryAtomically`（Incr 后键 TTL > 0）、`TestRedis_FlagTTL`、`TestRedis_ErrorWhenServerDown`（指向未监听端口返回 error 而非挂起，需在 2 秒内返回）
  - `ratelimit_middleware_test.go`：`TestRateLimitIP_BlocksEleventh`（limit=10：前 10 次 200，第 11 次 429、`Retry-After` 为正整数、响应码 `RATE_LIMITED`）、`TestRateLimitIP_FailsOpenOnCounterError`
- [ ] **Step 2: 运行确认失败** — `go test ./internal/platform/ratelimit ./internal/middleware -short -v`，预期编译失败。
- [ ] **Step 3: 实现** 上述接口。`JoinGuard.Allow` 取 `Hit.Degraded` 选择上限 30 或 15；`Fallback` 内部 secondary 为长期存活的 `Memory`。
- [ ] **Step 4: 运行确认通过** — `go test ./internal/platform/ratelimit ./internal/middleware`（Memurai 已启动时包含 Redis 测试）。
- [ ] **Step 5: 建议提交信息** `feat(ratelimit): add counter-based limiter with in-process fallback and join guard`

---

### Task 3: 迁移 0003 与仓储层

**Files:**
- Create: `db/migrations/0003_invite_reward_for.sql`
- Modify: `internal/service/ports.go`（新增/扩展接口）、`internal/repository/room_repo.go`、`room_member_repo.go`、`invite_code_repo.go`
- Create: `internal/repository/ban_repo.go`、`reward_repo.go`
- Test: `internal/repository/join_repos_test.go`（包 `repository_test`）、`internal/platform/db/migrate_test.go`（补断言）

**Interfaces:**
- Consumes: Task 1 的模型类型；现有 `conn(ctx,pool)`、`IsUniqueViolation`
- Produces（`service/ports.go`，由 repository 满足）：
  ```go
  // RoomStore 新增
  LockRooms(ctx, ids ...string) (map[string]model.RoomState, error) // 去重，ORDER BY id FOR UPDATE；缺失的 id 不在 map 中
  Admit(ctx, roomID string) (memberCount int, ok bool, err error)    // UPDATE ... WHERE status='open' AND member_count<capacity RETURNING
  MarkFull(ctx, roomID string) error
  ListForMember(ctx, userID string) ([]model.RoomDetail, error)       // 在席房间，自己的小屋排第一，Members 已填充
  // RoomMemberStore 新增
  IsActiveMember(ctx, roomID, userID string) (bool, error)
  CountActiveForUser(ctx, userID string) (int, error)
  AgeBands(ctx, roomID string) ([]string, error)                      // 在席成员的 users.age_band
  AddMember(ctx, roomID, userID string) (seatNo int, err error)       // 取最小空位号 1..5，role='member'
  ListMembers(ctx, roomID string) ([]model.MemberInfo, error)         // 在席，按 seat_no
  // InviteCodeStore 新增
  LockByCode(ctx, code string) (model.InviteCode, error)              // FOR UPDATE，无则 model.ErrNotFound
  MarkUsed(ctx, code, userID string, at time.Time) error
  VoidUnusedForRoom(ctx, roomID string) (holders []string, err error) // status=unused → void，返回被作废码的 issued_to（去重、去空）
  CountUnusedValid(ctx, roomID string, now time.Time) (int, error)    // status=unused 且 expires_at>now
  LifetimeCount(ctx, userID string) (int, error)                      // issued_to=用户 且 source<>'zero'
  InsertReward(ctx, code, roomID, userID, forCode string, expiresAt time.Time) (inserted bool, err error) // source='join_reward'，ON CONFLICT DO NOTHING
  RewardsFor(ctx, forCode string) ([]model.CodeInfo, error)
  ListForUser(ctx, userID string, now time.Time) ([]model.CodeListItem, error) // issued_to=用户，issued_at 倒序，Status 为有效状态（unused 且已过期 → expired）
  // 新接口
  type BanStore interface{ IsBanned(ctx, roomID, userID string) (bool, error) }
  type RewardStore interface {
      Grant(ctx, userID, roomID string, count int) (inserted bool, err error) // reward_grants ON CONFLICT DO NOTHING
      InsertEdge(ctx, inviterID, inviteeID, code string) error
  }
  ```
- Produces（`InviteCodeRepo`）：`InsertVoidZero(ctx, code, roomID string, expiresAt time.Time) error`（source='zero'、status='void'，冲突忽略），给 Task 8 用

- [ ] **Step 1: 写失败的测试**（均 `dbtest.Reset`）
  - `TestMigration0003_AddsRewardForColumn`：迁移后 `invite_codes.reward_for` 存在、可空、引用 `invite_codes(code)`；`down` 后消失
  - `TestLockRooms_DedupesAndOrders`：传入 `(b, a, a)` 返回两条；并发两个事务以相反顺序传入同一对房间，各 50 轮，不死锁（5 秒超时）
  - `TestAdmit_RespectsCapacityAndStatus`：满额返回 `ok=false`；`status='full'` 返回 `ok=false`；正常返回新人数
  - `TestAddMember_PicksSmallestFreeSeat`：座位 1、3 被占 → 返回 2；某成员 `left_at` 置位后其座位被回收
  - `TestVoidUnusedForRoom_ReturnsHoldersOnly`：3 个 unused（2 个 issued_to 同一人，1 个无 issued_to）+1 个 used → 作废 3 个，返回去重后的 1 个持有人，used 不变
  - `TestCountUnusedValid_ExcludesExpiredAndBoundary`：`expires_at == now` 不计入
  - `TestLifetimeCount_ExcludesZero`：signup、join_reward 计入，zero（`issued_to` 为空）不计入
  - `TestInsertReward_And_RewardsFor`：奖励码带 `reward_for`；重复插入同码返回 `inserted=false`
  - `TestListForUser_EffectiveStatus`：过期的 unused 返回 `expired`
  - `TestListForMember_OwnRoomFirstAndMembers`
  - `TestGrant_Once`：第二次 `inserted=false`；`count=0` 也能插入
- [ ] **Step 2: 运行确认失败** — `go test ./internal/repository ./internal/platform/db -v`，预期编译失败。
- [ ] **Step 3: 实现** 迁移（`alter table invite_codes add column reward_for text references invite_codes(code)` 加索引 `idx_invite_codes_reward_for`；Down 对称）与以上方法；`ports.go` 同步接口。SQL 手写 pgx，不引入 sqlc。
- [ ] **Step 4: 运行确认通过** — `go test -p 1 ./internal/repository ./internal/platform/db`。
- [ ] **Step 5: 建议提交信息** `feat(db): add reward_for migration and room-join repository queries`

---

### Task 4: `JoinService`

**Files:**
- Create: `internal/service/join_service.go`、`internal/service/events.go`、`internal/service/reward.go`
- Modify: `internal/service/ports.go`（`JoinGuard`、`Publisher`）、`internal/service/user_service.go`（把 `loadProfile` 的状态规则抽成可复用函数 `profileOf`，行为不变）
- Test: `internal/service/join_service_test.go`、`reward_test.go`、`join_helpers_test.go`

**Interfaces:**
- Consumes: Task 1–3 的全部类型与接口
- Produces:
  ```go
  type JoinGuard interface {
      Cooldown(ctx context.Context, userID string) time.Duration
      Allow(ctx context.Context, ip string) (bool, time.Duration)
      RecordFailure(ctx context.Context, userID string)
      RecordSuccess(ctx context.Context, userID string)
  }
  type Event struct{ Type, RoomID string; To []string; Data map[string]any }
  type Publisher interface{ Publish(ctx context.Context, e Event) }
  type NopPublisher struct{}   // Publish 什么也不做
  type JoinConfig struct{ CodeTTLDays, RewardCount, RewardLifetimeCap int }
  func NewJoinService(users UserStore, rooms RoomStore, members RoomMemberStore, codes InviteCodeStore,
      bans BanStore, rewards RewardStore, tx TxRunner, guard JoinGuard, pub Publisher,
      cfg JoinConfig, now func() time.Time) *JoinService
  func (s *JoinService) Join(ctx context.Context, userID, ip, rawCode string) (model.JoinResult, error)
  func NormalizeCode(raw string) (code string, ok bool)   // 去空白与 '-'、转大写；ok 表示符合 ^[2-9A-HJ-NP-Z]{8}$
  func RewardCount(configured, freeSlots, unusedValid, lifetimeLeft int) int // max(0, min(...))
  func MaskCode(code string) string   // 前 2 位 + "****"；长度不足 2 时返回 "****"。service 与 handler 的日志、错误信息只能用它输出码
  ```
- Join 的行为完全以 spec 5.1 为准：事务前 冷却 → `Allow(ip)` → 事务；事务内顺序为锁用户并校验已 onboarding 与账号状态 → 锁码（含重放判断）→ `LockRooms(目标房, 自己小屋)` → 业务校验（优先级：closed、房主是自己、被拉黑 → `ErrCodeInvalid`；已在房 → `*AlreadyMemberError`；在席房间数 ≥2 → `ErrRoomLimit`；年龄不一致 → `ErrAgeRestricted`）→ `Admit` → 其余步骤。事务返回 `ErrCodeInvalid`（含格式错）时调用 `RecordFailure`；成功或重放调用 `RecordSuccess`；其它错误不动计数。事件在提交成功后发布：`member.joined`（To=房内其余在席成员）、`code.invalidated`（To=`VoidUnusedForRoom` 返回的持有人）。奖励码主键冲突重试最多 5 次。

- [ ] **Step 1: 写失败的测试**
  - 单元（无数据库）：`TestNormalizeCode`（表驱动：`"abcd-efgh"→"ABCDEFGH"`、`" abcd efgh "`、`"ABCD0EFG"`含 0 → `ok=false`、`"ＡＢＣＤ２３４５"` 全角 → false、10KB 字符串 → false、`""` → false）；`TestRewardCount`（表驱动：配置 3/空位 5/未用 0/剩余 20 → 3；空位 1 → 1；未用已占满空位 → 0；终身剩余 2 → 2；配置 0 → 0；任一为负 → 0）；`TestMaskCode`（`"ABCD2345"→"AB****"`、`"A"→"****"`、`""→"****"`）；`TestJoin_ErrorsNeverContainFullCode`：对各类失败的 `err.Error()` 断言不含完整原码
  - 集成，helper `joinEnv`（`join_helpers_test.go`）：`newJoinEnv(t)` 用真实仓储与固定时钟、可记录的 `fakeGuard` 和 `recordingPublisher` 构建；`(e).user(t, email, ageBand) string` 经 `AuthService.EnsureUser` + `UserService.Onboard` 创建已 onboarding 用户并返回 id；`(e).signupCode(t, userID) string`
  - `TestJoin_Success`：B 用 A 的 signup 码 → A 的小屋 `member_count=2`、B 在 seat 2、码 `used` 且 `used_by=B`、`invite_edges` 1 行、`reward_grants(B, A房)` 1 行、B 的小屋多出 3 个 `join_reward` 码且 `reward_for=该码`；`RecordSuccess` 被调用；`member.joined` 发给 A
  - `TestJoin_CodeNormalization`：小写带连字符的码可用
  - `TestJoin_InvalidCases`（表驱动，均 `ErrCodeInvalid` 且 `RecordFailure` 被调用）：不存在的码、格式错的码、用自己的码、已被他人用过、已作废、`expires_at == now`、被房主拉黑（直接 SQL 插 `room_bans`）
  - `TestJoin_NotOnboarded`：`ErrOnboardingRequired`，不 `RecordFailure`
  - `TestJoin_AlreadyMember`：B 已在 A 房，再用 A 的另一个未用码 → `*AlreadyMemberError{RoomID}`，该码仍 `unused`，无奖励
  - `TestJoin_RoomLimit`：B 已在 A、C 两房，用 D 的码 → `ErrRoomLimit`，不 `RecordFailure`
  - `TestJoin_AgeRestricted`：成年 B 进未成年房主的房 → `ErrAgeRestricted`；反向同理
  - `TestJoin_ErrorPriority`：B 在 A 房且已有 2 间房，再用 A 房的码 → `*AlreadyMemberError`（优先于 `ErrRoomLimit`）
  - `TestJoin_RoomFull_VoidsRemainingCodes`：第 5 人入房后房间 `full`、其余 unused 码 `void`、`code.invalidated` 发给房主；之后第 6 人用已作废码 → `ErrCodeInvalid`；此外用直接把 `member_count` 置满但码仍 `unused` 的构造验证 `ErrRoomFull` 且不 `RecordFailure`
  - `TestJoin_ReplayReturnsSameResult`：同一用户同一码调用两次，第二次结果相同（含相同奖励码）、`member_count` 与奖励码数量不变、不 `RecordFailure`
  - `TestJoin_ReplayDeniedAfterLeaving`：B 入房后把其 `room_members.left_at/left_reason='left'` 置位，用同一码重放 → `ErrCodeInvalid`
  - `TestJoin_RejoinAfterLeavingGivesNoSecondReward`：B 退出后用新码重进同一房 → 奖励码总数不变
  - `TestJoin_RewardsEmptyNotNil`：B 的小屋已满（直接 SQL 构造）→ 入房成功，`Rewards` 为非 nil 的空切片；`reward_grants.granted_count=0` 且仍有该行
  - `TestJoin_RewardsLimitedByLifetimeCap`：用 `RewardLifetimeCap` 配置为 2 且 B 已有 signup 码 → 只发 1 个
  - `TestJoin_CooldownAndRateLimitShortCircuit`：`fakeGuard` 返回冷却 → `*CooldownError{RetryAfter}`，未开事务（用 `Allow` 未被调用、库无变化断言）；`Allow` 返回 false → `*RateLimitedError`
  - `TestJoin_PublishOnlyAfterCommit`：事务因 `ErrRoomFull` 回滚时 `recordingPublisher` 无事件
- [ ] **Step 2: 运行确认失败** — `go test -p 1 ./internal/service -run "TestJoin|TestNormalizeCode|TestRewardCount" -v`，预期编译失败。
- [ ] **Step 3: 实现** 上述类型与 `Join`。奖励数量在锁住两间房之后计算（空位 = 自己小屋 `Capacity - MemberCount`），先算数量再 `Grant`，仅 `inserted=true` 时才插入奖励码。
- [ ] **Step 4: 运行确认通过** — `go test -p 1 ./internal/service`，预期全部 PASS（含原有测试）。
- [ ] **Step 5: 建议提交信息** `feat(service): add transactional join-by-code with rewards and replay`

---

### Task 5: 并发测试

**Files:**
- Create: `internal/service/join_concurrency_test.go`

**Interfaces:**
- Consumes: Task 4 的 `joinEnv`、`JoinService.Join`；用 `sync.WaitGroup` + 起跑栅栏让协程同时开始；整个测试 30 秒超时

- [ ] **Step 1: 写测试**（B3a 范围内的 spec 第 8 节并发 1–5，每个都用真实数据库）
  - `TestConcurrent_100JoinersForLastSeat`：房主 + 3 人已入房（余 1 席），房主再有 100 个未用码（直接 SQL 插入）分给 100 个用户并发入房 → 恰好 1 个成功，其余为 `ErrRoomFull` 或 `ErrCodeInvalid`；`member_count == 5`、`status == 'full'`；`reward_grants` 对该房间恰 1 行；未用码全部 `void`
  - `TestConcurrent_SameUserTwoDifferentRooms`：A、C 两间房，B 并发用两个码各进一间，重复 20 轮 → 每轮 B 在席房间数 ≤ 2（自己的 + 最多 1 间），另一个请求返回 `ErrRoomLimit`
  - `TestConcurrent_SameUserSameCodeDuplicateSubmit`：同一用户 10 个协程提交同一个码 → 全部成功且结果相同，`member_count` 只加 1，奖励码只发一份
  - `TestConcurrent_AdultAndMinorSameRoom`：成年房主的房，成年 X 与未成年 M 并发用不同码入房 → X 成功、M 恒为 `ErrAgeRestricted`，房间无未成年成员
  - `TestConcurrent_CrossJoinNoDeadlock`：A 用 B 的码进 B 房、B 同时用 A 的码进 A 房，重复 50 轮（每轮重置状态）→ 每轮均在 5 秒内完成且无 `deadlock detected` 错误
- [ ] **Step 2: 运行** — `go test -p 1 ./internal/service -run TestConcurrent -race -count=1 -v`。若失败，按失败信息回到 Task 4 修正 `Join`（不要放宽断言），修正后重跑直到稳定通过。再用 `-count=5` 重复运行确认不是偶然通过。
- [ ] **Step 3: 建议提交信息** `test(service): concurrent join invariants (capacity, duplicate submit, cross-room locking)`

---

### Task 6: 查询服务（背包与我的房间）

**Files:**
- Create: `internal/service/code_service.go`、`internal/service/room_service.go`
- Test: `internal/service/code_service_test.go`、`room_service_test.go`

**Interfaces:**
- Consumes: Task 3 的 `ListForUser`、`ListForMember`
- Produces:
  ```go
  func NewCodeService(codes InviteCodeStore, now func() time.Time) *CodeService
  func (s *CodeService) List(ctx context.Context, userID string) ([]model.CodeListItem, error) // 无记录返回非 nil 空切片
  func NewRoomService(rooms RoomStore) *RoomService   // B3b 会扩展此结构体，新增参数时需同步调用方
  func (s *RoomService) Mine(ctx context.Context, userID string) ([]model.RoomDetail, error)
  ```

- [ ] **Step 1: 写失败的测试**
  - `TestCodeList_EffectiveStatusAndOrder`：signup 码、两个奖励码（其中一个把 `expires_at` 改为过去）→ 倒序，过期的为 `expired`，每项带所属房间人数；`zero` 码不出现
  - `TestCodeList_EmptyIsNotNil`
  - `TestRoomMine_OwnRoomFirst`：B 自己的小屋 + 已加入 A 的小屋 → 两项，自己的排第一且 `IsOwner=true`，成员按 seat 排序；已离开的成员不出现
- [ ] **Step 2: 运行确认失败** — `go test -p 1 ./internal/service -run "TestCodeList|TestRoomMine" -v`
- [ ] **Step 3: 实现**（薄封装，主要保证空切片与排序语义）
- [ ] **Step 4: 运行确认通过** — `go test -p 1 ./internal/service`
- [ ] **Step 5: 建议提交信息** `feat(service): list own invite codes and rooms`

---

### Task 7: HTTP 层、装配与 OpenAPI

**Files:**
- Create: `internal/handler/room_handler.go`、`internal/handler/code_handler.go`
- Modify: `internal/handler/auth_handler.go`（新增 `func (h *AuthHandler) WithLoginLimit(mw gin.HandlerFunc) *AuthHandler`，登录三条路由挂上它；构造函数签名不变）、`cmd/api/main.go`、`api/openapi.yaml`、`internal/server/contract_test.go`、`internal/handler/helpers_test.go`
- Test: `internal/handler/room_handler_test.go`、`code_handler_test.go`、`auth_handler_test.go`（追加）

**Interfaces:**
- Consumes: Task 4、6 的服务；Task 2 的 `RateLimitIP`；现有 `middleware.Auth`
- Produces:
  ```go
  func NewRoomHandler(join *service.JoinService, rooms *service.RoomService, authn gin.HandlerFunc) *RoomHandler // Routes: POST /rooms/join, GET /rooms/mine
  func NewCodeHandler(codes *service.CodeService, authn gin.HandlerFunc) *CodeHandler                           // Routes: GET /codes
  ```
- 响应与错误映射以 spec 5.8 为准；`join` 请求体 `{ "code": string }`，缺少字段、`null`、非字符串、非 JSON 一律 `VALIDATION_FAILED`（不计入失败）；`code` 字段存在即交给 `Join`（含空串与乱码 → `CODE_INVALID`）。`ALREADY_MEMBER` 的 `details.room_id`；`CODE_COOLDOWN` 的 `details.retry_after_seconds`；`RATE_LIMITED` 带 `Retry-After`。IP 取 `c.ClientIP()`。成功 `rewards` 序列化为 `[]`。
- `main.go` 装配：`counter := ratelimit.NewFallback(ratelimit.NewRedis(rdb), ratelimit.NewMemory(time.Now), warnLog)`；`NewJoinGuard(counter)`；登录用 `middleware.RateLimitIP(counter, "login", 10, time.Minute)`；`NopPublisher{}`；`JoinConfig` 取自 `cfg`。

- [ ] **Step 1: 写失败的测试**（真实数据库，沿用 `newRouter`/`call`/`devLogin`；先把 `helpers_test.go` 的 `newServices`/`newRouter` 扩展到包含新服务，Redis 用 `ratelimit.NewMemory`）
  - `TestJoin_HTTPSuccess`：B 用 `DEVA2345` 风格的码 → 200，`data.room.member_count=2`、`data.rewards` 为 3 个元素的数组
  - `TestJoin_ErrorMapping`（表驱动，检查状态码与 `error.code`）：无码 → 404 `CODE_INVALID`；已满 → 409 `ROOM_FULL`；已在房 → 409 `ALREADY_MEMBER` 且 `details.room_id`；第 3 间 → 409 `ROOM_LIMIT`；年龄 → 403 `AGE_RESTRICTED`；未 onboarding → 403 `ONBOARDING_REQUIRED`；无 token → 401
  - `TestJoin_BadBodies`：`{}`、`{"code":null}`、`{"code":123}`、`not json`、空体 → 400 `VALIDATION_FAILED`；之后同一用户再 4 次错码仍未进入冷却（证明未计入失败）
  - `TestJoin_GarbageCodes`：全角、10KB、`"  "`、`"abcd-0efg"` → 404 `CODE_INVALID`
  - `TestJoin_CooldownAfterFiveWrongCodes`：5 次错码后第 6 次（即使码正确）→ 429 `CODE_COOLDOWN` 且 `details.retry_after_seconds` 在 (0, 1800]
  - `TestJoin_DisabledAccount`：`users.status='banned'` 的用户带有效 token → 403 `ACCOUNT_DISABLED`
  - `TestJoin_WorksWhenRedisDown`：以总是报错的 Counter 经 `Fallback` 装配 → 入房仍 200
  - `TestCodes_HTTP` / `TestRoomsMine_HTTP`：状态码、字段、`[]` 而非 `null`、无 token 401
  - `TestLogin_RateLimited`：同 IP 第 11 次 `login/dev` → 429 `RATE_LIMITED` 带 `Retry-After`；`/auth/refresh` 不受限
  - 契约：`contract_test.go` 的路由构造补上 `RoomHandler`、`CodeHandler`；`TestOpenAPI_RoutesMatchDocument` 通过说明文档与路由一致
- [ ] **Step 2: 运行确认失败** — `go test -p 1 ./internal/handler ./internal/server -v`
- [ ] **Step 3: 实现** 两个 handler、`WithLoginLimit`、`main.go` 装配；`openapi.yaml`：版本改 `0.3.0`，新增 3 个路径与 `Join*` / `RoomDetail` / `CodeListItem` 等 schema，新增错误响应组件（`CodeInvalid`、`Conflict`、`Forbidden`、`TooManyRequests` 且 429 声明 `Retry-After` 头），`Error.code` 描述补全新错误码，登录路径补 429。
- [ ] **Step 4: 运行确认通过** — `go test -p 1 ./...`，并 `go vet ./...`，预期全部 PASS。
- [ ] **Step 5: 建议提交信息** `feat(api): join room by code, list codes and rooms, rate-limit login`

---

### Task 8: 种子数据与冒烟脚本

**Files:**
- Modify: `cmd/tools/seed/seed.go`、`cmd/tools/seed/seed_test.go`、`scripts/smoke.ps1`、`Taskfile.yml`（seed 描述）、`internal/repository/invite_code_repo.go`（若 Task 3 未加则补 `InsertVoidZero`）
- Test: `cmd/tools/seed/seed_test.go`

**Interfaces:**
- Consumes: Task 4 的 `JoinService`（用进程内 `JoinGuard` 与 `NopPublisher` 构造）；`InviteCodeRepo.InsertZero / InsertVoidZero`
- 新增 5 个用户（全部 `18_plus`，母语与 MBTI 任选合法值，时区 `Europe/London`）：`dev-full@local` 与 `dev-fill1..4@local`；为 dev-full 的小屋写入 4 个 zero 码 `FULA2345`、`FULB2345`、`FULC2345`、`FULD2345`（有效期 10 年），由 4 个占座用户经真实 `Join` 加入；占满后再写入 `FULL2345`（zero、void）

- [ ] **Step 1: 写失败的测试**（更新原测试并追加）
  - 修改 `TestRun_CreatesSixUsersAndZeroCode` 的用户数断言为 11（A–F 的属性断言保持不变，测试名同步改为 `...CreatesElevenUsers...`）
  - `TestRun_BuildsFullRoom`：dev-full 的房间 `status='full'`、`member_count=5`、5 个在席成员；`FULL2345` 为 `void`；4 个占座码均为 `used`；A–F 各自的在席房间数仍为 1
  - `TestRun_FullCodeIsInvalidForJoin`：B（dev-b）用 `FULL2345` 入房 → `ErrCodeInvalid`
  - `TestRun_DevaCodeStillJoinable`：B 用 `DEVA2345` 入房成功（证明 A–F 没被占座名额）
  - 原有的"运行两次结果一致"断言扩展覆盖新用户与新码（总行数不变）
- [ ] **Step 2: 运行确认失败** — `go test -p 1 ./cmd/tools/seed -v`
- [ ] **Step 3: 实现** 种子扩展；`scripts/smoke.ps1` 追加 B3 流程：再登录第二个临时用户并 onboarding，用第一个用户的 signup 码入房，断言返回 200、`data.room.member_count -eq 2`、`data.rewards.Count -gt 0`，再 `GET /rooms/mine` 断言第一项 `is_owner` 为 true 且共 2 个房间；`Taskfile.yml` 的 `seed` 描述改为"11 个用户 + `DEVA2345` + `FULL2345`"。
- [ ] **Step 4: 运行确认通过** — `go test -p 1 ./...` 全部 PASS；启动 Memurai 与 API 后 `task seed` 连续运行两次无错误，`task smoke` 输出 OK 行。
- [ ] **Step 5: 建议提交信息** `feat(seed): full-room fixture FULL2345 and join smoke flow`

---

## 完成后

全部任务通过后请用户审查整个工作区。提交由用户决定，按项目约定拆成多个 conventional commits（建议提交信息已在每个任务末尾）。B3b（移除、解散、补码、退出）另写计划，复用本计划的 `joinEnv`、`LockRooms`、`RoomService`。
