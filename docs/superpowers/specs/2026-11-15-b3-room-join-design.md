# B3 设计：验证码入房、奖励、限流冷却、房主管理

日期：2026-11-15　开发方式：直接在 `main` 上开发（不开 PR、不提交，由用户审查后提交）
依据：后端 TRD v1.2 的 6.2、7.2、7.8、8、11、12.1；PRD v1.4 的 R3、R6、R8、R10、R11；前端 TRD 11.5、11.6。
状态：草稿，待用户确认。

## 1. 意图

让两个已 onboarding 的用户凭一个验证码进入同一间小屋，并且在并发下**不超员、不重复发奖励、不死锁**。这是 TRD 里程碑 M1"核心机制跑通"。前端据此做验证码输入、入房动画、背包和房主管理，所以接口契约与实现同等重要。

## 2. 目标 / 非目标 / 反目标

**目标**
- B3a：`POST /rooms/join`、`GET /codes`、`GET /rooms/mine`；奖励码；输码限流与冷却；登录限流；`FULL2345` 种子
- B3b：`DELETE /rooms/{id}/members/{user_id}`（移除）、`POST /rooms/{id}/dissolve`（解散）、`POST /rooms/{id}/codes`（补码）、`POST /rooms/{id}/leave`（退出）
- 第 8 章新增错误码落地并补进 TRD；`api/openapi.yaml` 补全，契约测试覆盖
- 并发测试（见第 8 节）

**非目标（留给后续）**
- WebSocket 与事件真正送达（B4）。B3 只定义事件并在提交后调用 `Publisher`，实现是空操作
- 推送（B7）、`GET /rooms/{id}` 详情（B4 的 `room.snapshot` 覆盖）、`GET /codes/{code}/share-card`（B7）
- 邀请码过期的后台清理任务。过期靠 `expires_at` 在读取时判断，不改库里的 `status`
- `app_config` 热改。B3 的业务数值先放环境变量配置，`app_config` 读取留到有第一个必须热改的需求时
- 异常邀请树风控（PRD 防刷第 4 点）

**反目标**（出现任何一条即视为失败）
- 同一用户在同一房间被发两次奖励；房间 `member_count` 超过 `capacity`；同一房间同时出现成年人与未成年人
- 被房主移除的用户能从任何响应里看出自己是"被移除"（一律表现为 `CODE_INVALID`）
- Redis 故障导致入房接口 5xx（必须退化为进程内限流）
- 日志、错误里出现完整验证码（只记前 2 位加 `****`）

## 3. 约束

- 沿用 B1/B2：Gin、pgx、分层 `handler → service → repository`、统一响应 `{data}` / `{error:{code,message,request_id}}`、接口前缀 `/api/v1`
- 本地 PostgreSQL + Memurai，无 Docker；数据库测试用 `petbridge_test`，`-short` 跳过；Redis 测试遵循同样的跳过约定
- 不修改 `0001`、`0002`；表结构改动走新迁移 `0003`
- 代码不自动提交，留给用户审查
- SQL 继续手写 pgx，B3 约 24 条查询仍可管理，不引入 sqlc（B2 spec 里的"B3 再评估"结论：不引入，B6 前再看）【我的决定】

## 4. 方案比较

**4.1 切分方式（用户已选 A）**
- **A. 一份 spec，两段实现（采用）**：B3a 入房主路径，B3b 房主管理。B3a 完成后可先审查、先联调前端入房。
- B. 一份 spec、一份计划一次做完：审查量大，联调要等全部完成。弃用。

**4.2 防超员的并发控制**
- **A. 数据库行锁 + 条件更新（采用，TRD 原方案）**：竞争只在同一间房的 5 个席位上，范围天然受限；Redis 丢数据不影响正确性。
- B. Redis Lua 原子脚本（PRD 备选）：Redis 在 TRD 中定位为"可丢失"，不能承担业务正确性。弃用。
- C. SERIALIZABLE 隔离 + 重试：序列化失败要重试，测试难以稳定。弃用。

**4.3 重试幂等**
- **A. 以 `invite_codes.used_by` 判重放（采用）**：成功入房后码已是 `used`，TRD 原流程第 1 步会对重试返回 `CODE_INVALID`，第 3 步的幂等分支实际走不到。改为：码的 `used_by` 就是自己，则按原结果返回（含奖励码）。`Idempotency-Key` 请求头接受但不存储。
- B. 新建幂等表按 key 缓存响应：多一张表和清理任务，而 `used_by` 已能区分重放。弃用。

## 5. 已定的设计

### 5.1 入房 `POST /rooms/join`

请求体 `{ "code": "..." }`，需登录。码规范化：去空白与连字符、转大写；格式不符（长度、字符集）**同样按 `CODE_INVALID` 处理并计入失败**，避免用错误码区分"格式错"与"不存在"。缺少 `code` 字段才返回 `VALIDATION_FAILED`。

**事务前**：未 onboarding → `ONBOARDING_REQUIRED`；限流检查（见 5.3）。

**事务内，加锁顺序 users → invite_codes → rooms → room_members**（与 TRD 一致），具体步骤：

```
0. 锁 users(me) FOR NO KEY UPDATE   串行化同一用户的并发入房（不能用 FOR UPDATE，原因见下方修正 3）
1. 查出码所属房间（不加锁），锁住该房间**全部 unused 码**（按 code 排序 FOR UPDATE，防死锁，见下方修正 4），
   再锁 invite_codes(code) FOR UPDATE
     · 不存在 / expires_at <= now / status 为 expired|void → CODE_INVALID
     · status=used 且 used_by=me → 仅当我此刻仍是该码所属房间的在席成员，才算重放：回读并返回当前房间、座位与首次的奖励码
       （不重复入房、不重复发奖励、不计失败，**也不清零失败计数**，见 5.3）；
       否则（我已退出、被移除或房间已解散）→ CODE_INVALID，计入失败。否则被移除者拿旧码重放会得到 200，暴露"被移除"
       已过期的已用码重放同样是 CODE_INVALID（重放只为客户端数秒内的超时重试而设）
     · status=used 且 used_by≠me → CODE_INVALID
2. 按 id 升序一次锁定两间房：码所属房间、我自己的小屋（SELECT ... WHERE id IN (...) ORDER BY id FOR UPDATE）
3. 业务校验（此时已持有目标房间锁，读到的成员是稳定的）。多条同时成立时按下列顺序，第一条命中即返回：
     · 房间 status='closed' → CODE_INVALID（B3 不会产生 closed，作为防御）
     · 码所属房间的房主是我 → CODE_INVALID
     · room_bans 命中 → CODE_INVALID
     · 我已在该房间在席 → ALREADY_MEMBER（返回房间 id，不消耗码、不发奖励；优先于 ROOM_LIMIT）
     · 我的在席房间数 >= 2 → ROOM_LIMIT
     · 我与房间现有全部成员的成年状态不一致 → AGE_RESTRICTED
     ROOM_LIMIT、AGE_RESTRICTED、ALREADY_MEMBER、ROOM_FULL 都**不消耗码**；它们会让持码者知道码有效，这是可接受的：
     8 位 32 字符的码空间约 1.1 万亿，枚举靠冷却与 IP 限流挡住，不靠隐藏这几种状态
4. UPDATE rooms SET member_count=member_count+1 WHERE id=$r AND status='open' AND member_count<capacity
     → 0 行：ROOM_FULL（不计入失败冷却；此时已持锁，status 只可能是 full 或满额）
5. 取最小空位号，INSERT room_members(role='member')
6. UPDATE invite_codes SET status='used', used_by, used_at
7. INSERT invite_edges（zero 码没有持有人，邀请人记为该房房主）；INSERT reward_grants ON CONFLICT DO NOTHING
     · 本次真正插入才发奖励码，绑定到我自己的小屋（见 5.2）
8. 若 member_count = capacity：rooms.status='full'；该房间所有 unused 码置 void
9. COMMIT；提交后 Publish：member.joined（房内其余成员）、code.invalidated（被作废码的 issued_to 持有人）
```

**对 TRD 7.2 的四处修正**（已同步 TRD）：
1. **先锁房间，再做全部业务校验。** TRD 的步骤让校验读成员时不持有房间锁，而"是否在席""是否满员"在校验与第 4 步之间可能被并发的入房、移除、退出改变。先锁房间后，校验读到的是稳定状态。注：年龄隔离本身不存在并发缺口——房主永远在席且不能退出，房主的年龄段就决定了整间房的类别，成年房永远拒绝未成年人，反之亦然。
2. **同时锁两间房并按 id 升序。** 奖励码要写入"我自己的小屋"，所以一次入房会涉及两间房。A 用码进 B 的房、B 同时用码进 A 的房，如果各自"先锁目标房再锁自己房"会互相等待而死锁。统一按 id 升序一次锁定，两边顺序一致。
3. **用户行用 `FOR NO KEY UPDATE`，不用 `FOR UPDATE`**（并发测试发现）。第 7 步写 `invite_edges.inviter_id` 时，外键检查要给**对方**用户行加 KEY SHARE 锁；A 进 B 的房、B 同时进 A 的房时，双方各自用 `FOR UPDATE` 锁着自己的用户行，会互相等待而死锁。`FOR NO KEY UPDATE` 与自身仍然互斥（同一用户的并发入房、onboarding 照样被串行化），但不与 KEY SHARE 冲突。
4. **先锁该房间全部 unused 码，再锁房间**（并发测试与评审发现）。房间被填满时，赢家要作废该房间所有 unused 码，其中包含另一个正在入房的人已经锁住的码。若各自只锁自己的码，则：甲持有 X1、乙持有 X2，甲锁房间并填满、去作废 X2（乙持有、乙在等房间锁），双方互相等待，死锁。统一为"房间内全部 unused 码按 code 排序锁定 → 房间"，B3b 的**解散必须同样先按此顺序锁码再改房间**，补码（只插入新码）不受影响。
   - 已知的极小窗口：第 1 步锁定 unused 码集合之后、拿到房间锁之前，房主新增了奖励码，且另一人带着排序在前的新码恰好已持锁，仍可能成环。窗口为毫秒级，B3a 不处理；B3b 已由 `service.RetryTx` 在 `main.go` 接线：入房与房主管理事务遇到 `40P01`/`40001` 整体重试一次（重试安全，已提交的入房重试会走重放分支）。

**成功响应** 200：`{ room: {id, status, member_count, capacity}, seat_no, members: [{user_id, nickname, pet_type, seat_no, role}], rewards: [{code, expires_at}] }`。`rewards` 在没有奖励时为 `[]`，不返回 null。

### 5.2 奖励码

- **时机与锁**：数量在第 2 步锁住两间房之后计算。能往我小屋写码的路径（本次入房、补码、onboarding）都先锁我的小屋，所以"未使用码数"与"终身已获得数"的读取是稳定的
- **一次性**：`reward_grants(user_id, room_id)` 在入房时**总是**插入，即使本次数量为 0（`granted_count=0`）。若数量为 0 就不插入，用户可以靠退出再加入同一房间，在腾出空位后刷到奖励【我的决定】
- **发放对象**：只有入房者得到奖励；房主/邀请人不因被邀请入房而得到码，只写 `invite_edges`（TRD 与 PRD 均无邀请人奖励）
- **码都绑定在发码人自己的小屋**（注册码、入房奖励码、补码），所以"成员持有的码属于他加入的他人小屋"不会发生；满员时作废的是房主的码，`code.invalidated` 发给这些码的 `issued_to`
- 数量 = `max(0, min(REWARD_CODE_COUNT, 自己小屋 capacity − member_count − 自己小屋当前未使用且未过期的码数, REWARD_CODE_LIFETIME_CAP − 终身已获得数))`
- 终身已获得数 = `invite_codes` 中 `issued_to = 我` 且 `source <> 'zero'` 的行数（signup、join_reward、owner_regen 全计入，PRD R6 口径），不另建计数表
- 奖励码 `source='join_reward'`，`issued_to=我`，`room_id`=我自己的小屋，有效期 `CODE_TTL_DAYS`
- **迁移 `0003`**：`invite_codes` 新增可空列 `reward_for text references invite_codes(code)`，奖励码记录是哪个码触发的。重放时按它回读奖励码，不依赖时间戳相等之类的巧合。
- 码生成沿用 `service.NewCode`，主键冲突重试最多 5 次（与 onboarding 相同）

### 5.3 限流与冷却（`internal/platform/ratelimit`）

固定窗口计数器接口，Redis 实现 + 进程内实现 + 自动退化的组合实现。进程内实现注入时钟，便于测试。

| 对象 | 规则 | 键 | 超限表现 |
| :-- | :-- | :-- | :-- |
| 输码失败 | 同用户 10 分钟内累计 5 次失败 → 冷却 30 分钟；仅"真正成功入房"清零，重放不清零（否则已入房的用户可靠重放旧码无限重置计数，绕过冷却） | 计数 `rl:join:u:{uid}`，冷却 `cd:join:u:{uid}` | `CODE_COOLDOWN` 429，`details.retry_after_seconds` |
| 输码频率 | 同 IP 每小时 30 次（所有尝试，含成功） | `rl:join:ip:{ip}` | `RATE_LIMITED` 429 + `Retry-After` |
| 登录 | 同 IP 每分钟 10 次，作用于三个 `/auth/login/*` | `rl:login:ip:{ip}` | `RATE_LIMITED` 429 + `Retry-After` |

- **检查顺序**：①用户冷却（命中即 `CODE_COOLDOWN`，**不**计入 IP 次数）→ ②IP 计数 +1，超限 `RATE_LIMITED` → ③`ONBOARDING_REQUIRED` 检查 → ④事务。因此 IP 次数包含未 onboarding 与最终失败的请求，不含被冷却拒绝的请求【我的决定】
- **计入失败**：只有 `CODE_INVALID`（含格式错、房主用自己的码、被移除者、重放资格丧失）。`ROOM_FULL`、`ALREADY_MEMBER`、`ROOM_LIMIT`、`AGE_RESTRICTED` 不计入
- 冷却期间的请求在事务前被拒绝，不查库
- **Redis 不可用**（超时或报错）：自动改用进程内实现，阈值收紧——输码频率改为每 IP 每小时 15 次，其余不变；记一条 warn 日志，不返回错误
- 客户端 IP 取 `c.ClientIP()`；当前不信任任何代理，所以是套接字对端地址，部署到 Caddy 后面时需配置可信代理（已在 B1 记录）
- 5 次 / 10 分钟 / 30 分钟 / 30 次每小时 / 10 次每分钟是 PRD 与 TRD 的固定值，写成代码常量，不进配置

### 5.4 `GET /codes`

返回我名下（`issued_to=我`）的全部码，按 `issued_at` 倒序：`[{ code, source, status, expires_at, room: {id, member_count, capacity} }]`。`status` 是**有效状态**：库里为 `unused` 但已过期的，返回 `expired`。种子 `zero` 码没有 `issued_to`，不会出现。不分页（终身上限 20 加 1 个注册码）。

### 5.5 `GET /rooms/mine`

返回我在席的全部房间，自己的小屋排第一：`[{ id, is_owner, status, member_count, capacity, members: [{user_id, nickname, pet_type, seat_no, role}] }]`。不含已离开的成员，不含黑名单信息。

### 5.6 房主管理（B3b）

全部遵守 7.8 的流程与锁顺序，操作各自一个事务，先锁房间行。

- **移除** `DELETE /rooms/{id}/members/{user_id}`：成功返回 200 `{ room }`；目标已不在席视为成功（幂等）；目标是房主本人 → `VALIDATION_FAILED`；原为 `full` 则恢复 `open`；写 `room_bans`。
- **解散** `POST /rooms/{id}/dissolve`：先作废 unused 码，再移出成员，再改房间；不拉黑；成功返回 200 `{ room }`。**TRD 17 章把"房主解散"排在 B6，我提前到 B3b**：7.8 的测试要求里有"dissolve 与 join 并发不死锁"，这是验证锁顺序的关键用例，而且它和移除共用大部分代码。【我的决定，见第 9 节】
- **补码** `POST /rooms/{id}/codes`：条件与 `details.reason` 取值以 TRD 7.8 为准（`room_full` / `enough_unused` / `daily_limit` / `lifetime_limit`）；"今日"按房主 `users.tz` 的本地日界限计算；成功返回 201 `{ code, expires_at }`。
- **退出** `POST /rooms/{id}/leave`：成功 204；房主调用 → `OWNER_CANNOT_LEAVE`；曾经在席、现已不在席的人（主动退出、被移除、被解散）一律视为成功（204，被移除者与主动退出者看到的结果完全一致）；从没进过该房间 → `ROOM_NOT_FOUND`。
- 房主类操作（移除、解散、补码）：房间不存在，或调用者既不是房主也不是在席成员 → `ROOM_NOT_FOUND` 404（不泄露房间是否存在）；是在席成员但不是房主 → `NOT_ROOM_OWNER` 403。解散先不加锁地 Peek 房间并校验房主，再按 code 升序锁 unused 码，最后锁房间，所以非房主不会去锁别人房间的码。

### 5.7 事件（空实现）

`service.Publisher` 接口，B3 的实现 `NopPublisher`。事件在事务提交**之后**由 service 调用，提交失败不发。事件：`member.joined`、`member.left{reason}`、`room.kicked{reason}`、`code.invalidated`，字段按 TRD 第 9 章。`member.joined` / `member.left` 发给房内其余在席成员，`room.kicked` 发给被移出者，`code.invalidated` 发给被作废码的 `issued_to`。B4 提供真实实现并接入 Redis `ws:events`。

### 5.8 错误码与 HTTP 状态（B3 新增）

| code | HTTP | 说明 |
| :-- | :-- | :-- |
| ONBOARDING_REQUIRED | 403 | 未完成 onboarding（TRD 7.1 约定，补入第 8 章） |
| CODE_INVALID | 404 | 对外不区分原因 |
| ROOM_FULL | 409 | |
| ALREADY_MEMBER | 409 | `details.room_id` |
| ROOM_LIMIT | 409 | |
| AGE_RESTRICTED | 403 | |
| CODE_COOLDOWN | 429 | `details.retry_after_seconds` |
| RATE_LIMITED | 429 | 带 `Retry-After` 头 |
| NOT_ROOM_OWNER | 403 | |
| OWNER_CANNOT_LEAVE | 409 | |
| CODE_ISSUE_LIMIT | 409 | `details.reason` |
| ROOM_NOT_FOUND | 404 | TRD 未列出，新增 |

### 5.9 配置新增

`REWARD_CODE_COUNT`（默认 3，范围 0–5）、`REWARD_CODE_LIFETIME_CAP`（默认 20，≥1）、`OWNER_REGEN_DAILY_LIMIT`（默认 3，≥1）。加入 `config.Config`，校验风格同 `CODE_TTL_DAYS`。

### 5.10 种子数据

`task seed` 在 B2 基础上追加，仍然幂等，且走真实的入房路径：
- 新增 5 个用户：`dev-full@local`（满员小屋房主）与 `dev-fill1..4@local`（占座者），全部 18_plus
- 为 dev-full 的小屋写入 4 个占座用的 zero 码，由 4 个占座用户用真实入房服务加入，小屋满员后码自动作废
- 再写入 `FULL2345`（zero、void、绑定 dev-full 的小屋，有效期 10 年），用于验证"满员后码失效返回 `CODE_INVALID`"
- 不让 A–F 去占座：入房后他们会占掉第二个房间名额，联调"B 用 `DEVA2345` 加入 A"时会得到 `ROOM_LIMIT`
- 重复运行：占座用户的码已 `used_by` 自己，走重放分支，结果不变
- `scripts/smoke.ps1` 扩展：为保持种子夹具不被消耗，冒烟脚本新建两个临时用户，第二个用第一个的 signup 码入房 → `GET /rooms/mine`，断言 `member_count=2`、返回了奖励码、自己的小屋排第一且共 2 个房间（`DEVA2345` 留给人工联调）

### 5.11 代码结构

沿用现有分层（不是 B2 spec 里设想的 `internal/auth` 等包）：
- `internal/service`：`join_service.go`、`room_service.go`（移除/解散/退出）、`code_service.go`（补码、背包）、`events.go`（Publisher）
- `internal/repository`：补全 `room_repo`、`room_member_repo`、`invite_code_repo`，新增 `ban_repo`、`reward_repo`、`edge_repo`（或并入现有文件，由实现者决定）
- `internal/handler`：`room_handler.go`、`code_handler.go`
- `internal/platform/ratelimit`：限流实现；`internal/middleware/ratelimit_middleware.go`：登录限流
- `db/migrations/0003_invite_reward_for.sql`

## 6. 数据库

- 迁移 `0003`：`alter table invite_codes add column reward_for text references invite_codes(code)`；`create index idx_invite_codes_reward_for on invite_codes(reward_for)`
- 现有唯一约束 `uq_member_active`、`uq_seat_active`、`ck_member_cap`、`room_bans` 主键已覆盖 6.2 的要求，不再改动

## 7. 文档同步

- TRD 7.2：写入"先锁房间""两间房升序加锁""重放判定""错误优先级"几处修正（已同步）
- TRD 第 8 章：错误码清单补 `ONBOARDING_REQUIRED`、`ROOM_NOT_FOUND`，并写入 HTTP 状态
- TRD 17 章：房主解散由 B6 提前到 B3
- TRD 15.5：种子数据里 `FULL2345` 的构造方式与新增 5 个用户
- `api/openapi.yaml`：新增 7 个接口、错误码与 `Retry-After`

## 8. 测试要求

**单元**（不依赖数据库）：码规范化；奖励数量公式（含 0、被空位限制、被终身上限限制）；限流器的窗口、冷却、成功清零、进程内退化（注入时钟）；各错误码到 HTTP 状态的映射；补码的"本地日"边界（跨时区、夏令时）。

**集成**（`petbridge_test`）：
- 入房：成功路径（成员数、座位、码状态、边、奖励）；用自己的码；被移除者用新码 → `CODE_INVALID`；已在房 → `ALREADY_MEMBER`；第 3 间 → `ROOM_LIMIT`；成年/未成年双向隔离；过期码；格式错码；重放返回相同奖励且不重复发放；退房后重新加入同一房间不再发奖励
- 满员：最后一人入房后房间 `full`、其余 unused 码 `void`；有人退出后恢复 `open` 但已作废的码不恢复
- **并发**（TRD 7.2 / 7.8 的测试要求，全部必须稳定通过）：
  1. 100 个协程抢同一房间最后一席：恰好 1 人成功，`member_count` 不超限，奖励只发一份
  2. 同一用户并发用两个不同的码进两间不同房间：最多成功到"自己的 + 1 间"
  3. 同一用户并发用同一个码重复提交：一次入房，其余为重放
  4. 同一间房被成年人与未成年人并发用不同码入房：房主的年龄段决定谁能成功，另一方恒为 `AGE_RESTRICTED`，房间不会混龄
  5. A 进 B 的房与 B 进 A 的房同时发生，重复多轮：无死锁
  6. 移除与入房并发（最后一席）：不超员
  7. 解散与入房并发：不死锁，结果一致
  8. 补码并发：不超过空位数与每日上限
- 房主管理：非房主 → `NOT_ROOM_OWNER`；房主退出 → `OWNER_CANNOT_LEAVE`；移除幂等；解散后只剩房主且不拉黑；补码四种 `details.reason`
- 限流：连续 5 次输错进入冷却，成功清零；`ROOM_FULL` 不计入；Redis 不可用时入房仍可用且按收紧阈值限流
- 种子：运行两次结果一致；`FULL2345` 返回 `CODE_INVALID`
- 契约：`openapi.yaml` 与路由表一致

**手动冒烟**：`scripts/smoke.ps1`（见 5.10）。

## 9. 我做的决定（请重点看）

1. 先锁房间再做全部业务校验，并按 id 升序一次锁两间房——对 TRD 7.2 的修正（5.1）。原先以为"成年/未成年并发同房"是缺口，核对后发现房主年龄段已决定房间类别，不是缺口
2. 以 `used_by` 判重放，`Idempotency-Key` 只接受不存储（4.3）
3. 迁移 `0003` 加 `reward_for` 列（5.2）
4. 房主解散提前到 B3b（5.6）
5. 格式错的码按 `CODE_INVALID` 并计入失败（5.1）
6. `ROOM_FULL`、`ALREADY_MEMBER`、`ROOM_LIMIT`、`AGE_RESTRICTED` 不计入输错次数（5.3）
7. Redis 退化时每 IP 每小时 15 次（5.3）
8. 登录限流并入 B3（沿用 B2 spec 的约定）
9. 种子用 5 个新用户占座，而不是用 A–F（5.10）
10. 业务数值放环境变量，`app_config` 热改留待后续（非目标）
11. 不引入 sqlc（第 3 节）
12. 重放只在仍是在席成员时成立，否则 `CODE_INVALID` 并计入失败（5.1，来自实现者审读）
13. 奖励数量为 0 时也写 `reward_grants`，防止退出再加入刷奖励（5.2）
14. 限流检查顺序：冷却 → IP 计数 → onboarding → 事务（5.3）
15. 多个错误同时成立时的优先级（5.1 第 3 步）；邀请人不得奖励（5.2）
16. `leave` 对曾经在席的人幂等返回 204，从没进过的人 `ROOM_NOT_FOUND`（5.6，B3b）
17. 解散先 Peek 校验房主、再锁 unused 码、再锁房间，非房主不锁码（5.6，B3b）
18. 死锁/序列化失败由 `RetryTx` 重试一次，入房一并受益（5.1，B3b）

## 10. 留给实现者

仓储文件如何拆分、SQL 的具体写法、限流 Lua 脚本还是 `INCR`+`EXPIRE`、测试夹具的组织、并发测试的轮数与超时、成员列表的批量查询方式。
