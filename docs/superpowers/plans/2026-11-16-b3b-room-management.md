# B3b 房主管理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 房主能移除成员、解散小屋、自助补码，成员能退出他人小屋；全部在并发下不超员、不死锁、不泄露"被移除"。

**Architecture:** 新增 `ManageService`（`internal/service/room_manage_service.go`），与 `JoinService` 共用仓储接口和事件 `Publisher`；每个操作一个事务，先锁房间行再做业务校验。路由挂在现有 `RoomHandler` 上（`WithManage` 注入，与 `WithLoginLimit` 同风格）。死锁/序列化失败由一个 `RetryTx` 装饰器做一次有界重试，只在 `main.go` 接线，B3a 的 `JoinService` 一并受益（入房重试安全，已提交的会走重放分支）。

**Tech Stack:** Go、Gin、pgx、kin-openapi（契约测试，已在用）。无新依赖、无新迁移。

**Spec:** `docs/superpowers/specs/2026-11-15-b3-room-join-design.md` 的 5.6、5.7、5.8、8、9；TRD 7.8。本计划只覆盖 B3b；B3a 已完成。

## Global Constraints

- 接口前缀 `/api/v1`；成功 `{data}`，失败 `{error:{code,message,request_id,details?}}`；沿用 `handler.OK / Fail / FailDetails`
- 错误码常量 `NOT_ROOM_OWNER`(403)、`OWNER_CANNOT_LEAVE`(409)、`CODE_ISSUE_LIMIT`(409, `details.reason` ∈ `room_full|enough_unused|daily_limit|lifetime_limit`)、`ROOM_NOT_FOUND`(404) 已在 `internal/model/errcode.go`；`failRoom` 还没映射它们，本计划补上
- 加锁顺序 `users → invite_codes → rooms → room_members`；解散必须先按 code 排序锁该房全部 unused 码，再锁房间，不得直接 `UPDATE invite_codes ... WHERE room_id=...`
- 不修改 `0001`–`0003`；本计划不加迁移
- **越权与存在性规则（本计划定稿，spec 5.6 末段同步修订）**：房间不存在 → `ROOM_NOT_FOUND`；调用者对房主操作是该房在席成员但非房主 → `NOT_ROOM_OWNER`；既非房主也非在席成员 → `ROOM_NOT_FOUND`。`leave` 例外：曾经是成员（含主动退出、被移除、被解散）→ 204，从没进过 → `ROOM_NOT_FOUND`，使被移除者与主动退出者看到完全一样的结果
- 响应：移除、解散 200 `{room: RoomSummary}`；补码 201 `CodeInfo`（`{code, expires_at}`）；退出 204 无 body
- 事件仅在**提交之后**用 `context.WithoutCancel(ctx)` 发布；幂等空操作（目标已不在席、重复解散、重复退出）不发事件
- 日志、错误里不得出现完整验证码，只用 `MaskCode`
- **不提交**：用户先审查再提交。每个任务只验证测试通过，并给出建议提交信息（用户偏好分成多个 conventional commit）
- 数据库测试用 `dbtest.Reset(t)`（`-short` 跳过）；并发测试沿用 `join_concurrency_test.go` 的 `concurrentTimeout` 与启动栅栏写法

## Review Focus

1. 被移除者调 `leave` → 204，与主动退出者一致；从没进过的人 → 404；任何响应都不出现 "kicked"（Task 3、Task 6）
2. `{id}` 不是 UUID（`abc`、10KB 字符串）、`{user_id}` 不是 UUID：id 非法 → 404 `ROOM_NOT_FOUND`，user_id 非法 → 幂等 200，都不得 500（Task 1 仓储守卫、Task 3、Task 6）
3. 房主移除自己（`user_id` = 房主）→ 400 `VALIDATION_FAILED`，且不写 `room_bans`（Task 3）
4. 补码边界：解散后（成员数 1、空位 4）可补码；房间 `full` → `room_full`；`users.tz` 为空或非法时按 UTC 算"今日"，不得 500；日界按房主本地午夜，夏令时日也成立（Task 2、Task 5）
5. 解散两次幂等；被解散者可用**新码**重新加入但**不再**拿奖励，旧码 `CODE_INVALID`（Task 4）
6. 提交后客户端断开：事件仍然发出（Task 3，对应 B3a 同类用例）

---

## File Structure

| 文件 | 职责 |
| :-- | :-- |
| `internal/repository/room_repo.go`（改） | `Peek`、`Vacate`、`Reset` |
| `internal/repository/room_member_repo.go`（改） | `Leave`、`EverMember`、`DissolveMembers` |
| `internal/repository/ban_repo.go`（改） | `Ban` |
| `internal/repository/invite_code_repo.go`（改） | `InsertRegen`、`CountRegenBetween` |
| `internal/repository/tx.go`（改） | `IsRetryableTxError` |
| `internal/service/ports.go`（改） | 上述接口方法 |
| `internal/service/errors.go`（改） | 新错误 |
| `internal/service/events.go`（改） | `member.left`、`room.kicked` 常量与构造函数 |
| `internal/service/retry_tx.go`（新） | `RetryTx` 装饰器 |
| `internal/service/local_day.go`（新） | `LocalDayBounds` |
| `internal/service/room_manage_service.go`（新） | `ManageService`：Remove、Leave、Dissolve、RegenCode |
| `internal/handler/room_manage_handler.go`（新）、`room_handler.go`（改） | 四个路由、`failRoom` 补映射 |
| `cmd/api/main.go`（改） | 接线 |
| `api/openapi.yaml`、TRD、`scripts/smoke.ps1` | 契约、文档、冒烟 |

---

### Task 1: 仓储原语与接口

**Files:**
- Modify: `internal/repository/room_repo.go`、`room_member_repo.go`、`ban_repo.go`、`invite_code_repo.go`、`tx.go`、`internal/service/ports.go`、`internal/model/room.go`
- Test: `internal/repository/join_repos_test.go`（沿用其夹具与 `dbtest.Reset`）

**Interfaces:**
- Produces（`model`）：`RoomResult{Room RoomSummary "room"}`
- Produces（`RoomStore` 新增）：
  - `Peek(ctx, roomID string) (model.RoomState, error)` 不加锁；不存在或 id 非法返回 `model.ErrNotFound`
  - `Vacate(ctx, roomID string) (memberCount int, err error)`：`member_count-1`，状态为 `full` 时置 `open`，其余不变
  - `Reset(ctx, roomID string) error`：`member_count=1, status='open'`
- Produces（`RoomMemberStore` 新增）：
  - `Leave(ctx, roomID, userID, reason string, at time.Time) (left bool, err error)`：只更新在席行，`left=false` 表示本就不在席；`reason` ∈ `left|kicked|dissolved`；id 非法返回 `false, nil`
  - `EverMember(ctx, roomID, userID string) (bool, error)`：存在任意一行（含已离开）；id 非法返回 `false, nil`
  - `DissolveMembers(ctx, roomID string, at time.Time) (userIDs []string, err error)`：把所有 `role<>'owner'` 的在席行置 `dissolved`，返回被移出者，按 `seat_no` 升序
- Produces（`BanStore` 新增）：`Ban(ctx, roomID, userID, bannedBy string) error`，`ON CONFLICT DO NOTHING`
- Produces（`InviteCodeStore` 新增）：
  - `InsertRegen(ctx, code, roomID, ownerID string, issuedAt, expiresAt time.Time) (inserted bool, err error)`：`source='owner_regen'`，`issued_at` 显式写入（让时钟可注入），主键冲突 `inserted=false`
  - `CountRegenBetween(ctx, ownerID string, from, to time.Time) (int, error)`：`issued_to=ownerID and source='owner_regen' and issued_at>=from and issued_at<to`
- Produces（`repository`）：`IsRetryableTxError(err error) bool`，SQLSTATE `40P01` 或 `40001`（用 `errors.As` 到 `*pgconn.PgError`，包装过的错误也要识别）

- [ ] **Step 1: 写失败的仓储测试**（`join_repos_test.go`，同文件风格）：
  - `TestRoomRepo_PeekVacateReset`：`Peek` 返回与 `LockRooms` 一致的 `RoomState`；`Peek("abc")` 与不存在的 UUID 均为 `ErrNotFound`；满员房 `Vacate` 后 `status=='open'`、`member_count` 减 1；`open` 房 `Vacate` 后状态不变；`Reset` 后 `member_count==1 && status=='open'`
  - `TestMemberRepo_LeaveEverMemberDissolve`：`Leave(..., "kicked", at)` 第一次 `left=true`、第二次 `false`，行上 `left_reason='kicked'`；`EverMember` 对退出者为 `true`、对陌生人为 `false`、对 `"abc"` 为 `false` 且无错误；`Leave` 对 `"abc"` 返回 `false,nil`；`DissolveMembers` 返回非房主成员 id 且房主仍在席、`left_reason='dissolved'`；释放的座位号能被 `AddMember` 取回（最小空位）
  - `TestBanRepo_Ban`：`Ban` 后 `IsBanned` 为真；重复 `Ban` 不报错
  - `TestInviteRepo_Regen`：`InsertRegen` 写入后 `source='owner_regen'`、`issued_at` 等于传入值；重复 code 返回 `inserted=false`；`CountRegenBetween` 半开区间边界：`issued_at==from` 计入、`==to` 不计入；不计其他来源的码
  - `TestIsRetryableTxError`：包装的 `*pgconn.PgError{Code:"40P01"}`、`"40001"` 为真；`"23505"`、`nil`、`context.Canceled` 为假
- [ ] **Step 2: 运行确认失败** — `go test ./internal/repository -run "TestRoomRepo_Peek|TestMemberRepo_Leave|TestBanRepo_Ban|TestInviteRepo_Regen|TestIsRetryable" -v`，预期编译失败。
- [ ] **Step 3: 实现** 上述方法与 `model.RoomResult`，并把方法加进 `ports.go` 的各接口。SQL 手写 pgx，`conn(ctx, pool)` 风格；`Peek`/`Leave`/`EverMember`/`Ban` 入口用 `uuidRE` 守卫，避免类型转换报错。
- [ ] **Step 4: 运行确认通过** — `go build ./... && go test ./internal/repository -v`（需本地 PostgreSQL，`petbridge_test`）。预期 PASS。
- [ ] **Step 5: 建议提交信息** `feat(repo): add room vacate/reset, leave, ban and regen primitives`（不执行提交）

---

### Task 2: 错误、事件、重试装饰器、本地日界

**Files:**
- Modify: `internal/service/errors.go`、`internal/service/events.go`
- Create: `internal/service/retry_tx.go`、`internal/service/local_day.go`
- Test: `internal/service/retry_tx_test.go`、`internal/service/local_day_test.go`、`internal/service/events_test.go`

**Interfaces:**
- Produces（错误）：哨兵 `ErrNotRoomOwner`、`ErrOwnerCannotLeave`、`ErrCannotRemoveOwner`；`*CodeIssueLimitError{Reason string}`；常量 `ReasonRoomFull="room_full"`、`ReasonEnoughUnused="enough_unused"`、`ReasonDailyLimit="daily_limit"`、`ReasonLifetimeLimit="lifetime_limit"`
- Produces（事件）：常量 `EventMemberLeft="member.left"`、`EventRoomKicked="room.kicked"`；
  - `memberLeftEvent(roomID, userID, reason string, to []string) Event`：`Data={"room_id","user_id","reason"}`
  - `roomKickedEvents(roomID, reason string, users []string) []Event`：每人一条，`To=[user]`，`Data={"room_id","reason"}`
- Produces：`type RetryTx struct{ Inner TxRunner; Retryable func(error) bool }`，实现 `TxRunner`；`fn` 最多执行 2 次，仅当 `Retryable(err)` 且 `ctx.Err()==nil` 时才重试第二次，第二次的错误原样返回
- Produces：`LocalDayBounds(now time.Time, tz *string) (from, to time.Time)`：`tz` 为空或 `time.LoadLocation` 失败则用 UTC；`from` 为 `now` 在该时区当天 0 点，`to` 为次日 0 点（用 `time.Date(y, m, d+1, 0,0,0,0, loc)` 求，不要加 24h）

- [ ] **Step 1: 写失败的测试**
  - `TestRetryTx`：fake `TxRunner` 脚本化（先 `40P01` 后成功 → 共 2 次、返回 nil；两次都可重试错误 → 共 2 次、返回第二次错误；不可重试错误 → 共 1 次；第一次失败后 ctx 已取消 → 共 1 次）
  - `TestLocalDayBounds`（表驱动）：`Asia/Seoul` 的 `2026-11-15T16:00Z`（本地已是 16 日 01:00）→ `[2026-11-15T15:00Z, 2026-11-16T15:00Z)`；`America/New_York` 夏令时结束日 `2026-11-01` 的一天长度为 25h；春季开始日 `2026-03-08` 为 23h；`tz=nil`、`""`、`"Not/AZone"` 均按 UTC
  - `TestManageEvents`：`memberLeftEvent` 与 `roomKickedEvents` 的 `Type/To/Data` 精确匹配
- [ ] **Step 2: 运行确认失败** — `go test ./internal/service -short -run "TestRetryTx|TestLocalDayBounds|TestManageEvents" -v`
- [ ] **Step 3: 实现** 上述类型与函数（`RetryTx` 不依赖 repository，`Retryable` 由 `main.go` 注入 `repository.IsRetryableTxError`）。
- [ ] **Step 4: 运行确认通过** — `go test ./internal/service -short`，预期 PASS。
- [ ] **Step 5: 建议提交信息** `feat(service): add manage errors, member.left/room.kicked events, tx retry and local-day bounds`

---

### Task 3: ManageService：移除与退出

**Files:**
- Create: `internal/service/room_manage_service.go`
- Modify: `internal/service/join_helpers_test.go`（给 `joinEnv` 加 `mgr *ManageService` 与辅助函数）
- Test: `internal/service/room_manage_service_test.go`

**Interfaces:**
- Consumes：Task 1 的仓储方法、Task 2 的错误与事件
- Produces：
  ```go
  type ManageConfig struct{ CodeTTLDays, RegenDailyLimit, LifetimeCap int }
  func NewManageService(users UserStore, rooms RoomStore, members RoomMemberStore, codes InviteCodeStore,
      bans BanStore, tx TxRunner, pub Publisher, cfg ManageConfig, now func() time.Time) *ManageService
  func (s *ManageService) Remove(ctx context.Context, ownerID, roomID, targetID string) (model.RoomSummary, error)
  func (s *ManageService) Leave(ctx context.Context, userID, roomID string) error
  ```
  内部 `ownerCheck(ctx, userID string, room model.RoomState, found bool) error`（Task 4、5 复用）：`!found` → `ErrRoomNotFound`；`room.OwnerID==userID` → nil；否则 `IsActiveMember` 为真 → `ErrNotRoomOwner`，否则 `ErrRoomNotFound`
- 测试夹具产出：`joinEnv.mgr`（`RegenDailyLimit:3, LifetimeCap:20, CodeTTLDays:7`，事务器与 `e.pub` 复用）；`(e *joinEnv) fill(t, ownerID string, n int) []string`：新建 `n` 个 18_plus 用户并用房主的额外码逐个加入，返回他们的 user id；`(e *joinEnv) roomState(t, roomID string) model.RoomState`；`(e *joinEnv) assertConsistent(t, roomID string)`：`member_count` 等于在席行数、`<=capacity`、`status=='full'` 当且仅当等于容量

**加锁流程：**
- `Remove`：`tx` 内 `LockRooms(roomID)` → `ownerCheck`；`targetID==ownerID` → `ErrCannotRemoveOwner`；`Leave(roomID, target, "kicked", now)`，`left=false` 则直接返回当前摘要（幂等、无事件、不写封禁）；否则 `Ban` → `Vacate` → `ListMembers`；提交后发 `member.left{reason:"kicked"}`（给剩余在席成员，含房主）与 `room.kicked{reason:"kicked"}`（给被移除者）
- `Leave`：`tx` 内 `LockRooms` → 找不到 `ErrRoomNotFound`；房主 → `ErrOwnerCannotLeave`；调用者在席则 `Leave(..., "left", now)` + `Vacate`，提交后发 `member.left{reason:"left"}` 给剩余成员；不在席则 `EverMember` 为真返回 nil（幂等），否则 `ErrRoomNotFound`

- [ ] **Step 1: 写失败的测试**（`room_manage_service_test.go`，全部用 `newJoinEnv`）：
  - `TestRemove_Success`：房主 + 2 成员，移除一人 → 返回 `member_count==2`、被移除者 `left_reason='kicked'`、`room_bans` 有行、座位号被释放；事件 `member.left` 发给房主与另一成员、`room.kicked` 发给被移除者，`reason` 均为 `kicked`
  - `TestRemove_FullRoomReopens`：5 人满员（unused 码已 void）移除一人 → `status=='open'`，原先被作废的码仍为 `void`
  - `TestRemove_Idempotent`：再移除同一人 → 返回成功、无新事件、`room_bans` 不变；对已主动退出的人同样
  - `TestRemove_Authorization`：非房主在席成员 → `ErrNotRoomOwner`；陌生人 → `ErrRoomNotFound`；不存在的房间与 `"abc"` → `ErrRoomNotFound`
  - `TestRemove_OwnerTarget`：目标是房主 → `ErrCannotRemoveOwner`，`room_bans` 无行；`targetID=="abc"` → 成功且无副作用
  - `TestRemove_BannedCannotRejoin`：被移除者用同房间新码入房 → `ErrCodeInvalid`（与 B3a 的拉黑语义一致）；用旧码重放同样 `ErrCodeInvalid`
  - `TestLeave_Success`：成员退出 → `left_reason='left'`、`member_count` 减 1、满员房恢复 `open`、不写 `room_bans`、`member.left{reason:"left"}` 发给剩余成员；之后可用新码重新加入且不重复发奖励（`reward_grants` 仍一行）
  - `TestLeave_OwnerRejected`：房主 → `ErrOwnerCannotLeave`
  - `TestLeave_FormerMemberIdempotent`：已退出者、被移除者、被解散者再调 → nil，无事件；从没进过的人 → `ErrRoomNotFound`；被移除者与主动退出者返回值完全一致
  - `TestRemove_EventsSurviveCancel`：用 `cancelAfterTx` 包住事务器，提交后立即取消 ctx，事件仍被发布
- [ ] **Step 2: 运行确认失败** — `go test ./internal/service -run "TestRemove_|TestLeave_" -v`
- [ ] **Step 3: 实现** `ManageService`、`NewManageService`、`Remove`、`Leave`、`ownerCheck` 与夹具辅助函数；事件在 `tx` 返回之后用 `context.WithoutCancel` 发布，与 `JoinService.Join` 同样处理。
- [ ] **Step 4: 运行确认通过** — `go test ./internal/service -run "TestRemove_|TestLeave_" -v`
- [ ] **Step 5: 建议提交信息** `feat(service): add owner remove-member and member leave`

---

### Task 4: ManageService：解散

**Files:**
- Modify: `internal/service/room_manage_service.go`
- Test: `internal/service/room_manage_service_test.go`

**Interfaces:**
- Consumes：Task 3 的 `ownerCheck`、夹具
- Produces：`func (s *ManageService) Dissolve(ctx context.Context, ownerID, roomID string) (model.RoomSummary, error)`

**流程（严格此顺序）：** ① `Peek(roomID)` → `ownerCheck`，失败直接返回（不为非房主锁任何码，避免被无关用户拖住入房者）；② `tx` 内 `LockUnusedForRoom(roomID)`（code 升序）；③ `LockRooms(roomID)` 并再 `ownerCheck`；④ `VoidUnusedForRoom` → `holders`；⑤ `DissolveMembers(now)` → `removed`；⑥ `Reset`；⑦ 提交后对 `removed` 每人发 `room.kicked{reason:"dissolved"}`，对 `holders` 发 `code.invalidated{reason:"dissolved"}`。不写 `room_bans`。已是只剩房主的房间：照常执行并返回成功，`removed` 为空则不发 `room.kicked`

- [ ] **Step 1: 写失败的测试**：
  - `TestDissolve_Success`：房主 + 3 成员 + 2 个 unused 码 → 返回 `member_count==1,status=='open'`；成员行 `left_reason='dissolved'` 且房主仍在席；unused 码全部 `void`；`room_bans` 为空；`room.kicked` 发给 3 位成员、`code.invalidated` 发给房主
  - `TestDissolve_FullRoom`：满员房解散后 `status=='open'`
  - `TestDissolve_Idempotent`：连续解散两次均成功，第二次无事件
  - `TestDissolve_Authorization`：非房主成员 → `ErrNotRoomOwner`；陌生人、`"abc"`、不存在 → `ErrRoomNotFound`；失败时该房的 unused 码状态未变
  - `TestDissolve_MembersCanRejoinOnceWithoutSecondReward`：被解散者用房主**新**补的码重新加入成功，`reward_grants` 仍只有一行、`rewards` 为 `[]`；用解散前的旧码 → `ErrCodeInvalid`
- [ ] **Step 2: 运行确认失败** — `go test ./internal/service -run "TestDissolve_" -v`
- [ ] **Step 3: 实现** `Dissolve`。
- [ ] **Step 4: 运行确认通过** — `go test ./internal/service -run "TestDissolve_" -v`
- [ ] **Step 5: 建议提交信息** `feat(service): add owner dissolve with codes-before-room lock order`

---

### Task 5: ManageService：自助补码

**Files:**
- Modify: `internal/service/room_manage_service.go`
- Test: `internal/service/room_manage_service_test.go`

**Interfaces:**
- Consumes：Task 2 的 `LocalDayBounds`、`CodeIssueLimitError`；`NewCode`、`maxCodeAttempts`（与 `JoinService.insertReward` 同样的主键冲突重试 5 次）
- Produces：`func (s *ManageService) RegenCode(ctx context.Context, ownerID, roomID string) (model.CodeInfo, error)`

**流程：** `tx` 内 `LockRooms(roomID)` → `ownerCheck`；`LoadProfile(ownerID, lock=false)` 取 `tz`（不对用户行加锁，避免与入房事务形成环；`invite_codes.issued_to` 的外键只取 KEY SHARE，与入房的 `FOR NO KEY UPDATE` 兼容）；按下列顺序判定，第一条命中即 `*CodeIssueLimitError`：
1. `room.Status != "open"` → `room_full`
2. `CountUnusedValid(roomID, now) >= capacity-member_count` → `enough_unused`
3. `CountRegenBetween(owner, LocalDayBounds(now, tz))` `>= cfg.RegenDailyLimit` → `daily_limit`
4. `LifetimeCount(owner) >= cfg.LifetimeCap` → `lifetime_limit`

通过则 `InsertRegen(issuedAt=now, expiresAt=now+CodeTTLDays 天，UTC，截断到微秒)`；不发事件；返回 `CodeInfo`。

- [ ] **Step 1: 写失败的测试**：
  - `TestRegen_Success`：新用户（空位 4、有 1 个 signup 码）补码成功，库中 `source='owner_regen'`、`issued_to` 为房主、过期时间为 `now+7d`；返回值与库中一致
  - `TestRegen_Reasons`（表驱动，断言 `Reason` 与判定顺序）：满员房 → `room_full`；`unused 有效数 == 空位数` → `enough_unused`；已补满当日上限（`RegenDailyLimit` 设小，用 `InsertRegen` 直接造数）→ `daily_limit`；`LifetimeCap` 设 1（signup 码已占 1）→ `lifetime_limit`；同时满足多条时按上面顺序取第一条
  - `TestRegen_DayBoundary`：房主 `tz='Asia/Seoul'`，昨天本地 23:59 补过的码不计入今天；本地 0:00 之后的计入；`tz` 为空或非法 → 按 UTC，不报错
  - `TestRegen_ExpiredCodesDontCount`：已过期的 unused 码不占"未使用有效码"名额
  - `TestRegen_AfterDissolve`：解散后（成员数 1）可补码
  - `TestRegen_Authorization`：非房主成员 → `ErrNotRoomOwner`；陌生人、`"abc"` → `ErrRoomNotFound`
  - `TestRegen_CodeCollisionRetries`：把 `s.newCode` 替换成先返回已存在的码再返回新码 → 成功且只插入一行；一直冲突 5 次 → 返回错误且不是 `CodeIssueLimitError`
- [ ] **Step 2: 运行确认失败** — `go test ./internal/service -run "TestRegen_" -v`
- [ ] **Step 3: 实现** `RegenCode`（`ManageService` 加 `newCode func() (string, error)` 字段，默认 `NewCode`）。
- [ ] **Step 4: 运行确认通过** — `go test ./internal/service -run "TestRegen_" -v`
- [ ] **Step 5: 建议提交信息** `feat(service): add owner self-service code regeneration`

---

### Task 6: HTTP 层与接线

**Files:**
- Create: `internal/handler/room_manage_handler.go`
- Modify: `internal/handler/room_handler.go`（`manage` 字段、`WithManage`、`Routes`、`failRoom`）、`cmd/api/main.go`、`internal/server/contract_test.go`（`NewRoomHandler(nil,nil,authn).WithManage(nil)`）
- Test: `internal/handler/room_manage_handler_test.go`（用 `newApp`，见 `helpers_test.go`）

**Interfaces:**
- Consumes：`ManageService` 四个方法
- Produces：`func (h *RoomHandler) WithManage(m *service.ManageService) *RoomHandler`；路由（均需登录，挂在 `/rooms` 组）：`DELETE /:id/members/:user_id`、`POST /:id/dissolve`、`POST /:id/codes`、`POST /:id/leave`
- `failRoom` 新增映射：`ErrRoomNotFound`→404 `ROOM_NOT_FOUND`；`ErrNotRoomOwner`→403 `NOT_ROOM_OWNER`；`ErrOwnerCannotLeave`→409 `OWNER_CANNOT_LEAVE`；`ErrCannotRemoveOwner`→400 `VALIDATION_FAILED`；`*CodeIssueLimitError`→409 `CODE_ISSUE_LIMIT` 且 `details:{reason}`
- `main.go`：`retryTx := service.RetryTx{Inner: tx, Retryable: repository.IsRetryableTxError}`；`NewJoinService` 与 `NewManageService` 都传 `retryTx`；`ManageConfig{CodeTTLDays: cfg.CodeTTLDays, RegenDailyLimit: cfg.OwnerRegenDailyLimit, LifetimeCap: cfg.RewardCodeLifetimeCap}`；`NewRoomHandler(...).WithManage(manageSvc)`

- [ ] **Step 1: 写失败的 HTTP 测试**（`room_manage_handler_test.go`）：
  - `TestRemove_HTTP`：200，body `data.room.member_count` 正确；被移除者随后 `GET /rooms/mine` 不含该房；被移除者用新码 `POST /rooms/join` → 404 `CODE_INVALID`
  - `TestLeave_HTTP`：成员 204 且 body 为空；再调一次 204；房主 409 `OWNER_CANNOT_LEAVE`；陌生人 404 `ROOM_NOT_FOUND`；被移除者 204 且响应与主动退出者逐字节相同（状态码、body）
  - `TestDissolve_HTTP`：200，`member_count==1`；非房主成员 403 `NOT_ROOM_OWNER`；陌生人 404
  - `TestRegen_HTTP`：201，`data.code` 与 `data.expires_at` 存在；构造满员房 → 409 `CODE_ISSUE_LIMIT` 且 `error.details.reason=="room_full"`；其余三个 reason 各一例（用 Task 5 的造数方式）
  - `TestManage_ErrorMapping`（表驱动）：覆盖 `NOT_ROOM_OWNER`、`ROOM_NOT_FOUND`、`OWNER_CANNOT_LEAVE`、`VALIDATION_FAILED`（房主移除自己）的 HTTP 状态与 `error.code`
  - `TestManage_BadIDs`：`/rooms/abc/leave`、`/rooms/abc/dissolve`、`/rooms/abc/codes`、10KB 的 `{id}` → 404 `ROOM_NOT_FOUND`；`DELETE /rooms/{有效房间}/members/abc` 由房主调用 → 200 幂等；无 token → 401
  - `TestManage_NoTokenInErrors`：所有错误响应 body 不含完整验证码
- [ ] **Step 2: 运行确认失败** — `go test ./internal/handler -run "TestRemove_HTTP|TestLeave_HTTP|TestDissolve_HTTP|TestRegen_HTTP|TestManage_" -v`
- [ ] **Step 3: 实现** 处理器（统一用 `UserID(c)`、`c.Param`；成功响应 `OK(c, 200, model.RoomResult{Room: summary})`、`OK(c, 201, codeInfo)`、`c.Status(204)`）与 `failRoom` 映射、`main.go` 接线、契约测试的构造调用。
- [ ] **Step 4: 运行确认通过** — `go build ./... && go test ./internal/handler ./internal/server -short`（契约测试此时会因 openapi 缺少四个操作而失败，留给 Task 8；先确认 handler 包 PASS）
- [ ] **Step 5: 建议提交信息** `feat(api): expose remove, dissolve, regen-code and leave room endpoints`

---

### Task 7: 并发测试

**Files:**
- Create: `internal/service/manage_concurrency_test.go`（沿用 `join_concurrency_test.go` 的栅栏与 `concurrentTimeout`；先读它再写）
- Modify（仅当需要）：`internal/service/join_helpers_test.go`

**Interfaces:**
- Consumes：Task 3–5 的服务与 `assertConsistent`

每个测试都在 `concurrentTimeout` 内完成，超时即判死锁失败；每轮结束调用 `assertConsistent`，并断言没有任何 goroutine 返回 `40P01`（若因重试装饰器放过则测试用裸 `TxManager`，保证死锁会暴露而不是被重试掩盖）。

- [ ] **Step 1: 写测试**
  - `TestConcurrent_RemoveVsJoinLastSeat`：房主 + 3 成员（剩 1 席），N=8 个新用户抢这一席，同时房主反复移除成员；20 轮；每轮后 `member_count<=capacity`、行数一致、`status` 与计数一致，恰有 ≤1 个新用户新入房成功
  - `TestConcurrent_DissolveVsJoin`：房主 + 2 成员 + 多个 unused 码，4 个新用户并发入房 + 房主并发解散；20 轮；无死锁、`assertConsistent`；若解散在入房之后完成，则房内只剩房主
  - `TestConcurrent_DissolveVsFillRoom`：房间剩最后一席，一个入房者填满房间（作废剩余码）与房主解散并发；20 轮；无死锁（这是 TRD 7.8 点名的锁顺序用例：解散先锁码再锁房间）
  - `TestConcurrent_RegenSpaceBound`：`RegenDailyLimit=10`，房主空位 4、已有 1 个 unused 码，10 个并发 `RegenCode` → 恰好 3 个成功、其余全是 `enough_unused`；库中 unused 有效码数 == 空位数
  - `TestConcurrent_RegenDailyBound`：`RegenDailyLimit=2`、空位足够多（用 `InsertRegen` 之外的方式不要预占，必要时用较大的空位），10 个并发 → 恰好 2 个成功、其余 `daily_limit`
  - `TestConcurrent_LeaveVsRemoveSameMember`：同一成员同时 `Leave` 与被 `Remove`，20 轮：`member_count` 只减 1，成员行只有一个 `left_reason`
- [ ] **Step 2: 运行** — `go test ./internal/service -run "TestConcurrent_(Remove|Dissolve|Regen|Leave)" -count=1 -v`，预期全部 PASS。若出现 `40P01`，**先停下检查加锁顺序**，不要靠重试掩盖。
- [ ] **Step 3: 稳定性** — `go test ./internal/service -run "TestConcurrent_" -count=5`，预期 PASS。
- [ ] **Step 4: 建议提交信息** `test(service): cover remove/dissolve/regen/leave concurrency`

---

### Task 8: OpenAPI、文档与冒烟

**Files:**
- Modify: `api/openapi.yaml`、`后端技术设计文档（TRD）：PetBridge.md`、`docs/superpowers/specs/2026-11-15-b3-room-join-design.md`、`scripts/smoke.ps1`
- Test: `internal/server/contract_test.go`（已有，无需改动逻辑）

- [ ] **Step 1: 确认契约测试红** — `go test ./internal/server -run TestOpenAPI_RoutesMatchDocument -v`，预期失败并列出缺少的四个操作。
- [ ] **Step 2: 补 `openapi.yaml`**：`info.version` 升 `0.4.0`，`description` 去掉"arrives in B3b"；四个操作各带 `tags:[rooms]`、`security`、`{id}`（`user_id`）参数、成功响应（200 `RoomResult`、201 `CodeInfo`、204）与错误响应 `401/403 NOT_ROOM_OWNER/404 ROOM_NOT_FOUND/409 OWNER_CANNOT_LEAVE|CODE_ISSUE_LIMIT/400`；`CODE_ISSUE_LIMIT` 写明 `details.reason` 四个取值；`leave` 的 description 写明"曾经是成员一律 204"。新增 schema `RoomResult`。
- [ ] **Step 3: 运行确认通过** — `go test ./internal/server -v`，预期 PASS。
- [ ] **Step 4: 同步文档**：
  - spec 5.6 末段改为本计划 Global Constraints 里的"越权与存在性规则"，并在第 9 节新增两条决定："leave 对曾经成员幂等 204"、"解散 Peek 先于锁码"
  - TRD 7.8：`leave` 补一句幂等规则；第 8 章若未列 `ROOM_NOT_FOUND` 则补入
  - spec 第 5.1 末尾"B3b 可对 40P01/40001 做一次有界重试"改为"已由 `RetryTx` 在 `main.go` 接线"
- [ ] **Step 5: 扩展 `scripts/smoke.ps1`**：在现有 B3 入房流程之后追加——第二个临时用户 `POST /rooms/{房间}/leave` 期望 204；`GET /rooms/mine` 该用户只剩 1 个房间；第一个用户 `POST /rooms/{自己的房间}/codes` 期望 201 且返回 `code`；第一个用户 `POST /rooms/{自己的房间}/dissolve` 期望 200 且 `member_count==1`。失败输出沿用 `FAIL ...` + `exit 1` 的风格；`FULL2345`、`DEVA2345` 夹具不碰。
- [ ] **Step 6: 全量验证** — `go vet ./... && go test ./... -count=1`（需 PostgreSQL 与 Memurai 已启动），再 `task seed` 与 `pwsh scripts/smoke.ps1` 各跑一遍，预期全部通过。
- [ ] **Step 7: 建议提交信息** `docs(api): document B3b endpoints and settle leave/authorization rules`

---

## Self-Review

- **Spec 覆盖**：5.6 四个接口 → Task 3/4/5/6；5.7 事件 → Task 2/3/4；5.8 三个新错误码映射 → Task 6；8 节房主管理、并发 6/7/8 → Task 3–5/7；7 节文档与 openapi → Task 8；5.1 的 `40P01` 重试 → Task 2 + 接线（Task 6）。
- **类型一致**：`Leave`/`DissolveMembers` 的 `at time.Time`、`InsertRegen` 的 `issuedAt`、`ManageConfig` 字段、`ownerCheck` 签名在 Task 1/3/4/5 一致；`RoomResult` 只在 Task 1 定义、Task 6 使用。
- **已知取舍**：解散对非房主不锁码（先 `Peek`），所以不会被陌生人拖住；`Peek` 与后续加锁之间房主身份不会变（`owner_id` 不可变），只有在席状态可能变，影响的仅是 403 与 404 的选择。
