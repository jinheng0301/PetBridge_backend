# B6b 打卡与装扮 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 小屋成员每天打卡攒能量，全员打卡有额外能量，能量到里程碑自动解锁装扮，状态随快照和 WS 事件同步。

**Architecture:** 打卡是一个锁房间行的事务：写 `checkins`、判断并“用掉”全员奖励、累加 `room_progress`、写 `room_decor`，提交后发两个不带 seq 的事件。目录、数值、时区计算是纯函数。快照和 `GET /rooms/{id}/decor` 读同一组仓储方法。

**Tech Stack:** Go、Gin、pgx、goose 迁移。无新依赖。

**Spec:** `docs/superpowers/specs/2026-11-25-b6b-checkin-decor-design.md`。规则与数值看第 5 章，事务看第 7 章，契约看第 8 章，文案看第 9 章，文档清单看第 10 章。

## Global Constraints

- **不提交、不推送**：每个任务测试通过后停下，改动留在工作区。全部完成后交用户审阅，同意后再按任务分阶段提交（conventional commits，带 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`），推送 `jinheng main`。
- **分层**：`handler → service → repository`；service 不写 SQL、不依赖 Gin；时间一律用注入的 `now()`。
- **测试**：数据库测试用 `dbtest.Reset(t)`；串行 `go test -p 1`，同一时间只跑一个测试进程；完整的 service 套件放后台跑。开跑前 PostgreSQL 要在，Redis 用 `memurai-cli.exe ping` 确认（没起就 `task up`）。
- **数值**：打卡 +10；全员奖励 +20；全员至少 2 人，只算在席且 `users.status='active'` 的人；窗口 `created_at > now - 24h`；限流每用户每分钟 10 次。
- **目录**（顺序即阈值顺序）：`wall_clock` 10、`picture_frame` 40、`floor_lamp` 80、`string_lights` 140、`potted_tree` 220、`wall_map` 320、`woven_basket` 440、`bookshelf` 580、`mailbox` 740、`yarn_ball` 920、`record_player` 1120、`wind_chime` 1340。
- **事件**：`checkin.updated`、`decor.unlocked` 都是 `KindTransient`（不带 seq），`To` 是全部在席成员含打卡者。
- **没有新错误码**：不在席、房间不存在、id 格式错误都是 `ROOM_NOT_FOUND` 404。
- **只增不减**：能量、解锁记录、打卡记录不因退出、踢人、解散而变化。
- **不改 `GET /rooms/mine`、`member.joined` 的结构**：新字段只进快照。

## Review Focus

1. **时区为空或无效的用户打卡**：按 UTC 算，不报 500。（Task 2 `TestLocalDay_BadZoneIsUTC`）
2. **已删除（`status='deleted'`）的成员还占着席位**：不算进“全员”，其他人照常拿奖励。（Task 1 `TestClaimFullHouse_IgnoresInactive`）
3. **打完卡退出、当天又回来**：再打卡返回 `already`，能量不变；之前那条仍可用于全员奖励。（Task 3 `TestCheckin_RejoinSameDay`）
4. **房间里有目录已不认识的旧装扮**：快照和装扮列表都不报错，列表仍是 12 件，快照照常返回旧的那件。（Task 4 `TestSnapshot_UnknownDecorKept`、Task 3 `TestDecor_UnknownItemIgnored`）
5. **提交后客户端断开**：事件照发，能量已入账，重试得到 `already`。（Task 3 `TestCheckin_PublishesAfterClientGone`）

---

## File Structure

| 文件 | 职责 |
| :-- | :-- |
| `db/migrations/0007_checkin_decor.sql`（新） | `checkins`、`room_progress`、`room_decor` |
| `internal/model/checkin.go`（新） | 打卡与装扮的 DTO |
| `internal/model/event.go`（改） | 快照新字段、两个帧类型常量 |
| `internal/repository/checkin_repo.go`（新） | 三张表的读写 |
| `internal/service/decor.go`（新） | 目录、数值、阈值计算（纯函数） |
| `internal/service/local_day.go`（改） | 加 `LocalDay` |
| `internal/service/checkin_service.go`（新） | 打卡事务、装扮列表 |
| `internal/service/events.go`、`ports.go`（改） | 两个事件、`CheckinStore` |
| `internal/service/realtime_service.go`（改） | 快照带打卡与装扮 |
| `internal/handler/checkin_handler.go`（新） | 两个接口 |
| `cmd/api/main.go`、`api/openapi.yaml`、`internal/server/contract_test.go`（改） | 装配与契约 |
| 两个仓库的六份文档（改） | Task 6 |

worker 不用改。

---

### Task 1: 迁移与仓储

**Files:**
- Create: `db/migrations/0007_checkin_decor.sql`、`internal/model/checkin.go`、`internal/repository/checkin_repo.go`
- Modify: `internal/platform/db/migrate_test.go`（`TestMigration0003_AddsRewardForColumn` 的回滚循环 4 → 5，注释改成 0007 最新）
- Test: `internal/repository/checkin_repo_test.go`（用现有 `mkUser`、`mkRoom`、`mkMember`）

**Interfaces:**
- Produces（`model`）：
  ```go
  type DecorUnlock struct { ItemID string `json:"item_id"`; UnlockedAt time.Time `json:"unlocked_at"` }
  // MemberCheckin is a seated member's time zone and latest check-in day ("2006-01-02"), nil when none.
  type MemberCheckin struct { UserID string; TZ *string; LastDay *string }
  ```
- Produces（`repository.CheckinRepo`，`NewCheckinRepo(pool)`）：
  ```go
  InsertCheckin(ctx, roomID, userID, localDay string, energy int, at time.Time) (inserted bool, err error)
  ClaimFullHouse(ctx, roomID string, now time.Time, window time.Duration) (claimed bool, err error)
  AddEnergy(ctx, roomID string, delta int, at time.Time) (total int, err error)   // upsert
  Energy(ctx, roomID string) (int, error)                                          // 没有行返回 0
  UnlockDecor(ctx, roomID string, itemIDs []string, at time.Time) (added []string, err error) // 只返回新写入的，顺序不保证
  ListDecor(ctx, roomID string) ([]model.DecorUnlock, error)                       // 按 unlocked_at, item_id；非 nil
  MemberCheckins(ctx, roomID string) ([]model.MemberCheckin, error)                // 在席成员
  ```

- [ ] **Step 1：写失败的测试**
  - `TestInsertCheckin_OncePerDay`：同一 `(room,user,"2026-11-25")` 第二次 `inserted=false`；换一天、换房间都为 true。
  - `TestAddEnergy_Accumulates`：`Energy` 初始 0；`AddEnergy(10)` 返回 10，再 `AddEnergy(30)` 返回 40；`Energy` 为 40。
  - `TestUnlockDecor_OnlyNew`：先解锁 `["wall_clock"]`，再解锁 `["wall_clock","picture_frame"]`，第二次只返回 `["picture_frame"]`；空切片返回空、不报错；`ListDecor` 两件；空房间 `ListDecor` 非 nil。
  - `TestClaimFullHouse`（`now` 固定为 T）：
    - 1 人在席、打过卡 → false。
    - 2 人，只一人打过 → false，且那条 `bonus_at` 仍为 null。
    - 2 人都打过 → true，两条 `bonus_at = T`；马上再调 → false。
    - 一人的打卡在 `T-24h` 整 → false；在 `T-24h+1s` → true。
    - 一人窗口内有两条（`T-23h`、`T-1h`，不同 `local_day`）→ true，只有 `T-23h` 那条被用掉。
    - 已退出的人没打卡不影响；另一间房的打卡不算数。
  - `TestClaimFullHouse_IgnoresInactive`：3 人在席，第三人分别为 `banned`、`deleted` 且没打卡 → true；3 人里只有 1 人 active → false（不足 2 人）。
  - `TestMemberCheckins`：返回在席成员及 tz；`LastDay` 是该房间最大的 `local_day`，没打过为 nil；已退出的人不在内。

- [ ] **Step 2：确认失败**
  - 运行：`go test -p 1 ./internal/repository/ -run 'Checkin|AddEnergy|UnlockDecor|ClaimFullHouse'`
  - 期望：编译失败。

- [ ] **Step 3：写迁移**：SQL 照抄 spec 第 6 章，`-- +goose Up` / `-- +goose Down`（Down 依次 drop `room_decor`、`room_progress`、`checkins`）。

- [ ] **Step 4：实现仓储**
  - 所有方法走 `conn(ctx, r.pool)`。
  - `ClaimFullHouse` 必须是**一条语句**：CTE `house`（在席且 active 的成员）、CTE `pick`（`house` 中每人窗口内 `bonus_at is null` 的最早一条，`distinct on (user_id) … order by user_id, created_at`），`update checkins set bonus_at=$now from pick … where (select count(*) from house) >= 2 and (select count(*) from house) = (select count(*) from pick)`；`claimed = RowsAffected > 0`。
  - 时间参数用 `at`/`now`，不用数据库的 `now()`。

- [ ] **Step 5：确认通过**
  - 运行：`go test -p 1 ./internal/repository/ ./internal/platform/db/`
  - 期望：PASS。

---

### Task 2: 目录与日期的纯函数

**Files:**
- Create: `internal/service/decor.go`
- Modify: `internal/service/local_day.go`
- Test: `internal/service/decor_test.go`、`internal/service/local_day_test.go`

**Interfaces:**
- Produces：
  ```go
  const ( CheckinEnergy = 10; FullHouseBonus = 20; FullHouseWindow = 24 * time.Hour )
  type DecorItem struct { ID string; Threshold int }
  func DecorCatalog() []DecorItem           // 副本，按阈值升序
  func NextThreshold(total int) *int        // 第一个 > total 的阈值；都达到返回 nil
  func ItemsReached(total int) []string     // threshold <= total 的 id，按目录顺序，非 nil
  func LocalDay(now time.Time, tz *string) (day string, next time.Time)
      // day 为 "2006-01-02"；next 是当地下一个零点，UTC 表示；时区规则与 LocalDayBounds 相同
  func CheckinExpiry(now time.Time, tz, lastDay *string) *time.Time
      // lastDay 等于当地今天 → next；否则 nil
  ```

- [ ] **Step 1：写失败的测试**
  - `TestDecorCatalog_Fixed`：12 件，id 与阈值同 Global Constraints，阈值严格递增，id 不重复；改返回的切片不影响下一次调用。
  - `TestNextThreshold`：0→10、9→10、10→40、1339→1340、1340→nil、9999→nil。
  - `TestItemsReached`：0→`[]`（非 nil）；10→`["wall_clock"]`；100→前 3 件；1340→12 件。
  - `TestLocalDay`（表驱动，`now = 2026-11-15T20:00:00Z`）：
    - `Asia/Tokyo` → `"2026-11-16"`，next `2026-11-16T15:00:00Z`。
    - `America/Los_Angeles` → `"2026-11-15"`，next `2026-11-16T08:00:00Z`。
  - `TestLocalDay_DST`：`America/New_York`，`now = 2026-11-01T12:00:00Z`（切回冬令时当天）→ `"2026-11-01"`，next `2026-11-02T05:00:00Z`。
  - `TestLocalDay_BadZoneIsUTC`：nil、`""`、`"Nope/Zone"` 都按 UTC。
  - `TestCheckinExpiry`：lastDay 是今天 → next；昨天 → nil；nil → nil。

- [ ] **Step 2：确认失败**
  - 运行：`go test -short ./internal/service/ -run 'Decor|NextThreshold|ItemsReached|LocalDay|CheckinExpiry'`
  - 期望：编译失败。

- [ ] **Step 3：实现**：`LocalDay` 复用 `LocalDayBounds`（`from` 的日期就是当地日期，`to.UTC()` 是 next）。

- [ ] **Step 4：确认通过**：同 Step 2 的命令，期望 PASS。

---

### Task 3: `CheckinService`

**Files:**
- Create: `internal/service/checkin_service.go`
- Modify: `internal/service/ports.go`（`CheckinStore`，方法与 Task 1 的 `CheckinRepo` 完全一致）、`internal/service/events.go`、`internal/model/checkin.go`
- Test: `internal/service/checkin_service_test.go`、`internal/service/checkin_concurrency_test.go`

**Interfaces:**
- Consumes：Task 1、2 的全部。
- Produces：
  ```go
  // service
  EventCheckinUpdated = "checkin.updated"
  EventDecorUnlocked  = "decor.unlocked"
  func NewCheckinService(tx TxRunner, users UserStore, rooms RoomStore, members RoomMemberStore,
      store CheckinStore, pub Publisher, now func() time.Time) *CheckinService
  func (s *CheckinService) Checkin(ctx context.Context, userID, roomID string) (model.CheckinResult, error)
  func (s *CheckinService) Decor(ctx context.Context, userID, roomID string) (model.DecorList, error)

  // model
  type CheckinResult struct {
      Already          bool      `json:"already"`
      EnergyGained     int       `json:"energy_gained"`
      BonusGained      int       `json:"bonus_gained"`
      CheckinExpiresAt time.Time `json:"checkin_expires_at"`
      EnergyTotal      int       `json:"energy_total"`
      NextThreshold    *int      `json:"next_threshold"`
      Unlocked         []string  `json:"unlocked"` // 按目录顺序，非 nil
  }
  type DecorListItem struct {
      ItemID string `json:"item_id"`; Threshold int `json:"threshold"`; UnlockedAt *time.Time `json:"unlocked_at"`
  }
  type DecorList struct {
      EnergyTotal int `json:"energy_total"`; NextThreshold *int `json:"next_threshold"`; Items []DecorListItem `json:"items"`
  }
  ```
  事件 `Data`：
  - `checkin.updated`：`room_id`、`user_id`、`checkin_expires_at`（time.Time）、`energy_total`、`next_threshold`（*int）、`energy_gained`、`bonus_gained`。
  - `decor.unlocked`（每件一条，按目录顺序，排在 `checkin.updated` 之后）：`room_id`、`item_id`、`unlocked_at`。

- [ ] **Step 1：写失败的测试**（用 `newJoinEnv`，用户时区是 `Asia/Tokyo`；服务用自己的可调时钟，起点 `2026-11-15T12:00:00Z`）
  - `TestCheckin_First`：单人小屋 → `Already=false, EnergyGained=10, BonusGained=0, EnergyTotal=10, NextThreshold=40, Unlocked=["wall_clock"], CheckinExpiresAt=2026-11-15T15:00:00Z`；事件恰好两条：`checkin.updated`、`decor.unlocked`，都是 `KindTransient`，`To=[自己]`，字段与上面一致。
  - `TestCheckin_AlreadySameDay`：第二次 `Already=true`，gained 都是 0，`Unlocked` 非 nil 且空，`EnergyTotal=10`，没有新事件；时钟走到 `15:00:00Z`（东京零点）后能再打，`EnergyTotal=20`。
  - `TestCheckin_NotSeated`：陌生人、不存在的房间、`"abc"`、被移除的成员 → `ErrRoomNotFound`，`checkins` 没有行。
  - `TestCheckin_UppercaseRoomID`：大写 id 可用，事件里的 `room_id` 是小写。
  - `TestCheckin_TwoRoomsIndependent`：同一人在自己的和朋友的小屋各打一次，两边都 +10。
  - `TestCheckin_FullHouse`：2 人都打 → 第二人 `BonusGained=20, EnergyTotal=40`，`Unlocked=["picture_frame"]`；事件 `To` 含两人。
  - `TestCheckin_FullHouseFivePeople`：用 `e.seat` 坐满 5 人，前 4 人 `BonusGained=0`，第 5 人 20，总能量 70。
  - `TestCheckin_FullHouseOncePerRound`：A `11-15T00:00Z`、B `11-15T11:00Z`、A `11-16T00:00Z`、B `11-16T11:00Z` → 奖励依次 0、20、0、20，总能量 80。
  - `TestCheckin_NewMemberMustCheckInToo`：A、B 打过之前 C 入房 → B 打卡不发；C 打卡发 20。
  - `TestCheckin_LeaveDoesNotPayUntilNextCheckin`：A 打过，B 打过，C 没打；C 退出 → 没有事件和能量变化；第二天（窗口内）A 打卡 → 20。
    时间：A `11-15T12:00Z`、B `11-15T13:00Z`、A 第二次 `11-15T15:30Z`。
  - `TestCheckin_RejoinSameDay`：B 打卡 → 退出 → 用新码回来 → 再打 `Already=true`；随后 A 打卡 → 20。
  - `TestCheckin_CrossesSeveralThresholds`：预置 `room_progress=300`、没有 `room_decor` 行 → 打卡后 310，`Unlocked` 是前 5 件，5 条 `decor.unlocked`；预置 1335、前 11 件已解锁 → `NextThreshold=nil`，`Unlocked=["wind_chime"]`。
  - `TestCheckin_KeepsProgressAfterDissolve`：打卡后 `mgr.Dissolve`，`Energy` 和 `ListDecor` 不变，房主第二天打卡继续累加。
  - `TestCheckin_RollsBackTogether`：用 `commitFailTx` → 返回错误，三张表都没有行，没有事件。
  - `TestCheckin_PublishesAfterClientGone`：用 `cancelAfterTx` → 无错误，事件都发了且 `deadCalls=0`；再打 `Already=true`。
  - `TestDecor_List`：没打过卡 → `EnergyTotal=0, NextThreshold=10`，12 件 `UnlockedAt` 全 nil；打一次后第一件有时间；陌生人 → `ErrRoomNotFound`。
  - `TestDecor_UnknownItemIgnored`：`room_decor` 里插入 `old_item` → 列表仍是 12 件。
  - 并发（`checkin_concurrency_test.go`，用 `concurrentPool` + `raceAll`，裸 `TxManager`）：
    - `TestConcurrentCheckin_FiveMembersOneBonus`：5 人同时打 → 能量恰好 70，`BonusGained=20` 的恰好 1 人。
    - `TestConcurrentCheckin_SameUser`：同一人 20 个请求 → 恰好 1 个 `Already=false`，能量 10。
    - `TestConcurrentCheckin_WithRemoveAndJoin`：打卡与移除成员、入房交错，10 轮，没有死锁错误（只允许 `ErrRoomNotFound` 等业务错误），能量等于 `checkins` 的 energy 之和加奖励之和。

- [ ] **Step 2：确认失败**
  - 运行：`go test -p 1 ./internal/service/ -run 'TestCheckin|TestConcurrentCheckin|TestDecor_'`
  - 期望：编译失败。

- [ ] **Step 3：实现**
  - 步骤严格按 spec 第 7 章 1–8。`now()` 在拿到房间锁**之后**取，截到微秒，这样同一间房的 `created_at` 单调。
  - 调用者是否在席、事件的 `To` 都来自锁内的 `ListMembers`。
  - 时区用 `LoadProfile(ctx, userID, false)`，不锁用户行（spec 第 7 章加锁顺序）。
  - `UnlockDecor` 的返回值按目录顺序重排。
  - 发事件用 `context.WithoutCancel`，与 `ManageService.publish` 相同。
  - fn 开头重置外层变量，保证 `RetryTx` 重跑安全。
  - 更新 `repository/tx.go` 的加锁顺序注释：`users -> invite_codes -> rooms -> room_members -> checkins -> room_progress -> room_decor`。

- [ ] **Step 4：确认通过**
  - 运行：同 Step 2 的命令，期望 PASS。

---

### Task 4: 快照

**Files:**
- Modify: `internal/model/event.go`、`internal/service/realtime_service.go`
- Test: `internal/service/realtime_service_test.go`

**Interfaces:**
- Consumes：`CheckinStore.Energy/ListDecor/MemberCheckins`、`NextThreshold`、`CheckinExpiry`。
- Produces：
  ```go
  type RoomProgress struct { EnergyTotal int `json:"energy_total"`; NextThreshold *int `json:"next_threshold"` }
  type SnapshotMember struct { MemberPresence; CheckinExpiresAt *time.Time `json:"checkin_expires_at"` }
  // SnapshotRoom: Members 改为 []SnapshotMember；新增 Progress RoomProgress `json:"progress"`、Decor []DecorUnlock `json:"decor"`
  func (s *RealtimeService) WithCheckins(store CheckinStore, now func() time.Time) *RealtimeService
  ```
  没调用 `WithCheckins` 时：能量 0、阈值 10、`decor` 为 `[]`、`checkin_expires_at` 为 null。

- [ ] **Step 1：写失败的测试**
  - `TestSnapshot_CheckinFields`：没打过卡 → `progress {0, 10}`、`decor` 序列化为 `[]`、每个成员的 JSON 有 `"checkin_expires_at":null`；房主打卡后 → 房主有 `2026-11-15T15:00:00Z`，另一人 null，`progress {10, 40}`，`decor` 一件 `wall_clock`；时钟过东京零点后房主变回 null。
  - `TestSnapshot_UnknownDecorKept`：`room_decor` 有 `old_item` → 快照 `decor` 里有它。
  - `TestSnapshot_StoreErrorFails`：store 返回错误 → `Snapshot` 返回该错误。
  - `TestMemberJoined_HasNoCheckinField`：`member.joined` 事件的 member JSON 里没有 `checkin_expires_at`。

- [ ] **Step 2：确认失败**
  - 运行：`go test -p 1 ./internal/service/ -run 'TestSnapshot|TestMemberJoined_HasNo'`
  - 期望：编译失败。

- [ ] **Step 3：实现**：seq 仍在读成员之前读（`TestSnapshot_SeqReadBeforeMembers` 不能破），打卡数据在读成员之后读。

- [ ] **Step 4：确认通过**
  - 运行：`go test -p 1 ./internal/service/ -run 'Snapshot|PetTap|MemberJoined'` 和 `go test -p 1 ./internal/handler/ -run TestWS`
  - 期望：PASS。

---

### Task 5: 接口、装配、契约

**Files:**
- Create: `internal/handler/checkin_handler.go`
- Modify: `cmd/api/main.go`、`api/openapi.yaml`、`internal/server/contract_test.go`、`internal/handler/helpers_test.go`（`newAppQuota` 注册新 handler，限流用 `counter`）、`internal/handler/ws_handler_test.go`（`rt` 接上 `WithCheckins`，路由里注册新 handler，用同一个 `pub`）
- Test: `internal/handler/checkin_handler_test.go`

**Interfaces:**
- Consumes：`CheckinService`。
- Produces：
  ```go
  func NewCheckinHandler(svc *service.CheckinService, authn gin.HandlerFunc, limits ...gin.HandlerFunc) *CheckinHandler
  // POST /rooms/:id/checkin（authn + limits）、GET /rooms/:id/decor（authn）；错误走 failRoom
  ```

- [ ] **Step 1：写失败的测试**
  - `TestCheckinRoute`：首次 200，`data` 含 spec 8 章的 7 个键，`unlocked` 为 `["wall_clock"]`；重复 200、`already:true`、`"unlocked":[]`；没带 token 401；未 onboarding 的用户 404 `ROOM_NOT_FOUND`；陌生人、`abc` 404。
  - `TestCheckinRoute_RateLimited`：同一用户第 11 次 429 `RATE_LIMITED`，带 `Retry-After`。
  - `TestDecorRoute`：200，`items` 12 条，未解锁的 `unlocked_at` 为 null；陌生人 404。
  - `TestWS_CheckinUpdatedHasNoSeq`（`ws_handler_test.go`）：两人订阅同一房间，一人 POST 打卡 → 两边都收到 `checkin.updated`，帧里没有 `seq`；随后有人入房，`member.joined` 的 seq 等于快照 seq + 1。

- [ ] **Step 2：确认失败**
  - 运行：`go test -p 1 ./internal/handler/ -run 'Checkin|Decor'`
  - 期望：编译失败。

- [ ] **Step 3：实现与装配**
  - `cmd/api/main.go`：`checkins := repository.NewCheckinRepo(pool)`；`RealtimeService` 加 `.WithCheckins(checkins, time.Now)`；`NewCheckinService(retryTx, users, rooms, members, checkins, pub, time.Now)`；限流 `middleware.RateLimitUser(counter, "checkin", 10, time.Minute)`。
  - `contract_test.go` 注册 `handler.NewCheckinHandler(nil, authn)`。

- [ ] **Step 4：改 openapi**
  - 两个路径（`operationId: checkin`、`listDecor`，tag `rooms`），schema `CheckinResult`、`DecorList`、`DecorListItem`，响应 200 / 401 / 403 / 404 / 500，打卡另有 429。
  - 可空字段写 `nullable: true`，全部字段列进 `required`。
  - `info.version` 改 `0.6.0`，`description` 补 B6b；`/api/v1/ws` 描述补打卡事件。

- [ ] **Step 5：全量验证**
  - 运行：`go build ./... && go vet ./... && go test -p 1 ./...`（后台跑）
  - 期望：全部 PASS。
  - 手动冒烟（`task up`、`task migrate`、`task seed`、`task dev`）：用 seed 用户登录打卡，第二次 `already:true`，`GET /rooms/{id}/decor` 第一件有时间。做不了的要在交付时说明。

---

### Task 6: 文档同步

**Files:** 两个仓库根目录的三份主文档与三份变更记录。开工前先确认两边 md5 一致、版本仍是后端 v1.14 / 前端 v1.12 / PRD v1.10。

- [ ] **Step 1：后端 TRD v1.15**：按 spec 第 10 章逐项改。正文标注“（v1.15）”；表头版本列表最上面加一行；`配套文档`引用改为前端 v1.13、PRD v1.11。17 章写“v1.15：B6b 完成”。
- [ ] **Step 2：前端 TRD v1.13**：按 spec 第 10 章逐项改。文案表照抄 spec 第 9 章（含装扮名称）。`checkin.updated` / `decor.unlocked` 从第 4.5 节带 seq 那一组移到不带 seq 的规则，写明合并规则。
- [ ] **Step 3：PRD v1.11**：F5.1、F5.2 补规则；14 章里程碑补 B6b；1.1 版本记录表加一行。
- [ ] **Step 4：三份变更记录**：最上面各加一节 `## V1.x 变更说明`，用 ①②③ 写全 spec 里的【我的决定】和用户的两项决定（合作感、24 小时窗口）。
- [ ] **Step 5：复制到 `../PetBridge_frontend/` 并校验**
  - 运行：`md5sum *.md` 两边对比。
  - 期望：六份文件两两一致。
- [ ] **Step 6：交用户审阅**：列出改动文件、建议的分阶段提交（Task 1–5 各一个 commit，文档一个，spec 与 plan 随文档）；在用户同意前不提交。
