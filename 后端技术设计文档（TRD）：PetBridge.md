# 后端技术设计文档（TRD）：PetBridge

| 项目 | 内容 |
| :-- | :-- |
| 版本 | v1.2（草案，待自评审；较 v1.1：对齐 PRD v1.4，新增房主管理，修正消息表分区方案，补充本地运行说明） |
| 对应 PRD | 《跨国 MBTI 情感 AI 电子宠物 PRD v1.4》 |
| 配套文档 | 《前端 TRD（Flutter）v1.2》：WebSocket 协议、API 调用方式以本文第 8、9 章为准 |
| 语言 / 运行时 | Go 1.22+ |
| 开发模式 | **单人全栈开发**，所有选型以“一个人能长期维护”为第一约束 |
| 目标区域 | 日本东京区域起步（覆盖 JP / KR / TW 延迟；中国大陆暂不首发） |

> **V1.2 变更说明**：对齐 PRD v1.4，并修正 V1.1 的几处设计缺陷。① **新增房主管理**（新增 7.8 节）：房主可移除成员、解散小屋、自助补码，房主不可退出自己的小屋；对应新增 `room_bans` 表、`room_members.left_reason`、`invite_codes.source=owner_regen`，第 8 章新增 3 个接口与 `ROOM_LIMIT`/`NOT_ROOM_OWNER`/`OWNER_CANNOT_LEAVE`/`CODE_ISSUE_LIMIT` 四个错误码，第 9 章新增 `room.kicked` 事件、`member.left` 增加 `reason` 字段。② **入房事务（7.2）补全**：V1.1 只写了规则、没落实到事务步骤——新增用户行锁（否则同一用户并发用两个码入房会突破"最多 2 间房"）、禁用自己的码、`room_bans` 黑名单校验、未成年人双向隔离（成年人也不能进含未成年成员的房间）；并定下全局统一加锁顺序 `users → invite_codes → rooms → room_members`，防止入房与解散并发时死锁。③ 奖励码数量改为 `min(配置值, 自己小屋空位数 − 现有未用有效码数)`，终身上限 20 的口径改为除种子码外所有来源累计，新增环境变量 `OWNER_REGEN_DAILY_LIMIT`。④ **`messages` MVP 不分区**：PostgreSQL 分区表的主键和唯一索引必须包含分区键 `created_at`，V1.1 的"按月分区"会让 `(sender_id, client_msg_id)` 无法全局唯一、重发幂等失效；单库 200 QPS 写入不分区足够，靠 30 天清理任务控制体积，第 1.2、3、6.1、18.3 节同步。⑤ 新增 Widget 刷新用的 data-only 推送 `widget.refresh`，`devices` 表新增 `widget_installed`（7.7）：V1.1 前端依赖静默推送刷新 Widget，后端却未定义。⑥ **纯本地运行说明**（15.1、15.5）：当前没有云端预算，仅本地开发与测试；补充依赖服务的两种运行方式（Docker Desktop / 原生安装）及 Windows 下用 go-task 替代 make。第 16 章测试与第 17 章排期同步。5.x 的整体架构、第 10 章 AI 网关、第 11~12 章 Redis 与稳定性设计不变。**（2026-11-14 补充，无接口变更）** 7.1 新增“账号状态与前后顺序”表：登录只建空 `users` 行，小屋与 signup 码只在 onboarding 事务里创建；并约定未完成 onboarding 的用户入房返回 `ONBOARDING_REQUIRED`（B3 实现，尚未列入第 8 章错误码清单）。
>
> **V1.1 变更说明**：暂不首发中国大陆；补充强制更新接口（`UPGRADE_REQUIRED`（HTTP 426）、WebSocket `hello.min_app_version`）。
>
> 标记【假设】的是 PRD 没写清、由本文做出的设计决定，汇总在第 18 章。

---

## 1. 设计前提与原则

### 1.1 五条原则

1. **模块化单体，不做微服务。** 一个人维护多服务意味着多套部署、多套日志、多套联调。只在“扩容特征完全不同”的地方拆进程：API/WebSocket 一个进程，AI 与定时任务（worker）另一个进程，二者同一代码库、同一镜像、不同启动参数。
2. **正确性靠数据库事务，不靠分布式技巧。** 验证码占位、房间满员这类强一致逻辑，用 PostgreSQL 行锁和条件更新解决，不引入分布式锁。
3. **托管优先。** 数据库、Redis、对象存储、推送、TLS 证书能托管就托管，把时间花在业务上。
4. **先单节点，但接口预留。** WebSocket 事件分发抽象成“发布/订阅”接口，MVP 用 Redis Pub/Sub，将来加节点不改业务代码。
5. **AI 一律可降级。** 任何一次模型调用失败，用户都必须仍然收到消息（至少是原文或机翻）。

### 1.2 对“大厂高并发方案”的取舍

| 手段 | 本项目做法 | 原因 |
| :-- | :-- | :-- |
| 微服务拆分 | 不做，模块化单体 + 独立 worker | 单人维护成本过高 |
| K8s + 自动扩缩容 | MVP 不上，用 Docker Compose；灰度后再评估托管容器服务 | 3–5 万在线一台 4C8G 可承载，K8s 的运维成本不划算 |
| 分库分表 | 不做；消息表 MVP 也**不分区**，靠 30 天脱敏/清理任务控制体积 | 峰值写入约 200 QPS，单库远未到瓶颈；且分区表要求唯一键包含分区键，会破坏消息幂等约束 |
| 限流 / 熔断 / 降级 | **第一天就做** | 真正会出事的是 LLM 调用与验证码刷量，见第 12 章 |
| 读写分离、多级缓存 | 灰度后按监控数据再决定 | 没有数据就不优化 |

---

## 2. 容量与性能目标

以下为估算【假设】，上线前用灰度数据校准。

| 指标 | 估算 / 目标 |
| :-- | :-- |
| DAU | 5–10 万 |
| 同时在线 | 约 3–5 万（每人 1 条 WebSocket） |
| 日消息量 | 约 200 万（人均 20 条） |
| 消息写入峰值 | 约 200 QPS（峰值系数 8） |
| AI 调用峰值 | 约 55 QPS（按 30% 消息需要调用模型，其余走短语模板与缓存） |
| 验证码校验峰值 | ≥ 500 QPS（社交平台集中传播时） |
| 接口延迟 | 普通 REST P95 ≤ 200ms |
| 消息送达（不含润色） | P95 ≤ 1.5s |
| 小屋状态同步 | P95 ≤ 300ms（PRD 指标） |
| 润色 | P95 ≤ 5s，超 8s 自动降级 |
| 可用性 | 月可用性 ≥ 99.9% 为目标，单节点阶段按 99.5% 预期并接受 |

---

## 3. 技术选型

| 层 | 选型 | 理由 / 备注 |
| :-- | :-- | :-- |
| 语言 | Go 1.22+ | 协程适合长连接与并发 IO |
| Web 框架 | Gin | 生态成熟，资料多，单人上手快 |
| WebSocket | `coder/websocket` | 维护活跃，API 简洁，原生支持 context |
| 数据库 | **PostgreSQL 16** | JSONB、行级锁、`FOR UPDATE SKIP LOCKED` 一套搞定 |
| 数据访问 | `pgx` + `sqlc` | 编译期生成类型安全查询，单人开发能少写大量样板；复杂事务手写 SQL |
| 迁移 | `goose` | 纯 SQL 迁移，简单可靠 |
| 缓存 / 限流 / 在线状态 | Redis 7（`go-redis`） | 缓存、令牌桶限流、presence、Pub/Sub |
| 异步任务 | `asynq`（基于 Redis） | 重试、延迟、优先级队列、Web UI，省去自己搭 Kafka |
| 定时任务 | worker 内置 cron（asynq 的 Scheduler） | 过期验证码清理、数据脱敏、统计汇总 |
| 熔断 | `sony/gobreaker` | 给每个 AI 供应商单独一个熔断器 |
| 鉴权 | `golang-jwt/jwt/v5` + Apple / Google ID Token 校验 | 见第 13 章 |
| 推送 | FCM HTTP v1（iOS 通过 FCM 转 APNs） | 一个入口覆盖双端；中国大陆厂商推送通道暂不做 |
| 配置 | 环境变量 + `koanf` | 12-factor，密钥不进仓库 |
| 日志 | 标准库 `log/slog`（JSON） | 零依赖，够用 |
| 指标 / 追踪 | Prometheus metrics + OpenTelemetry（可选） | 先指标，后追踪 |
| 错误上报 | Sentry | 免费额度足够 MVP |
| 对象存储 | S3 兼容（分享海报、美术资源）+ CDN |  |
| 接口契约 | OpenAPI 3（`openapi.yaml`）+ `oapi-codegen` | **前后端都是你一个人写**，用契约生成 Go 服务端桩与 Dart 客户端，能避免大量对接错误 |
| 部署 | Docker + Docker Compose + Caddy（自动 HTTPS，原生支持 WebSocket） | 见第 15 章 |

**刻意不选**：Kafka（量级不需要，asynq 足够）、gRPC（对外是移动端，REST + WS 更省事）、Elasticsearch（没有搜索需求）、自建 K8s。

---

## 4. 总体架构

```
  Flutter App ──HTTPS/REST──┐
  Flutter App ──WSS─────────┤
                            ▼
                   Caddy（TLS、反向代理）
                            ▼
        ┌──────────────────────────────────────────┐
        │  api 进程（cmd/api）                     │
        │  Gin REST · WS Hub · 鉴权/限流中间件     │
        │  订阅 Redis 频道 ws:events → 本机连接    │
        └───────┬───────────────────┬──────────────┘
                │                   │ enqueue
         PostgreSQL              Redis ◄───────────┐
    （业务数据、消息分区表）  （缓存/限流/presence/      │
                              Pub/Sub/asynq 队列）     │
                                    │ dequeue         │
        ┌───────────────────────────▼────────────┐   │
        │  worker 进程（cmd/worker）              │───┘ publish
        │  翻译/润色任务 · 推送 · 定时清理         │
        │  AI 网关：缓存→限流→熔断→供应商链         │
        └───────┬───────────────┬────────────────┘
                ▼               ▼
          LLM 供应商 A/B     机翻服务（兜底）
          FCM（推送）         内容审核 API
```

**进程职责**

| 进程 | 职责 | 扩容方式 |
| :-- | :-- | :-- |
| api | REST、WebSocket 长连接、事件分发 | 加节点；因事件走 Redis Pub/Sub，节点之间无状态依赖 |
| worker | AI 翻译/润色、推送、定时任务 | 调大 asynq 并发数或加节点，与 api 独立伸缩 |

---

## 5. 项目目录结构

```
petbridge/
├── cmd/
│   ├── api/                # HTTP + WS 入口
│   ├── worker/             # asynq 消费者 + 定时任务
│   └── tools/              # seedcodes（零号用户种子码）、backfill 等一次性工具
├── api/openapi.yaml        # 接口契约（前后端共用）
├── internal/
│   ├── config/
│   ├── auth/               # Apple/Google 校验、JWT、刷新令牌
│   ├── user/               # 用户、onboarding、MBTI→宠物映射
│   ├── invite/             # 验证码生成、校验、奖励发放（核心）
│   ├── room/               # 房间、成员、打卡、装扮
│   ├── chat/               # 消息收发、送达记录、短语模板
│   ├── ai/                 # AI 网关：provider、缓存、熔断、配额、提示词
│   ├── moderation/         # 本地词库 + 外部审核
│   ├── realtime/           # WS Hub、presence、事件分发、断线重放
│   ├── push/               # FCM 封装、推送策略
│   ├── safety/             # 举报、屏蔽、未成年人规则
│   ├── platform/           # db(pgx)、redis、queue、storage、clock 等基础设施
│   └── httpx/              # 中间件、统一响应、错误码
├── db/
│   ├── migrations/         # goose SQL
│   └── queries/            # sqlc 查询
├── deploy/                 # compose.yml、Caddyfile、备份脚本
└── test/                   # 集成测试、k6 压测脚本
```

依赖方向：`httpx → 各业务包 → platform`，业务包之间只通过接口通信，避免循环依赖。

---

## 6. 数据库设计（PostgreSQL）

### 6.1 表一览

| 表 | 关键字段 | 说明 |
| :-- | :-- | :-- |
| users | id(uuid), nickname, native\_lang, mbti, pet\_type, age\_band, tz, region, mbti\_changed\_at, status, deleted\_at | pet\_type 由 mbti 映射后落库，保证宠物形象长期稳定 |
| auth\_identities | provider, subject, user\_id | 唯一键 (provider, subject)，一个用户可绑多种登录方式 |
| refresh\_tokens | id, user\_id, token\_hash, device\_id, expires\_at, revoked\_at | 只存哈希 |
| devices | id, user\_id, platform, push\_token, locale, widget\_installed, last\_seen\_at | 推送与 Widget 使用；`widget_installed` 由客户端通过 `PUT /devices/current` 上报，决定是否发送 `widget.refresh` |
| rooms | id, owner\_id, capacity(默认5), member\_count, status(open/full/closed) | 每个用户注册时自动拥有一间“自己的小屋” |
| room\_members | room\_id, user\_id, seat\_no, role, joined\_at, left\_at, left\_reason(left/kicked/dissolved) | 在席唯一约束见 6.2 |
| room\_bans | room\_id, user\_id, banned\_by, created\_at | 被房主移除者的黑名单，主键 (room\_id, user\_id)；入房时命中则按 CODE\_INVALID 处理 |
| invite\_codes | code, room\_id, issued\_to, source, status, used\_by, used\_at, expires\_at | source：zero / signup / join\_reward / owner\_regen；status：unused / used / expired / void |
| reward\_grants | user\_id, room\_id, granted\_count, created\_at | 唯一键 (user\_id, room\_id)，保证奖励幂等 |
| invite\_edges | inviter\_id, invitee\_id, code, created\_at | 裂变关系树，运营分析与风控用 |
| messages | id(uuidv7), room\_id, sender\_id, scope(room/dm), to\_user\_id, kind(text/phrase/sticker), src\_lang, body, client\_msg\_id, mod\_status, created\_at | **MVP 不分区**（见下方说明）；主键 id，唯一键 (sender\_id, client\_msg\_id) 保证重发幂等；索引 (room\_id, id) 支撑游标分页 |
| deliveries | id, message\_id, receiver\_id, target\_lang, base\_text, polished\_text, polish\_status, read\_at | 每个接收者一行，存各自语言的结果 |
| checkins | user\_id, room\_id, local\_day, energy | 唯一键 (user\_id, room\_id, local\_day)，按用户时区算“当天” |
| room\_progress / room\_decor | room\_id, energy\_total, level / item\_id, unlocked\_at | 打卡能量与装扮解锁 |
| reports / blocks | reporter\_id, target\_id, message\_id, reason, status | 应用商店 UGC 合规必备 |
| ai\_usage\_daily | day, user\_id, kind, calls, tokens\_in, tokens\_out, cost\_micro | 配额与成本看板 |
| app\_config | key, value(jsonb) | 运营开关：奖励数量、有效期、AI 开关等，进程内缓存 30 秒 |

> **为什么 messages 不分区**：PostgreSQL 分区表上的主键和唯一索引必须包含分区键（`created_at`），这样 `(sender_id, client_msg_id)` 就无法全局唯一，重发幂等会失效。MVP 单库约 200 QPS 写入、每日 200 万行，30 天保留期内约 6000 万行，配合 `(room_id, id)` 索引完全够用。过期数据由 worker 的定时任务批量脱敏/删除（每批限量，避免长事务）。若日后确需分区，再用单独的 `msg_idempotency` 表承担幂等。

### 6.2 验证码与房间核心约束

```sql
-- 同一用户在同一房间只能有一条在席记录
CREATE UNIQUE INDEX uq_member_active ON room_members(room_id, user_id) WHERE left_at IS NULL;
-- 座位号在房间内唯一（仅在席）
CREATE UNIQUE INDEX uq_seat_active ON room_members(room_id, seat_no) WHERE left_at IS NULL;
-- 房间人数不得超过容量
ALTER TABLE rooms ADD CONSTRAINT ck_member_cap CHECK (member_count <= capacity);
-- 验证码唯一
ALTER TABLE invite_codes ADD CONSTRAINT pk_code PRIMARY KEY (code);
-- 黑名单：同一房间对同一用户只记一次
ALTER TABLE room_bans ADD CONSTRAINT pk_room_ban PRIMARY KEY (room_id, user_id);
```

**验证码格式**：8 位，字符集 32 个（去掉易混淆的 0/O/1/I），约 1.1 万亿种组合；用 `crypto/rand` 生成，撞库时依赖主键冲突重试，展示为 `XXXX-XXXX`，输入时不区分大小写、自动去连字符。

---

## 7. 核心流程设计

### 7.1 注册与 onboarding

1. 客户端用 Apple / Google 登录，把 ID Token 交给 `POST /auth/login/apple|google`，后端校验后创建或找到用户，返回 access + refresh token。
2. 新用户调用 `POST /me/onboarding`（母语、MBTI、昵称、年龄区间、时区）。**一个事务内**完成：写 users → 按映射表写 pet\_type → 创建 rooms（owner=自己，member\_count=1）→ 写 room\_members（seat 1）→ 生成 1 个 signup 验证码绑定该房间。
3. 极简测试在**客户端本地**计分，只把结果 MBTI 传给后端，后端不存题库。
4. 接口幂等：已完成 onboarding 的用户再次调用，直接返回现有结果。

**账号状态与前后顺序**（对应 PRD 5.1：先有账号，再用验证码）：

| 阶段 | 数据状态 | 能做什么 |
| :-- | :-- | :-- |
| 已登录、未 onboarding | `users` 行已存在但资料列为空，`onboarded_at` 为空；**没有 rooms、room\_members、邀请码** | 只能调用 `GET /me`、`POST /me/onboarding`；不能入房、不能输入验证码 |
| 已 onboarding（单人模式） | 资料、`pet_type` 已写入；自己的小屋只有自己一人，并有 1 个 signup 码 | 可输入他人验证码入房，也可分享自己的码 |

登录接口只负责创建或找到 `users` 行与 `auth_identities`，不创建小屋；小屋只在 onboarding 事务里创建，所以“登录先建 users 行”与“onboarding 写 users”不矛盾：前者建空行，后者填资料。客户端据此判断是否需要 onboarding（`GET /me` 的 `onboarded` 字段）。验证码入房（7.2）要求调用者已完成 onboarding，否则返回 `ONBOARDING_REQUIRED`。

### 7.2 验证码入房（最关键的一段）

`POST /rooms/join`，请求体 `{ code }`，请求头必带 `Idempotency-Key`。服务端一个数据库事务内按固定顺序执行：

```
0. 前置：限流检查（Redis，见第 12 章）
0a. SELECT 1 FROM users WHERE id=$me FOR UPDATE
      （串行化同一用户的并发入房，否则两个并发请求会同时通过“最多 2 间房”的检查）
1. SELECT * FROM invite_codes WHERE code=$1 FOR UPDATE
      → 不存在/已用/已过期/已作废：回滚，返回 CODE_INVALID（对外不区分原因）
1a. 业务校验（在事务内读取该房间现有成员）：
      · 码所属房间的房主就是自己（用自己的码）→ CODE_INVALID
      · 命中 room_bans（曾被房主移除）→ CODE_INVALID（对外不暴露“被移除”的事实）
      · 用户当前在席房间数 ≥ 2 → ROOM_LIMIT
      · 未成年人双向隔离：用户与房间现有全部成员的“是否成年”必须一致，否则 AGE_RESTRICTED
2. UPDATE rooms SET member_count = member_count + 1
      WHERE id=$room AND status='open' AND member_count < capacity RETURNING member_count
      → 0 行：返回 ROOM_FULL（不计入失败冷却）
3. 取最小空位号，INSERT room_members；若违反唯一约束（已在房）→ 视为成功，返回现有房间（幂等）
4. UPDATE invite_codes SET status='used', used_by, used_at
5. INSERT invite_edges；INSERT reward_grants ON CONFLICT DO NOTHING
      → 本次真正插入才生成奖励码，绑定到**用户自己的小屋**；数量 = min(app_config 配置值, 自己小屋空位数 − 现有未用有效码数)，下限 0，并受终身上限 20 约束（口径：除种子码外所有来源累计，见 PRD R6）
6. 若 member_count = capacity：rooms.status='full'；
      UPDATE invite_codes SET status='void' WHERE room_id=$room AND status='unused'
7. COMMIT；提交后发布事件 member.joined、code.invalidated（对被作废码的持有者）
```

**为什么不需要分布式锁**：竞争只发生在同一个房间的 5 个席位上，`UPDATE ... WHERE member_count < capacity` 是数据库级原子条件更新，多人同时抢最后一个席位时只有一个能得到 `RETURNING` 行。热点房间的锁竞争范围天然被 5 人上限限制住了。

**统一加锁顺序（防死锁）**：凡是同时涉及多张表的事务（入房、踢人、解散、补码、退出），一律按 `users → invite_codes → rooms → room_members` 的顺序加锁，不得反向。入房事务是“码 → 房间”，所以解散房间时也必须先作废码、再改房间。

**测试要求**：集成测试里 100 个协程同时抢同一房间最后一个席位，断言恰好 1 人成功、member\_count 不超限、奖励只发 1 次；同一用户并发用两个不同的码加入两间不同的房间，断言最多只成功到“自己的 + 1 间”。

### 7.3 实时连接与房间事件

- 客户端连接 `GET /ws`（升级请求用 `Authorization` 头携带 access token，避免 token 进 URL 日志）。
- 连接建立后服务端下发 `hello`（含服务器时间、心跳间隔），客户端发送 `room.sub`；服务端回 `room.snapshot`（成员、座位、presence、最近事件序号）。
- **事件分发**：业务代码调用 `Publisher.Publish(event)`，事件写入 Redis 频道 `ws:events`；每个 api 节点订阅该频道，只推给本机上属于目标用户的连接。单节点阶段这层看似多余，但它让“加第二台 api”变成零改动。
- **断线重放**：每个房间有单调递增序号 `room_seq`（Redis `INCR`），最近 500 条事件存入 Redis Stream，保留 24 小时。客户端重连时带 `last_seq`，服务端能补就补，缺口过大则下发 `resync`，客户端改走 REST 全量拉取。
- **Presence**：连接存活期间每次心跳刷新 `presence:u:{id}`（TTL 70 秒）；状态变化（在线、离线、打瞌睡）广播给同房成员。
- **连接保护**：单用户最多 3 条并发连接；慢客户端（写缓冲堆积、写超时 5 秒）主动断开；读消息大小上限 4KB。
- **优雅重启**：收到 SIGTERM 先下发 `server.restart` 帧，30 秒内断开所有连接，客户端按退避重连。

### 7.4 消息发送与 AI 信使

`POST /rooms/{id}/messages`，体：`client_msg_id`、`kind`、`text` 或 `phrase_id`、`scope`、`to_user_id`、`polish`（布尔，默认 false）。

```
同步段（api，目标 < 100ms）
  1. 校验：成员身份、屏蔽关系、年龄规则、长度（≤500 字）、限流
  2. 前置审核：本地词库（Aho-Corasick，按语言分库）
     命中高危 → 拒绝（CONTENT_BLOCKED）；疑似 → 标记 mod_status=review，仍继续
  3. 以 (sender_id, client_msg_id) 幂等写入 messages
  4. 若 kind=phrase：直接用服务端短语模板表取各目标语言文案，写 deliveries，推送，结束（不调用 AI）
  5. 否则入队 asynq：translate:{message_id}，返回 202 + message_id

异步段（worker）
  6. 取接收者，按 target_lang 分组（群聊中同语言只翻译一次）
  7. 对每个语言：AI 网关 → 基础翻译（便宜模型），结果写 deliveries.base_text
  8. 若 polish=true：再按（目标语言 × 接收者 MBTI）走润色
  9. 后置审核：对译文再过一遍词库
 10. 发布 message.new 事件（每个接收者带各自的译文、发送者宠物形象、原文）
 11. 接收者不在线 → 入队推送
```

**润色的两个入口**【假设】：发送方发送时打开“AI 润色”开关；接收方在卡片上点“让 AI 润色”，调用 `POST /deliveries/{id}/polish`，结果写回 `deliveries.polished_text` 并以 `message.polished` 事件通知。两个入口共用同一套缓存和配额。

**卡片切换“润色译文 / 原文”完全在客户端完成**，后端已在 message.new 中下发原文、基础译文，润色后补发 polished\_text。

### 7.5 打卡与装扮

- `POST /rooms/{id}/checkin`：以用户时区计算 `local_day`，唯一键保证一天一次；事务内累加 `room_progress.energy_total`，达到里程碑则写 `room_decor` 并发布 `decor.unlocked`。
- 装扮目录（10–15 件）与解锁阈值放在代码常量或 `app_config`，不建单独的商品表。

### 7.6 桌面 Widget 数据

`GET /widget/snapshot`：一次返回“单人状态 / 小屋成员状态 + 最新一条信使消息摘要”，响应体控制在 2KB 内，带 `ETag`。Widget 由系统调度刷新，**不依赖 WebSocket**；消息正文是否展示由客户端隐私设置决定，服务端只返回摘要字段。

### 7.7 推送策略

- 触发：收到信使消息、有人加入小屋、打卡提醒、被房主移除（文案只写“你已离开该小屋”，不写原因）。
- 规则：用户当前有活跃 WebSocket 连接则不推；同一房间 5 分钟内合并；按用户时区夜间（23:00–08:00）静默；每用户每日上限 10 条。
- 推送内容只含摘要，不含完整译文。
- **Widget 刷新推送（v1.2 新增）**：新信使消息、有人加入/离开小屋时，除普通推送外，向已安装 Widget 的设备（`devices` 记录 `widget_installed`）额外发一条 data-only 推送 `{type: "widget.refresh"}`，不显示通知，客户端收到后更新共享存储并刷新 Widget。该推送不受“每日 10 条”上限约束，但同一房间 5 分钟内合并。iOS 对静默推送有系统节流，**不保证送达**，前端文案不得承诺实时。

### 7.8 房主管理（踢人、解散、自助补码、退出）

对应 PRD R10、R11。以下操作都只有**房主**能对**自己的小屋**执行，服务端校验 `rooms.owner_id = 当前用户`，否则返回 `NOT_ROOM_OWNER`。所有操作各自一个事务，遵守 7.2 的统一加锁顺序。

**移除成员** `DELETE /rooms/{id}/members/{user_id}`

```
1. 校验：调用者是房主；目标在席且不是房主本人（房主不能移除自己）
2. UPDATE room_members SET left_at=now(), left_reason='kicked'
      WHERE room_id=$r AND user_id=$u AND left_at IS NULL
      → 0 行：目标已不在房内，按幂等视为成功，直接返回
3. INSERT room_bans ON CONFLICT DO NOTHING
4. UPDATE rooms SET member_count = member_count - 1, status = 'open' WHERE id=$r
      （原为 full 则恢复 open；此前因满员而作废的码保持 void，不恢复）
5. COMMIT 后：向房内其余成员发 member.left{reason:"kicked"}，向被移除者发 room.kicked，
   并让其连接退订该房间；给被移除者发一条普通推送
```

座位号随 `left_at` 释放，下一位入房者取最小空位。被移除者仍保留“自己的小屋”，不影响其继续使用其他功能。

**解散** `POST /rooms/{id}/dissolve`

```
1. 校验：调用者是房主
2. UPDATE invite_codes SET status='void' WHERE room_id=$r AND status='unused'   （先码，后房间）
3. UPDATE room_members SET left_at=now(), left_reason='dissolved'
      WHERE room_id=$r AND user_id <> owner_id AND left_at IS NULL
4. UPDATE rooms SET member_count=1, status='open' WHERE id=$r
5. COMMIT 后：向每位被移出的成员发 room.kicked（reason 为 dissolved），不加入黑名单
```

解散不拉黑：这些成员日后拿到新码仍可再次加入。解散后房间只剩房主，与注册时的初始状态一致。

**自助补码** `POST /rooms/{id}/codes`

```
1. 校验：调用者是房主；SELECT ... FROM rooms WHERE id=$r FOR UPDATE（串行化同一房间的并发补码）
2. 房间 status 必须为 open；空位数 free = capacity - member_count
3. 房间当前“未使用且未过期”的码数 < free（生成后不得超过空位数）
4. 今日（按房主时区的 local_day）source=owner_regen 的码数 < OWNER_REGEN_DAILY_LIMIT（默认 3）
5. 房主终身累计获得码数（除 zero 外所有来源）< REWARD_CODE_LIFETIME_CAP（默认 20）
6. INSERT invite_codes(source='owner_regen', expires_at=now()+CODE_TTL_DAYS)
```

任一条件不满足返回 `CODE_ISSUE_LIMIT`，`error.details.reason` 取 `room_full` / `enough_unused` / `daily_limit` / `lifetime_limit`，前端据此展示不同文案。

**成员退出** `POST /rooms/{id}/leave`：仅限“所加入的他人小屋”。房主对自己的小屋调用返回 `OWNER_CANNOT_LEAVE`（应使用解散）。事务内置 `left_reason='left'`、`member_count - 1`、满员则恢复 `open`；不拉黑，已作废的码不恢复。

**测试要求**：踢人与入房并发（最后一个席位）不超员；dissolve 与 join 并发不死锁；补码并发不超过“空位数”和每日上限；被移除者用同房间新码入房一律得到 CODE_INVALID。

---

## 8. REST API 设计

基础路径 `/api/v1`，JSON，UTF-8。成功返回 `{ data }`，失败返回 `{ error: { code, message, request_id } }`。`message` 仅供调试，客户端依据 `code` 做多语言展示。

| 分组 | 方法与路径 | 说明 |
| :-- | :-- | :-- |
| 认证 | POST /auth/login/apple、/auth/login/google | 换取 token；手机号登录列为 P1【假设】 |
|  | POST /auth/refresh、/auth/logout | 刷新令牌轮换 |
| 用户 | GET /me；PATCH /me | 昵称、母语（30 天一次）、推送与隐私设置 |
|  | POST /me/onboarding | 见 7.1，幂等 |
|  | POST /me/mbti-change | 注册后 7 天内可改 1 次【假设】 |
|  | DELETE /me | 账号删除（应用商店要求），见第 13 章 |
| 验证码 | GET /codes | 背包：码、状态、过期时间、绑定房间人数 |
|  | GET /codes/{code}/share-card | 分享卡片所需数据（宠物、码、人数进度），海报图由客户端渲染 |
| 房间 | POST /rooms/join | 见 7.2 |
|  | GET /rooms/mine | 我所在的房间（自己的 + 已加入的） |
|  | GET /rooms/{id}；POST /rooms/{id}/leave | 详情与退出（仅限他人小屋，见 7.8） |
|  | DELETE /rooms/{id}/members/{user\_id} | 房主移除成员，见 7.8 |
|  | POST /rooms/{id}/dissolve | 房主解散，见 7.8 |
|  | POST /rooms/{id}/codes | 房主自助补码，见 7.8 |
| 消息 | POST /rooms/{id}/messages；GET /rooms/{id}/messages?before= | 发送、游标分页 |
|  | POST /deliveries/{id}/polish；POST /deliveries/{id}/read | 接收方润色、已读 |
| 小屋 | POST /rooms/{id}/checkin；GET /rooms/{id}/decor | 打卡与装扮 |
| 配置 | GET /config/phrases?v= | 常用短语多语言模板，带版本号，客户端缓存 |
| 安全 | POST /reports；POST /blocks；DELETE /blocks/{user\_id} | 举报与屏蔽 |
| 设备 | PUT /devices/current | 登记推送 token、语言、时区 |
| Widget | GET /widget/snapshot | 见 7.6 |

**业务错误码（节选）**

| code | 含义 | 客户端处理 |
| :-- | :-- | :-- |
| CODE\_INVALID | 验证码无效/过期/已用 | 提示向朋友索取新码 |
| ROOM\_FULL | 小屋已满 | 引导用自己的码邀请他人 |
| CODE\_COOLDOWN | 连续输错进入冷却 | 显示剩余时间 |
| ALREADY\_MEMBER | 已在该房间 | 直接进入 |
| AGE\_RESTRICTED | 年龄规则限制 | 说明原因 |
| QUOTA\_EXCEEDED | 润色次数用尽 | 引导明日再试 |
| CONTENT\_BLOCKED | 内容未通过审核 | 提示修改 |
| RATE\_LIMITED | 触发限流 | 退避重试 |
| UPGRADE\_REQUIRED | App 版本低于最低要求（HTTP 426） | 全屏强制更新页，跳转商店 |
| ROOM\_LIMIT | 已在 2 间房内，不能再加入（v1.2 新增） | 提示先退出已加入的小屋 |
| NOT\_ROOM\_OWNER | 非房主执行房主操作（v1.2 新增） | 隐藏入口，一般不会触发 |
| OWNER\_CANNOT\_LEAVE | 房主尝试退出自己的小屋（v1.2 新增） | 引导使用“解散” |
| CODE\_ISSUE\_LIMIT | 自助补码被拒，`details.reason` 区分原因（v1.2 新增） | 按 reason 展示文案 |

---

## 9. WebSocket 协议

帧格式为 JSON：`{ t: 事件类型, id: 帧ID, seq: 房间序号(可选), d: 负载 }`。

| 方向 | t | 负载要点 |
| :-- | :-- | :-- |
| S→C | hello | 服务器时间、心跳间隔（25 秒）、协议版本 proto、min\_app\_version |
| C→S | room.sub | room\_id、last\_seq |
| S→C | room.snapshot | 成员列表（宠物、昵称、母语国旗、座位）、presence、当前 seq |
| S→C | member.joined / member.left | 成员信息；`member.left` 另带 `reason`（left / kicked / dissolved） |
| S→C | room.kicked | 仅发给被移除者：room\_id、reason（kicked / dissolved）；客户端收到后退出该房间页并刷新房间列表 |
| S→C | presence.update | user\_id、状态 |
| C→S / S→C | pet.tap / pet.action | 轻触宠物与对应的性格动作（MBTI 决定动作 id） |
| S→C | message.new | 发送者宠物、原文、基础译文、delivery\_id、是否已润色 |
| S→C | message.polished | delivery\_id、润色文本 |
| S→C | checkin.updated / decor.unlocked | 能量、装扮 |
| S→C | code.invalidated | 房间已满导致某码失效 |
| S→C | resync | 客户端需全量刷新 |
| S→C | server.restart | 服务重启，请退避重连 |
| 双向 | ping / pong | 使用 WS 控制帧心跳，应用层不重复实现 |

版本策略：`hello` 中带 `proto` 版本号，向后兼容的新增字段不升版本；不兼容变更升主版本；hello 同时下发 min\_app\_version（App 最低版本号），REST 在客户端版本低于该值时返回 HTTP 426 与 UPGRADE\_REQUIRED，客户端据此展示强制更新页。

---

## 10. AI 网关设计

### 10.1 调用链

```
请求 → 配额检查 → 缓存命中？→ 是：直接返回
                    ↓ 否
        全局并发信号量 → 熔断器(供应商A) → 超时控制
                    ↓ 失败或熔断打开
        供应商B → 机器翻译兜底 → 原文 + “未润色”标记
```

### 10.2 关键设计

| 项 | 设计 |
| :-- | :-- |
| 抽象 | `Translator` 与 `Polisher` 两个接口，供应商可配置替换，业务层不感知具体模型 |
| 模型分层 | 基础翻译用低成本模型；润色用质量更高的模型；均通过 `app_config` 切换 |
| 提示词管理 | 提示词模板带版本号（`prompt_version`），按语言 × MBTI 维度组织，版本号进入缓存键，改提示词即自然失效旧缓存 |
| 输出约束 | 要求模型只返回结构化结果（译文 + 风险标记）；解析失败视为一次失败，走降级 |
| 事实保护 | 润色提示词明确要求不得增删事实、数字、时间、人名；后端对数字与时间做简单一致性校验，不一致则回退到基础译文 |
| 缓存键 | 基础翻译：`tr:{sha256(规范化原文)}:{src}:{tgt}:{pv}`，TTL 7 天；润色：`po:{sha256}:{src}:{tgt}:{接收者MBTI}:{pv}`，TTL 7 天（仅按接收者 MBTI 分桶，把键空间压到 16 档以提高命中） |
| 短语模板 | 早安/晚安/谢谢等高频短语由服务端维护多语言模板并随版本下发给客户端缓存，发送时只传 `phrase_id`，完全不触发 AI |
| 超时 | 单次调用基础翻译 3 秒、润色 6 秒；超时即视为失败 |
| 熔断 | 每个供应商一个 gobreaker：最近 20 次请求失败率 ≥ 50% 打开，30 秒后半开探测 |
| 配额 | 每用户每日润色次数默认 30【假设】、每分钟 6 次；全局设日预算，超预算自动关闭润色只保留基础翻译 |
| 成本统计 | 每次调用记录 token 与估算成本，写 `ai_usage_daily`，按语言、功能拆分，供看板使用 |
| 隐私 | 发给供应商的内容只含当前文本，不带用户标识；与供应商确认不将数据用于训练 |

---

## 11. Redis 键设计

| 键 | 类型 | 用途 | TTL |
| :-- | :-- | :-- | :-- |
| `presence:u:{uid}` | string | 在线状态 | 70 秒，心跳刷新 |
| `room:{rid}:seq` | counter | 房间事件序号 | 无 |
| `room:{rid}:events` | stream | 断线重放，最多 500 条 | 24 小时 |
| `ws:events` | pub/sub | 跨节点事件分发 | — |
| `rl:join:u:{uid}`、`rl:join:ip:{ip}` | 令牌桶 / 计数 | 验证码输入限流 | 滑动窗口 |
| `cd:join:u:{uid}` | string | 输错冷却标记 | 30 分钟 |
| `rl:msg:u:{uid}`、`rl:polish:u:{uid}` | 令牌桶 | 消息与润色限流 | — |
| `quota:polish:{uid}:{yyyymmdd}` | counter | 每日润色配额 | 到次日 |
| `tr:*`、`po:*` | string | 翻译与润色缓存 | 7 天 |
| `cfg:app` | string | 运营配置缓存 | 30 秒 |

约定：Redis 只存**可丢失**数据。Redis 整体丢失后，系统应能自动恢复（限流计数清零、presence 重建、缓存回源），不允许出现业务数据丢失。

---

## 12. 稳定性：限流、熔断、降级

### 12.1 限流

| 对象 | 规则 |
| :-- | :-- |
| 验证码输入 | 同一用户 10 分钟内连续输错 5 次，冷却 30 分钟（PRD R8）；同一设备/IP 每小时上限 30 次；房间已满不计入失败 |
| 登录 | 同一 IP 每分钟 10 次 |
| 发消息 | 每用户每分钟 20 条、每日 1000 条 |
| 润色 | 见 10.2 配额 |
| WebSocket | 每用户最多 3 条连接；每连接每秒最多 10 帧 |
| 全局 | 网关层总 QPS 上限，超出返回 503 + `Retry-After`，宁可部分用户稍后重试，也要保住核心链路 |

### 12.2 降级矩阵

| 故障 | 表现 | 降级行为 |
| :-- | :-- | :-- |
| 主 LLM 超时/熔断 | 翻译变慢 | 切备用供应商 → 机翻 → 原文 + 未翻译标记，用户可手动重试 |
| AI 日预算耗尽 | — | 关闭润色，保留基础翻译与短语 |
| Redis 故障 | 限流与缓存失效 | 限流改用进程内令牌桶（偏严）；缓存直通；presence 暂时显示为未知；**消息收发不受影响** |
| 队列积压 | 译文延迟 | 先发原文卡片（标记翻译中），译文到达后用 `message.polished` 同类事件补发 |
| 数据库慢 | 全面变慢 | 只读路径加超时；拒绝非核心写入（打卡、装扮）并提示稍后再试 |
| 推送通道故障 | 无推送 | 不影响应用内消息，记录失败，重试 3 次后放弃 |

降级开关全部放在 `app_config`，**不需要发版即可切换**，这是单人运维时最重要的应急能力。

---

## 13. 安全、隐私与合规实现

| 事项 | 实现 |
| :-- | :-- |
| 令牌 | access token 15 分钟；refresh token 30 天，每次使用轮换，检测到旧 token 重放则吊销整条链；refresh 只存哈希 |
| 传输 | 全程 TLS；证书由 Caddy 自动续期 |
| 敏感数据 | 数据库与磁盘启用静态加密（云厂商能力）；密钥、供应商 API Key 走环境变量/密钥服务，不进仓库、不进日志 |
| 日志脱敏 | 日志中不记录消息正文与译文，只记长度、语言、耗时 |
| 消息保留 | 消息正文与译文保留 30 天，之后脱敏为元数据【假设，对应 PRD 数据保留】 |
| 账号删除 | `DELETE /me` 先软删除并立即吊销全部 token，30 天后由定时任务硬删除个人数据，消息按匿名化处理 |
| 区域与数据主体权利 | 提供数据导出、删除入口；隐私政策按 JP / KR / TW 分别准备；若运营主体在马来西亚，另需评估马来西亚 PDPA（法律结论需法务确认） |
| 未成年人 | 注册收集年龄区间；未满 13 岁拒绝注册；未满 18 岁禁用私聊；**未成年人与成年人双向隔离**：未成年人只能加入全员未成年的房间，成年人也不能加入含未成年成员的房间【假设】，该规则在入房事务（7.2 第 1a 步）与发消息校验中服务端强制 |
| 内容安全 | 本地多语言词库 + 外部审核 API 做复核；高危内容进入人工复核队列；举报入口与屏蔽即时生效 |
| 防刷 | 设备标识、IP、邀请关系树异常检测（短时间大量新号涌入同一房间则暂停奖励发放）；后续接入 App Attest / Play Integrity（P2） |
| 输入校验 | 所有入参用 validator 校验；昵称做敏感词过滤；消息长度、频率限制 |

---

## 14. 可观测性

| 类别 | 内容 | 告警阈值（初版） |
| :-- | :-- | :-- |
| 指标 | QPS、错误率、P95 延迟、WS 在线连接数、队列积压、AI 成功率/延迟/成本、缓存命中率、验证码成功率与失败原因分布 | 5xx > 1% 持续 5 分钟；队列积压 > 1000；AI 成功率 < 90% |
| 日志 | slog JSON，带 `request_id`、`user_id`（哈希）、`room_id` | — |
| 错误 | Sentry，按版本与区域分组 | 新增错误类型即时通知 |
| 可用性 | 外部探活（`/healthz`、`/readyz`） | 连续 3 次失败通知手机 |
| 看板 | 一页 Grafana：流量、AI 成本、裂变漏斗（注册→入房→发码→被用） | 每日人工看一次 |

---

## 15. 部署与运维（单人友好）

### 15.1 两个阶段

> **当前阶段（v1.2 说明）**：目前没有云端预算，开发、测试、联调**全部在本机进行**，不涉及线上部署。下表的 A / B 两个阶段是日后真要上线时的参考方案，现在不需要做。本地运行方式见 15.5。

| 阶段 | 形态 | 适用 |
| :-- | :-- | :-- |
| A 起步（开发/内测/小规模灰度） | 东京区域一台 4C8G 云主机，Docker Compose 运行 Caddy（同时托管 apple-app-site-association、assetlinks.json 与分享落地页）+ api + worker + Redis；**PostgreSQL 优先用托管服务**，预算不足时同机运行并做异地备份 | 0 – 1 万 DAU |
| B 放量 | 托管 PostgreSQL（含自动备份与只读副本选项）+ 托管 Redis；api 与 worker 各 2 节点；Caddy 或云负载均衡（注意 WebSocket 空闲超时调大） | 1 万 – 10 万 DAU |

只有在监控显示单机成为瓶颈、且你愿意承担运维时，才考虑托管容器服务或 K8s。

### 15.2 交付流水线

- GitHub Actions：`golangci-lint` → 单元测试 → 集成测试（testcontainers 起 PostgreSQL 和 Redis）→ 构建镜像 → 推送镜像仓库 → SSH 触发 `compose pull && compose up -d`。
- 迁移：发布时先跑 `goose up`，迁移遵循“先加后删、向前兼容”，保证新旧版本短暂共存不报错。
- 发布时服务端先下发 `server.restart`，客户端自动重连，用户基本无感。

### 15.3 备份与恢复

- PostgreSQL 每日全量备份 + 持续归档（托管服务自带），备份保留 14 天。
- **上线前必须做一次真实恢复演练**，把“恢复步骤”写成文档放在 `deploy/RESTORE.md`。

### 15.4 配置管理（.env）

配置只通过环境变量注入。**本地**用 `.env` 文件（不进仓库，仓库里只放 `.env.example`）；**预发和线上**用平台的环境变量或密钥服务，线上不使用 `.env` 文件。服务启动时用 koanf 读取并校验，缺少必填项直接退出并列出缺哪些，不带着错误配置启动。

```env
# ===== 运行环境 =====
APP_ENV=local                  # local | staging | production
APP_PORT=8080
LOG_LEVEL=debug                # debug | info | warn

# ===== 数据库 / 缓存（必填）=====
DATABASE_URL=postgres://petbridge:petbridge@localhost:5432/petbridge?sslmode=disable
REDIS_URL=redis://localhost:6379/0

# ===== 登录与令牌 =====
JWT_SIGNING_KEY=local-dev-only-change-me   # 必填；线上必须是随机长密钥
ACCESS_TOKEN_TTL=15m
REFRESH_TOKEN_TTL=720h
APPLE_CLIENT_ID=                           # 本地可留空
GOOGLE_CLIENT_ID=                          # 本地可留空
DEV_LOGIN_ENABLED=true                     # 仅 local 允许为 true；APP_ENV 不是 local 时若为 true，启动直接失败

# ===== AI 网关 =====
AI_PROVIDER=fake                           # fake | live；本地默认 fake，不花钱
FAKE_AI_LATENCY_MS=300                     # 仅 fake：模拟延迟
FAKE_AI_FAIL_RATE=0                        # 仅 fake：模拟失败比例 0~1，用于降级演练
LLM_PRIMARY_BASE_URL=
LLM_PRIMARY_API_KEY=
LLM_PRIMARY_MODEL_TRANSLATE=
LLM_PRIMARY_MODEL_POLISH=
LLM_SECONDARY_BASE_URL=
LLM_SECONDARY_API_KEY=
MT_FALLBACK_API_KEY=
AI_DAILY_BUDGET_USD=5
AI_POLISH_DAILY_QUOTA=30

# ===== 推送 / 存储 / 监控 =====
FCM_CREDENTIALS_FILE=                      # 留空则推送只打日志，不真正发送
S3_ENDPOINT=http://localhost:9000          # 本地用 MinIO
S3_BUCKET=petbridge-dev
S3_ACCESS_KEY=minioadmin
S3_SECRET_KEY=minioadmin
SENTRY_DSN=

# ===== 业务默认值（运行后以 app_config 表为准，可热改）=====
ROOM_CAPACITY=5
CODE_TTL_DAYS=7
REWARD_CODE_COUNT=3
REWARD_CODE_LIFETIME_CAP=20     # 口径：每用户除种子码外所有来源累计获得的码数
OWNER_REGEN_DAILY_LIMIT=3       # 房主每日自助补码上限
```

| 项 | 本地 | 预发 / 线上 |
| :-- | :-- | :-- |
| 必填（缺则启动失败） | `DATABASE_URL`、`REDIS_URL`、`JWT_SIGNING_KEY` | 同左；`AI_PROVIDER=live` 时还需 `LLM_PRIMARY_*`、`S3_*`、FCM 凭据 |
| 登录 | `DEV_LOGIN_ENABLED=true`，用开发登录 | 必须为 false，配置 `APPLE_CLIENT_ID`、`GOOGLE_CLIENT_ID` |
| AI | `AI_PROVIDER=fake` | `live`，使用真实供应商密钥；预发用测试额度 |
| 推送 | 留空，只打日志 | FCM 凭据 |
| 存储 | MinIO | 云对象存储 |
| 密钥来源 | `.env` 文件 | 平台环境变量 / 密钥服务 |

**安全规则**

- `.env` 写进 `.gitignore`；`.env.example` 只放占位和本地默认值，不含任何真实密钥。
- 密钥不进日志、不进镜像。
- `APP_ENV=production` 时启动自检：`DEV_LOGIN_ENABLED` 必须为 false、`AI_PROVIDER` 必须为 live、`JWT_SIGNING_KEY` 长度不少于 32，任一不满足就拒绝启动。

### 15.5 本地开发环境

所有功能都在本地跑通。本地只需要 Go 和 PostgreSQL / Redis / MinIO 三个依赖服务，不需要云账号、不需要真实 LLM，也不需要 Apple / Google 登录。

**依赖服务的两种运行方式**【假设，v1.2 新增】：

| 方式 | 说明 |
| :-- | :-- |
| A（推荐）Docker Desktop | 个人学习/开发使用免费；用下方 `compose.dev.yml` 一条命令起三个依赖，环境干净、可一键重置。Windows 需开启虚拟化（WSL2） |
| B 原生安装 | 不装 Docker 时，直接装 PostgreSQL 16；Redis 可用 WSL 里的 Redis 或 Windows 版 Memurai；MinIO 本阶段可跳过（`S3_*` 留空，海报由客户端渲染，后端不依赖存储即可跑通核心流程）。集成测试里的 testcontainers 依赖 Docker，选 B 时需改为连接本机数据库的专用测试库 |

**Windows 下没有 `make`**：下文命令以 Makefile 形式书写，Windows 上用 [go-task](https://taskfile.dev)（`go install github.com/go-task/task/v3/cmd/task@latest`）提供同名任务，写在 `Taskfile.yml` 里，即 `make up` 对应 `task up`。

**依赖服务（`deploy/compose.dev.yml`）**：只启动依赖，api 与 worker 在本机直接运行，便于断点调试。

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: petbridge
      POSTGRES_PASSWORD: petbridge
      POSTGRES_DB: petbridge
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
  redis:
    image: redis:7
    ports:
      - "6379:6379"
  minio:
    image: minio/minio
    command: server /data --console-address :9001
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    ports:
      - "9000:9000"
      - "9001:9001"
volumes:
  pgdata:
```

**常用命令（Makefile）**

| 命令 | 作用 |
| :-- | :-- |
| `make up` | 启动 PostgreSQL、Redis、MinIO |
| `make migrate` | 执行 goose 迁移 |
| `make seed` | 写入测试数据（见下） |
| `make dev` | 用 air 热重载启动 api；另开终端 `make worker` 启动 worker |
| `make test` | 单元测试 |
| `make test-int` | 集成测试（testcontainers，需要 Docker），含验证码并发测试 |
| `make smoke` | 冒烟脚本：登录 → onboarding → 入房 → 发消息 |
| `make load` | k6 本地小规模压测 |
| `make down` / `make reset` | 停止依赖 / 停止并清空数据卷 |

**首次启动步骤**

1. `cp .env.example .env`（本地默认值可直接使用）。
2. `make up`，再 `make migrate`，再 `make seed`。
3. 两个终端分别运行 `make dev` 与 `make worker`。
4. `curl localhost:8080/healthz` 与 `/readyz` 返回正常。

**本地替代外部依赖**

| 线上依赖 | 本地做法 |
| :-- | :-- |
| Apple / Google 登录 | `POST /auth/login/dev`（body 只含邮箱，直接签发 token）；只在 `APP_ENV=local` 且 `DEV_LOGIN_ENABLED=true` 时注册该路由【假设，本地与自动化测试专用】 |
| LLM / 机翻 | `AI_PROVIDER=fake`：返回“\[目标语言\] 原文”，可用 `FAKE_AI_LATENCY_MS`、`FAKE_AI_FAIL_RATE` 模拟慢与失败 |
| FCM 推送 | 不配置凭据，推送只打印日志 |
| 对象存储 | MinIO（控制台 `localhost:9001`） |
| 内容审核 API | 只启用本地词库 |
| HTTPS / Caddy | 本地不需要，直接用 `http://` 与 `ws://` |

**测试数据（`make seed`）**

- 6 个测试用户：A 日语 ENFP、B 韩语 INFP、C 繁中 INTJ、D 简中 ESTJ、E 英语 ISFP、F 未成年人（用于验证年龄规则）。邮箱形如 `dev-a@local`。
- 零号种子验证码 `DEVA2345`，绑定 A 的小屋，用于测试入房。
- 一间已满员的小屋及其已作废的验证码 `FULL2345`，用于验证 `CODE_INVALID`（满员后验证码自动作废）；`ROOM_FULL` 只会在并发抢最后一席时出现，由并发集成测试覆盖。

**手动验证流程（人工冒烟）**

1. 用开发登录给 A、B 各拿一个 token。
2. 用 `wscat -c ws://localhost:8080/ws -H "Authorization: Bearer <token>"` 模拟两个客户端订阅同一小屋。
3. B 使用 `DEVA2345` 入房，A 的连接应收到 `member.joined`，B 收到奖励验证码。
4. A 发一条消息，B 的连接应收到 `message.new`，含原文与基础译文。
5. 停掉 Redis：消息收发仍应正常，限流退化为进程内。
6. 设置 `FAKE_AI_FAIL_RATE=1` 重启 worker：消息应以原文 + 未翻译标记送达。
7. 重启 api：客户端应收到 `server.restart` 并重连。

**让手机或模拟器访问本机后端**：api 默认监听 `0.0.0.0:8080`。Android 模拟器用 `10.0.2.2`，iOS 模拟器用 `localhost`，真机用电脑的局域网 IP（同一 Wi-Fi，防火墙放行 8080）；Android 真机也可用 `adb reverse tcp:8080 tcp:8080`。

### 15.6 上线前自检（本地 → 预发 → 线上）

| 阶段 | 环境 | 必做 |
| :-- | :-- | :-- |
| 本地 | `compose.dev.yml` + fake AI | `make test`、`make test-int`、`make smoke` 全部通过；停 Redis、`FAKE_AI_FAIL_RATE=1`、重启 api 三项故障演练通过 |
| 预发 | 与线上同配置（托管 PostgreSQL / Redis），`APP_ENV=staging` | `DEV_LOGIN_ENABLED=false`；用真实供应商测试额度跑通翻译与润色；k6 压测（用假供应商）；TestFlight / Play 内测真机联调；备份恢复演练 |
| 线上 | `APP_ENV=production` | 迁移先行；所有配置来自平台变量；启动自检通过；探活与告警开启；用 `cmd/tools/seedcodes` 生成零号种子码；先灰度台湾 |

---

## 16. 测试与压测策略

单人开发的测试原则：**只对出错代价最大的地方写重测试**。

| 类型 | 重点 |
| :-- | :-- |
| 单元测试 | 验证码生成与格式化、MBTI→宠物映射、语气路由、限流逻辑、短语模板 |
| 并发集成测试 | 100 协程抢同一房间最后一席；同一用户重复提交同一验证码（幂等）；奖励只发一次；房间满员后其余验证码自动作废；同一用户并发加入两间房（断言最多 2 间）；房主踢人/解散/补码与入房并发（不超员、不死锁，见 7.8） |
| AI 网关测试 | 用假供应商注入延迟与失败：超时、熔断打开与恢复、降级链路、缓存命中 |
| 契约测试 | 以 `openapi.yaml` 为准校验响应结构，前端同步使用生成的客户端 |
| 压测（k6） | ① 5 万 WS 长连接 + 心跳；② 房间内消息广播；③ 验证码 500 QPS 峰值；④ AI 调用打到限流边界；压测在预发环境，用假供应商，避免真实花费 |
| 故障演练 | 手动停 Redis、停主 LLM、杀 api 进程，验证第 12 章降级行为 |

---

## 17. 开发排期（单人，后端约 8 周，含缓冲）

| 周 | 内容 | 产出 |
| :-- | :-- | :-- |
| B1 | 仓库骨架、CI、OpenAPI 初稿、数据库迁移、配置与日志 | 能部署的空服务 |
| B2 | 登录（Apple/Google）、token、onboarding、宠物映射 | 前端可注册 |
| B3 | 验证码生成、入房事务、奖励发放、限流冷却、房主踢人/自助补码/退出（7.8）、并发测试 | **核心机制跑通** |
| B4 | WS Hub、presence、事件分发、断线重放 | 小屋实时同步 |
| B5 | 消息收发、短语模板、AI 网关（缓存/熔断/降级/配额） | 跨语言消息可用 |
| B6 | 内容审核、举报屏蔽、未成年人规则、房主解散、打卡与装扮 | 合规与养成 |
| B7 | 推送、Widget 快照、分享卡片数据、账号删除 | 功能补齐 |
| B8 | 压测、故障演练、监控告警、备份恢复演练、灰度准备 | 可灰度上线 |

前端开发与后端交错进行，整体里程碑见前端 TRD 第 20 章。上表为估算，建议每周末对照实际进度调整，**优先砍的是装扮、手机号登录、MBTI 修改功能，不砍验证码与 AI 降级**。

---

## 18. 假设、风险与演进

### 18.1 假设与待确认

1. 验证码绑定到“用户自己的小屋”，单次使用、7 天有效，奖励数量默认 3、终身上限 20（所有来源累计，种子码除外）。满员后由房主通过移除成员/解散 + 自助补码（每日 3 个）重新打开邀请路径，MVP 小屋上限固定 5 人。
2. 用户最多同时在 2 间房：自己的 + 最多 1 间已加入的。
3. 润色有“发送方开关”和“接收方点击”两个入口。
4. 未成年人与成年人双向隔离：未成年人只能进入全员未成年的房间，成年人也不能进入含未成年成员的房间。
5. 登录方式 MVP 只做 Apple + Google。
6. **中国大陆**：已确认暂不首发。本文按海外单区域部署设计；大陆的独立部署、备案、生成式 AI 服务合规、厂商推送通道均不在 MVP 范围，后续进入大陆时再评估。
7. 供应商选择（LLM、机翻、审核）尚未定，本文只定义抽象接口。

### 18.2 主要风险

| 风险 | 应对 |
| :-- | :-- |
| 单人开发周期超期 | 严格按优先级砍功能，核心只保验证码、小屋、跨语言消息三件事 |
| 单点故障（单机阶段） | 备份演练、降级开关、外部探活；放量前切托管数据库 |
| AI 成本失控 | 手动润色、缓存、短语模板、每日预算熔断 |
| 验证码被黑产囤积 | 关系树风控、设备限制、码有效期、奖励上限 |
| 长连接数超预期 | 压测确定单节点上限，事件分发已支持多节点 |

### 18.3 演进路线（有数据再动）

1. 监控显示 api 单机 CPU 或连接数逼近上限 → 加第二个 api 节点（无需改代码）。
2. 数据库 CPU 持续偏高 → 先加索引与只读副本，再谈消息表分区（需配套 `msg_idempotency` 表，见 6.1 说明）。
3. Redis Pub/Sub 成为瓶颈 → 按房间哈希拆多个频道，再考虑 NATS。
4. 团队扩大后 → 先拆出 AI 网关为独立服务，其次才是消息服务。

---

## 19. 与 PRD 的对应关系

| PRD 需求 | 本文章节 |
| :-- | :-- |
| F1 注册与 MBTI 宠物 | 7.1、第 6 章 users |
| F2 验证码与裂变、防刷、并发 | 6.2、7.2、12.1、13 |
| F3 小屋 5 人实时、房主管理 | 7.3、7.8、第 9 章 |
| F4 AI 信使与性能保障 | 7.4、第 10 章、12.2 |
| F5 养成、Widget、推送 | 7.5、7.6、7.7 |
| F6 运营与风控后台 | `app_config`、14 章看板；管理后台 MVP 用 SQL + Grafana + CLI 工具代替，不单独开发（PRD 将 F6 列为 P0，评审时需确认该替代方案视为达标） |
| 非功能指标 | 第 2 章、第 16 章 |
