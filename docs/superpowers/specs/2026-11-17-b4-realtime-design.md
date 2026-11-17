# B4 设计：WebSocket Hub、presence、事件分发

日期：2026-11-17　开发方式：直接在 `main` 上开发，每个计划任务完成后分阶段提交并推送到 `jinheng` main
依据：后端 TRD v1.3 的 4.5③、7.3、9、11、12.1、12.2；PRD v1.5 的 F3.2、F3.3；前端 TRD v1.3 的 4.5③、6.3 reducer 规则、9.2、10.1。
状态：已确认（2026-11-17）。先改 PRD 与两份 TRD（两个仓库的副本保持一致），再写计划。

## 1. 意图

前端 F2 要做小屋场景：同屋的人加入、离开、被移除、上线下线、轻触宠物，彼此在 300ms 内看到（PRD F3.2，P95 ≤ 300ms）。B3 已经在事务提交后调用 `Publisher.Publish`，但实现是 `NopPublisher`，事件直接丢弃。B4 提供一条会话级 WebSocket，把这些事件真正送到客户端，并在 Redis 故障时尽量不中断实时体验。这是里程碑 M2“多人实时可见”的后端部分。

## 2. 目标 / 非目标 / 反目标

**目标**
- `GET /api/v1/ws`：鉴权、升级、`hello`、`room.sub` / `room.unsub`、`room.snapshot`、`error` 帧
- 把 B3 的四个事件（`member.joined`、`member.left`、`room.kicked`、`code.invalidated`）真正送达
- presence：在线 / 离线两种状态，`presence.update` 广播给同屋成员，快照里带每个成员的状态
- `pet.tap` → `pet.action`（PRD F3.3，P0）
- Redis 正常时走 TRD 原路径（`room:{rid}:seq` + `ws:events` Pub/Sub）；Redis 故障时退化为本进程直接推送（方案 2，用户已选）
- 连接保护：每用户 3 条、读帧 4KB、每连接每秒 10 帧、慢客户端断开、优雅重启发 `server.restart`
- PRD、两份 TRD、`api/openapi.yaml` 同步

**非目标（留给后续）**
- 断线重放用的 Redis Stream `room:{rid}:events`。每次 `room.sub` 都下发完整快照，B4 的事件全部能由快照还原，重放没有用处（见 4.2）。B5 有了 `message.new` 再评估是否需要
- `message.new`、`message.polished`（B5）、`checkin.updated`、`decor.unlocked`（B6）。帧格式已能容纳，届时只新增事件类型
- REST 的版本检查中间件与 HTTP 426（`hello` 里先下发 `min_app_version`，中间件另做）
- 多节点下的单用户连接数上限（只在本节点内限制）
- 宠物心情 / 打瞌睡（PRD F5.3，P1，依据活跃度，不是 presence）

**反目标**（出现任何一条即视为失败）
- 被移除或已退出的用户还能收到该房间后续的任何事件，或能订阅不属于自己的房间
- Redis 停止时 api 进程崩溃、`/ws` 无法连接，或已订阅的单节点客户端收不到成员变化
- 客户端因 seq 回退或缺口卡在旧状态，无法通过重新订阅恢复
- 一个慢客户端或刷帧的客户端拖慢其他连接的推送
- access token 出现在 URL 或日志里

## 3. 约束

- 沿用现有分层 `handler → service → repository / platform`；service 不依赖 Gin，也不直接依赖 WebSocket 库
- WebSocket 库用 TRD 选定的 `github.com/coder/websocket`
- 本地 PostgreSQL + Memurai，无 Docker；Redis 相关测试沿用现有约定（Redis 不可用或 `-short` 时跳过）
- 不改已有迁移；B4 不需要新表
- Redis 只存可丢失数据（TRD 11），丢失后系统应自动恢复

## 4. 方案比较

**4.1 事件分发与 Redis 的关系（用户已选 B）**
- A. 严格按 TRD，Redis 是唯一通道：Redis 停止时实时功能整体停止，事件丢弃，客户端重连后靠快照修正。代码最少，但本地 Memurai 一停小屋就不动。弃用。
- **B. TRD 路径 + 本机兜底（采用）**：正常时 `INCR` 取 seq、`PUBLISH ws:events`，各节点订阅后推给本机连接。任一步写 Redis 失败，就把事件直接交给本进程的 Hub 推送，**不带 seq**。单节点阶段 Redis 停止时实时照常；多节点时兜底只覆盖本机，其他节点的客户端靠下次订阅的快照修正。与 `ratelimit.NewFallback` 同一思路，符合 TRD 12.2。
- C. 进程内 Hub + PostgreSQL 事件日志：单机最简单，但违背 TRD 原则 4“加节点零改动”。弃用。

**4.2 断线恢复：快照 vs Stream 重放（我的决定，待用户确认）**
- **A. 每次 `room.sub` 都下发完整快照（采用）**：快照从 PostgreSQL 读成员、从 presence 读在线状态，是唯一可信的状态。B4 的事件都是“状态变化”，快照已经包含其结果；而早于快照 seq 的事件按前端 reducer 规则 1 会被丢弃，补发了也不会被应用。所以 B4 不写 Stream，`room.sub` 里的 `last_seq` 收下但不使用（保留字段，B5 可能启用）。前端的 `resync` 改为“重新发一次 `room.sub`”，不再需要 `GET /rooms/{id}`。
- B. TRD 原方案，Stream 补发 + 缺口过大才 resync：要维护 500 条 Stream、24 小时过期，且补发的事件与快照之间要仔细排序，否则会把已经离开的人“补”回来。对 B4 的事件没有收益。弃用（B5 再评估）。

**4.3 presence 存储（我的决定）**
- **A. 每用户一个 ZSET，成员是连接 id，分数是过期时间（采用）**：`presence:u:{uid}`，心跳时刷新本连接的分数，在线 = 未过期的成员数 > 0。多节点、多连接下判断都正确，最后一条连接关闭时才判定离线。
- B. TRD 原写法，单个字符串键 + TTL：同一用户在两台节点或两条连接上时，一条关闭就删键会误判离线。弃用。

## 5. 已定的设计

### 5.1 连接建立

- 路径 `GET /api/v1/ws`，与其他接口同在 `/api/v1` 下（前端 `WS_URL` 改为 `ws://10.0.2.2:8090/api/v1/ws`）。
- 升级前走与 REST 相同的 `Auth()`（`Authorization: Bearer`，失败 401）；再检查已完成 onboarding，未完成返回 403 `ONBOARDING_REQUIRED`。这些失败都是普通 HTTP 响应，不升级。
- 只在升级时校验 token；连接建立后 token 过期不断开【我的决定：事件只发给当前成员，被移除的人会同时被取消订阅，长连接不会泄露数据；每 15 分钟断一次重连会让小屋频繁闪“重连中”】。
- 同一用户在本节点超过 3 条连接时，关闭**最旧**的一条（关闭码 4001 `replaced`），而不是拒绝新连接：App 被杀后旧连接要等心跳超时才消失，拒绝新连接会把用户锁在门外。
- 升级成功后立即下发 `hello`。

### 5.2 帧格式

沿用 TRD 第 9 章：`{ "t": 类型, "id": 帧ID(可选), "seq": 房间序号(可选), "d": 负载 }`。客户端帧的 `id` 由客户端生成，服务端在 `error` 帧里原样回传用于对应；服务端帧不带 `id`。

| 方向 | t | d | 带 seq |
| :-- | :-- | :-- | :-- |
| S→C | `hello` | `server_time`(RFC3339)、`heartbeat_sec`(25)、`proto`(1)、`min_app_version` | 否 |
| C→S | `room.sub` | `room_id`、`last_seq`(可选，B4 不使用) | — |
| C→S | `room.unsub` | `room_id` | — |
| S→C | `room.snapshot` | `room_id`、`seq`（当前房间序号）、`room`（与 `GET /rooms/mine` 单项同结构：`id`、`status`、`member_count`、`capacity`、`is_owner`、`members[]`），每个成员多一个 `presence`：`online` / `offline` | 否（seq 在 d 里） |
| S→C | `member.joined` | `room_id`、`member`（多一个 `presence` 字段，见 5.5） | 是 |
| S→C | `member.left` | `room_id`、`user_id`、`reason`(left/kicked/dissolved) | 是 |
| S→C | `room.kicked` | `room_id`、`reason`(kicked/dissolved/left) | 否 |
| S→C | `code.invalidated` | `room_id`、`reason`(room_full/dissolved) | 否 |
| S→C | `presence.update` | `room_id`、`user_id`、`status`(online/offline) | 否 |
| C→S | `pet.tap` | `room_id`、`target_user_id` | — |
| S→C | `pet.action` | `room_id`、`from_user_id`、`target_user_id`、`action`(`tap`) | 否 |
| S→C | `resync` | `room_id`(可选，缺省表示全部房间) | 否 |
| S→C | `server.restart` | — | 否 |
| S→C | `error` | `ref`（引发错误的客户端帧 id）、`code` | 否 |

- **带 seq 的只有“发给全房在席成员”的状态事件**（目前是 `member.joined`、`member.left`）。只发给个别人的事件和瞬时事件不带 seq，客户端收到即应用。这样同一房间所有订阅者看到的 seq 序列一致，缺口检测才有意义。
- Redis 故障走兜底时，`member.*` 也不带 seq，客户端照样直接应用（成员增删按 `user_id` 幂等）。
- `pet.action.action` 先固定为 `tap`；不同宠物的动作差异由前端 Rive 文件内部实现（前端 TRD 10.2），后端不按 MBTI 下发动作 id。
- `error.code` 复用第 8 章错误码：非成员或房间不存在一律 `ROOM_NOT_FOUND`（不泄露房间是否存在），帧格式错误或未知类型 `VALIDATION_FAILED`，超出订阅上限也用 `VALIDATION_FAILED`。

### 5.3 订阅与快照

- `room.sub`：在用户自己的在席房间列表（复用 `ListForMember`）里找该房间，找不到回 `error ROOM_NOT_FOUND`。每条连接最多订阅 2 个房间（用户最多在 2 间房）。重复订阅同一房间视为重新拉快照。
- 快照顺序，保证不漏事件也不倒退：
  1. 先把连接登记为该房间订阅者，此后到达的该房间事件暂存在连接的待发队列里；
  2. 读 `room:{rid}:seq` 得到 S（Redis 不可用时 S = 0）；
  3. 从 PostgreSQL 读房间与成员，从 presence 读各成员状态；
  4. 发 `room.snapshot{seq:S}`，再按顺序发暂存的事件。
  第 1 步之后、第 3 步之前发生的变化会同时体现在快照和暂存事件里，所以前端对 `member.*` 必须幂等（按 `user_id` 增删），这一点写进前端 reducer 规则。
- **前端规则（写入前端 TRD）**：收到快照后用快照整体替换该房间状态，并把 `last_seq` **设为**快照的 seq（不取较大值，Redis 数据丢失后 seq 会从头开始）；之后丢弃 seq ≤ last_seq 的事件；不带 seq 的事件直接应用；发现 seq 缺口或收到 `resync` 时，重新发 `room.sub`。
- 服务端收到发给某用户的 `room.kicked` 时，同时取消该用户所有连接对该房间的订阅。

### 5.4 事件分发与兜底

```
service ──Publish(event)──► EventPublisher
                              │ 状态事件：INCR room:{rid}:seq → 写入 seq
                              │ PUBLISH ws:events（JSON：事件 + seq）
                              │   成功 → 各节点订阅者收到 → 本机 Hub 投递
                              └ 任一步失败 → 本机 Hub 直接投递（不带 seq），记 warn 日志（30 秒限频）
```

- `Event` 增加一个 `Kind` 字段区分“房间状态事件（带 seq）”“房间瞬时事件”“个人事件”。Hub 的投递规则：
  - 状态事件与瞬时事件：只投给 `To` 里的用户、且该连接订阅了 `RoomID` 的连接
  - 个人事件（`room.kicked`、`code.invalidated`）：投给 `To` 里用户的所有连接，不管是否订阅
- 正常路径下，本节点自己发布的事件也只经订阅回来再投递一次，不在发布时直接投，避免重复。
- **Redis 订阅断开后重连成功时，向本机所有连接发一次 `resync`**（不带 `room_id`）。断开期间可能有事件只走了别的节点的兜底，或 Redis 重启后 seq 从头开始；让客户端重新订阅、拿新快照，是最简单的统一恢复方式。
- 发布侧调用方不变：B3 的 `JoinService`、`ManageService` 仍然只调 `Publisher.Publish`，`cmd/api/main.go` 把 `NopPublisher` 换成真实实现。

### 5.5 presence

- 状态只有 `online` / `offline`。“睡觉”是前端把离线成员画成宠物在睡觉，后端不区分（用户已确认）。PRD F3.2 的“打卡”状态随 B6 的 `checkin.updated` 实现。
- 状态必须持续实时更新（用户要求）：离线的人一回来（App 回到前台重连），同屋在线的人立刻看到他的宠物从“睡觉”变成醒着；离开时同理。也就是每一次 0→1、1→0 的变化都要广播，没有“只在快照里更新”的情况（进程被强杀这一例外见下）。网络断开但没有正常关闭的连接，最迟在下一次 ping 失败（25 + 10 秒）时判定关闭并广播 `offline`。
- `presence:u:{uid}` 为 ZSET：连接注册时 `ZADD 连接id 过期时间`，服务端每 25 秒对每条连接发一次 WS ping，成功即刷新分数（过期时间 = 现在 + 70 秒）并给整个键续 70 秒 TTL；ping 10 秒内没有回应则关闭连接。
- 用户第一条连接注册（在线数 0→1）时广播 `online`；最后一条连接关闭（在线数降为 0）时广播 `offline`。在线数 = `ZCOUNT key 现在 +inf`，关闭时先 `ZREM` 再计数。广播对象：该用户所有在席房间里的其他成员，每个房间发一条 `presence.update`。
- 新成员入房时通常早已在线，不会触发 0→1 的广播。因此 `EventPublisher` 发布 `member.joined` 前，查一次该成员的 presence，填进 `member.presence`【我的决定：放在 Publisher 里，`JoinService` 不需要依赖 presence】。
- Redis 不可用时，presence 退化为本进程内的连接计数（单节点下准确）。
- 已知局限：api 进程被强杀时不会广播 `offline`，其他人要等下次快照才看到；ZSET 成员 70 秒后自然过期，快照读到的状态是对的。单节点阶段可以接受，优雅重启不受影响。

### 5.6 `pet.tap`

- 连接必须已订阅该房间，`target_user_id` 必须是该房间当前成员且不是自己，否则回 `error`。
- 投递给房间里除发起者以外的所有成员（发起者本地已经乐观播放）。成员列表每次从 PostgreSQL 读（最多 5 人，单次主键查询）。
- 每条连接 `pet.tap` 每秒最多 2 次，超出的直接丢弃，不回错误（属于正常的连点）。

### 5.7 连接保护与生命周期

- 读帧上限 4KB，超出关闭（1009）。
- 每连接每秒最多 10 个客户端帧（令牌桶），超出关闭（1008）。
- 每条连接一个容量 64 的发送队列，由单独的写协程发送，单次写超时 5 秒。队列满或写超时就关闭该连接（慢客户端）。
- 优雅重启：进程收到 SIGTERM 时，先向所有连接发 `server.restart`，再以 1001 关闭全部连接，然后才关闭 HTTP 服务。`http.Server.Shutdown` 不会等待已劫持的连接，所以 Hub 的关闭必须显式在它之前执行【我的决定：TRD 写的“30 秒内断开”简化为立即断开，客户端本来就会随机等待 1～10 秒再重连】。
- 连接关闭时：从 Hub 注销、清理订阅、更新 presence（可能广播 `offline`）。

### 5.8 补齐 B3 的两处事件缺口

审查时发现 B3 发出的事件有两处会让在线客户端停在旧状态，B4 一并修正（改 `room_manage_service.go` 及其测试）【我的决定】：
- **解散**：现在只给被移出的成员发 `room.kicked`，房主收不到任何事件，已订阅的房主会一直看到已离开的成员。改为再给房主发每个被移出成员的 `member.left{reason:dissolved}`（状态事件，带 seq）。
- **主动退出**：现在只通知剩余成员，退出者的其他设备不知道自己已离开。改为再给退出者本人发 `room.kicked{reason:left}`（个人事件）。客户端处理与被移除相同（移除该房间、清本地缓存与 `last_seq`），只是 `left` 不需要任何提示。Hub 收到它时同样取消该用户对该房间的订阅。

### 5.9 配置

- 新增环境变量 `MIN_APP_VERSION`，默认 `0.0.0`，只用于 `hello`。
- 心跳 25 秒、presence 70 秒、每用户 3 条连接、4KB、每秒 10 帧、发送队列 64、写超时 5 秒、`pet.tap` 每秒 2 次：先写成代码常量【我的决定：这些是协议参数，改了要同步前端，不适合做成可随手改的环境变量】。

### 5.10 代码落点

按 TRD 第 5 章已规划的文件，具体拆分由实现者在此基础上决定：
- `internal/model/event.go`：WS 帧与各事件负载
- `internal/service/hub_service.go`：本机连接表、订阅、投递规则；通过一个小接口操作连接，不依赖 WebSocket 库
- `internal/service/presence_service.go`：上下线判定与广播
- `internal/service/event_publisher.go`：seq、发布、兜底
- `internal/platform/...`：Redis 侧的 seq / Pub/Sub / presence ZSET 实现，service 只依赖接口（与 `ratelimit` 的做法一致）
- `internal/handler/ws_handler.go`：升级、读写协程、帧解析与限流
- `cmd/api/main.go`：装配，替换 `NopPublisher`，关闭顺序
- `internal/service/room_manage_service.go`：5.8 的两处事件

### 5.11 安全加固（v1.5，实现后安全审查追加，用户已同意）

安全审查与代码审查后追加，对应后端 TRD v1.5：
- **连接不再比 token 活得久**：服务端在升级所用 access token 到期时以关闭码 4401 断开，客户端刷新后立即重连。取代 5.1 的“只在升级时校验、过期不断开”。登出、封号最迟在 access token 到期（15 分钟）时对 WS 生效，与 REST 一致。原先设想的“登出时断开该用户所有连接”没有做：登出只吊销当前设备的刷新令牌，按用户断开会把其他设备也踢掉，而 token 到期断开已经给出了同样的上限【我的决定】。
- **限流**：升级按 IP 每分钟 60 次、按用户每分钟 20 次（429）；`room.sub` 每连接每 10 秒 10 次（`error{RATE_LIMITED}`）；非 UUID 的 `room_id` 不查库直接 `ROOM_NOT_FOUND`。
- **其余**：重启开始后新升级返回 503 `NOT_READY`（也消除了 Shutdown 等待与新连接并发时 `WaitGroup` 的 panic）；`hello` 保证第一帧；订阅暂存超过 64 条以 1008 断开；单次客户端请求背后的 DB / Redis 操作 5 秒超时；连接协程与 Redis 订阅回调有 panic 保护；内部错误写日志。

## 6. 文档与契约同步（先于计划完成）

- **PRD v1.6**：F3.2 写明 presence 只有在线 / 离线，离线时宠物显示为睡觉；打卡状态随打卡功能。
- **后端 TRD v1.4**：4.5③ 时序图与 7.3 改为“每次订阅都下发快照、B4 不做 Stream 重放”；第 9 章按 5.2 的表重写（新增 `room.unsub`、`error`，标明哪些带 seq，`pet.action` 负载，路径 `/api/v1/ws`）；第 11 章 presence 键改为 ZSET，`room:{rid}:events` 标为 B5 再评估；12.2 降级矩阵补 Redis 故障时的实时行为；15.4 补 `MIN_APP_VERSION`；第 17 章 B4 行说明范围。
- **前端 TRD v1.4**：reducer 规则改为 5.3 的“前端规则”（快照替换、seq 设为快照值、无 seq 直接应用、成员事件幂等、缺口 / `resync` → 重新订阅）；9.2 的 `resync` 不再拉 `GET /rooms/{id}`；`WS_URL` 改为 `/api/v1/ws`；presence 只有两态，离线画成睡觉。
- 两个仓库（`PetBridge_backend`、`PetBridge_frontend`）根目录下的三份文档保持完全一致；前端仓库只暂存并提交这三个文件（那里有前端会话未提交的改动，不动），两个仓库都推送到 `jinheng` 的 `main`。
- 文档同步在写计划之前完成，不作为计划任务。
- `api/openapi.yaml`：新增 `GET /api/v1/ws`（带 `operationId`），描述 101 升级与 401 / 403 错误，帧格式指向 TRD 第 9 章。契约测试要求文档路径与路由一一对应，所以这一项随 `/ws` 路由在计划中一起完成，不在文档同步里做。

## 7. 测试

- **B3 事件补齐**：解散时房主收到每个被移出成员的 `member.left{dissolved}`；退出时退出者收到 `room.kicked{left}`。
- **Hub 单元测试**（假连接）：投递规则（订阅过滤、个人事件不需订阅、被移除后取消订阅）、每用户 3 条连接时踢最旧的、发送队列满时断开、快照期间暂存事件按序发出。
- **Publisher 单元测试**：Redis 正常时带 seq 发布；`INCR` 或 `PUBLISH` 失败时走本机投递且不带 seq；Redis 订阅重连后发 `resync`。
- **presence 测试**：多连接下最后一条关闭才离线；Redis 不可用时退化为本地计数；ZSET 过期判定（需要 Redis，按约定可跳过）。
- **端到端测试**（`httptest` 服务器 + `coder/websocket` 客户端 + 测试库）：无 token 401、未 onboarding 403；连接收到 `hello`；订阅自己的房间收到快照，订阅别人的房间收到 `ROOM_NOT_FOUND`；另一用户入房后收到带 seq 的 `member.joined`；房主移除成员后被移除者收到 `room.kicked`，之后收不到该房间事件；`pet.tap` 被转发给其他成员；Redis 不可用时 `member.joined` 仍然送达（不带 seq）。
- 契约测试：`GET /api/v1/ws` 在 `openapi.yaml` 中且有错误响应。
- `scripts/smoke.ps1` 不覆盖 WS（PowerShell 写 WS 客户端成本高），以端到端测试为准。

## 8. 留给实现者

- Hub 内部的数据结构与加锁方式、各协程的组织
- Redis Pub/Sub 消息的具体 JSON 结构（只在后端内部使用）
- 兜底日志的限频实现（可复用 `cmd/api/fallback_log.go` 的思路）
- 测试中模拟 Redis 故障的方式
