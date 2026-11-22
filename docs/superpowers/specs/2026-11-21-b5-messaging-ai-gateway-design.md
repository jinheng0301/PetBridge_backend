# B5 设计：消息收发、短语、AI 网关

日期：2026-11-21　开发方式：直接在 `main` 上开发，每个计划任务完成后分阶段提交，用户审阅后再推送到 `jinheng` main
依据：后端 TRD v1.8 的 4.5②、6.1、7.4、第 8–12 章、15.4、15.5、17；PRD v1.7 的 F4、第 8 章、第 13 章；前端 TRD v1.8 的 4.5②③、9.2、11.4、12、18。
状态：草案，待用户审阅。标【我的决定】的条目是写文档时我自己拍板的，审阅时可以逐条改；4.2–4.4 的方案选择同样待确认，计划按本文现有决定来写，用户改了哪条就先改本文再改计划。PRD、两份 TRD 的同步（第 6 章）在写计划之前完成，不是计划任务。

## 1. 意图

前端 W7–8 要做 F3 聊天：信使卡片、常用短语、离线发送队列（outbox）。这些都要后端提供消息收发和翻译。B5 交付的就是这一块，即里程碑 M3“跨语言消息可用”的后端部分：同一小屋里母语不同的人，能用自己的母语发消息，对方看到的是自己母语的译文。可以选择按接收方的 MBTI 润色语气，AI 出故障时消息照样送达。

用户要求 B5 **接真实的 LLM**，选定 **Gemini 做主供应商、Claude 做备用**（2026-11-21）。本地默认仍用假 AI（不花钱），要看真实效果时切到 live。

## 2. 目标 / 非目标 / 反目标

**目标**
- 发送消息：群聊（整个小屋）和私聊（同一小屋里的一个人）；文本消息和短语消息；按 `client_msg_id` 幂等
- 拉取消息：按游标分页，向前翻历史、向后补断线期间的消息
- 短语模板：`GET /config/phrases`，发短语不调用 AI
- 实时事件：`message.new`、`message.translated`、`message.polished`
- worker 进程（`cmd/worker`，asynq）：翻译与润色在 worker 执行
- AI 网关：Gemini → Claude 的供应商链；缓存、全局并发上限、每个供应商一个熔断器、超时；都失败时消息仍然送达，带“暂无法翻译”标记
- 润色：发送方开关和接收方按钮两个入口；后端 TRD 10.3 的提示词与程序校验；每人每日配额、每分钟上限、全局每日预算
- 成本统计：`ai_usage_daily`
- PRD、两份 TRD、三份变更记录同步；`api/openapi.yaml` 随路由在计划中更新（见第 6 章）

**分两段交付**【我的决定，与 B3 拆 B3a / B3b 同理】
- **B5a 消息与翻译**：迁移 0004、短语、发送、拉取、`message.new` / `message.translated`、worker、翻译网关（假 AI / Gemini / Claude、缓存、熔断、降级、重试、补偿扫描）、全局成本累加与翻译硬上限。B5a 完成后前端就能联调完整的聊天，只是没有润色
- **B5b 润色与成本**：两个润色入口、`message.polished`、10.3 的提示词与校验、配额与预算、`ai_usage_daily`（迁移 0005）、评测工具

**非目标（留给后续）**
- 内容审核（前置与后置词库、外部审核 API）、举报、屏蔽：B6。B5 只留接口（5.9）
- 离线推送：B7。不在线的接收者打开 App 后通过拉取看到消息
- 已读（`POST /deliveries/{id}/read`）：B7 与 Widget 一起做【我的决定：PRD 没有已读回执，现在没有用到它的界面】
- 30 天到期脱敏 / 删除的定时任务：B7 与账号删除一起做
- 贴纸（PRD F4.9，P1）、点踩反馈（F4.8，P1）
- 机器翻译兜底：MVP 不接【我的决定：又是一家供应商、一笔费用；两家 LLM 都失败的概率很低，失败时显示原文并自动重试】
- 语言识别：源语言就是发送者的母语（`users.native_lang`）【我的决定：用户在输入框里写自己的母语（PRD F4.1）】
- 润色“标准”档的开放、用户自定义敬语等级（后端 TRD 10.3.4、10.3.10）
- 多节点下的 AI 并发上限（只在本进程内限制）

**反目标**（出现任何一条即视为失败）
- AI 全部失败、Redis 停止或队列积压时，接收者收不到消息
- 被移除或已退出的人还能收到该房间之后的消息或消息事件，或能拉到自己不在场时的消息
- 未成年人能发私聊或收到私聊
- 同一条消息因客户端重试而出现两次
- 发给 AI 供应商的内容里带有用户 id、昵称等身份信息；API key 出现在日志或仓库里
- 有真实用户的环境用了 Gemini 免费层（免费层的内容会被 Google 用来改进产品，见 5.6）
- 一个失控的用户或一次故障让 AI 费用超出每日预算

## 3. 约束

- 沿用分层 `handler / task → service → repository / platform`；service 只依赖 `Translator` / `Polisher` 接口，不依赖具体供应商（后端 TRD 4.6）
- 新依赖：`github.com/hibiken/asynq`、Gemini 的官方 Go SDK `google.golang.org/genai`、Anthropic 官方 Go SDK `github.com/anthropics/anthropic-sdk-go/v2`；`github.com/google/uuid` 已在依赖树里，生成 UUIDv7 用它
- 数据访问沿用现有的手写 pgx 写法（仓库里没有用 sqlc）
- 本地 PostgreSQL + Memurai，无 Docker；需要 Redis 的测试在 Redis 不可用或 `-short` 时跳过；集成测试串行（`go test -p 1`）
- 不改已有迁移；新增 0004（B5a）、0005（B5b）
- Redis 只存可丢失的数据（后端 TRD 11）：消息与译文全在 PostgreSQL，Redis 丢了不能丢消息
- 自动化测试一律用假供应商，不调用真实 API、不花钱

## 4. 方案比较

**4.1 供应商（用户已选）**
- **A. Gemini 主力 + Claude 备用（采用）**：开发期 Gemini 免费层零成本；Claude Haiku 5.5 很便宜（输入每百万 token $0.10、输出 $0.50），作为备用让熔断、切换供应商这条路能真的测到。上线前用评测集（后端 TRD 10.3.9）比较两家再定谁当主力。
- B. 只接 Gemini，备用用假 AI：备用路径只能在假 AI 上演练。
- C. 只接 Claude：没有免费额度，开发期也要充值。

**4.2 接收者何时看到消息（我的决定，待确认）**
- **A. 落库后立即推 `message.new`（带原文、译文待定），译文好了再推 `message.translated`（采用）**：接收者几乎立刻看到卡片（前端 11.4 的“翻译中”骨架屏），队列积压或 AI 变慢时也不会“消息消失”，正好对上后端 TRD 12.2 的“先发原文卡片，译文到达后补发”。代价是多一种事件。
- B. TRD 7.4 原写法，译文好了才推 `message.new`：事件少一种，但 AI 一慢接收者就什么都看不到，与 12.2 和前端骨架屏矛盾。弃用。

**4.3 断线后怎么补消息（我的决定，待确认）**
- **A. 从 PostgreSQL 拉（采用）**：重连、重新订阅之后，客户端用 `GET /rooms/{id}/messages?after=<本地最新消息 id>` 补齐。消息本来就要落库，拉取接口本来就要做，这样不需要任何额外的存储。
- B. Redis Stream `room:{rid}:events` 重放（B4 留给 B5 再评估）：要维护 Stream，Redis 丢失时还得再回 PostgreSQL 兜底，等于做两遍。弃用，TRD 第 11 章删掉这个键。

**4.4 Redis 不可用时翻译怎么办（我的决定，待确认）**
- **A. 入队失败就在 api 进程内直接翻译（有并发上限），再加 worker 的补偿扫描（采用）**：单节点阶段 Redis 停了，消息照样翻译、照样通过 B4 的本机兜底推送出去，与后端 TRD 12.2“消息收发不受影响”一致。进程内满了或崩溃留下的“待翻译”，由 worker 每分钟扫一次补上。
- B. 只靠 worker：Redis 停了 asynq 也停了，消息一直停在“翻译中”，直到 Redis 恢复。弃用。

## 5. 已定的设计

### 5.1 数据表（迁移 0004，B5a）

`messages`
- `id uuid primary key`，由服务端生成 **UUIDv7**（按时间递增，直接当分页游标）
- `room_id`、`sender_id`、`scope`（`room` / `dm`）、`to_user_id`（私聊时必填，群聊为空）
- `kind`（`text` / `phrase`）、`phrase_id`（短语消息才有）、`src_lang`（发送者当时的母语）、`body`（原文；短语消息存发送者语言的短语文字）
- `client_msg_id`（客户端生成，1–64 个字符，限字母、数字、`-`、`_`）、`polish_requested boolean`（发送方打开了润色开关且通过了配额检查，见 5.7）
- `mod_status text not null default 'ok'`（B6 使用）、`created_at`
- 唯一键 `(sender_id, client_msg_id)`；索引 `(room_id, id)`

`deliveries`（每个接收者一行，发送者自己没有）
- `id uuid primary key default gen_random_uuid()`、`message_id`（外键，级联删除）、`receiver_id`、`target_lang`（接收者当时的母语）
- `base_text`（基础译文，可空）、`translation_status`（`pending` / `done` / `failed`）、`translate_attempts int not null default 0`（已经完整跑过几次供应商链，用于日志与排查）
- `polished_text`（可空）、`polish_status`（`none` / `pending` / `done` / `fallback`，默认 `none`）、`polish_by`（谁触发、谁扣配额：发送方开关为发送者，按钮为接收者，可空）
- `read_at`（可空，B7 使用）、`created_at`、`updated_at`
- 唯一键 `(message_id, receiver_id)`；索引 `(receiver_id, message_id)`；补偿扫描用的部分索引 `(updated_at) where translation_status in ('pending', 'failed') or polish_status = 'pending'`

迁移 0005（B5b）：`ai_usage_daily(day date, user_id uuid, kind text, calls int, tokens_in bigint, tokens_out bigint, cost_micro bigint, primary key (day, user_id, kind))`，`kind` 为 `translate` / `polish`。

消息表仍不分区（后端 TRD 6.1 的说明不变）。

### 5.2 发送 `POST /api/v1/rooms/{id}/messages`

请求体：`client_msg_id`、`kind`（`text` / `phrase`）、`text`（`kind=text` 时必填）或 `phrase_id`（`kind=phrase` 时必填）、`scope`（默认 `room`）、`to_user_id`（`scope=dm` 时必填）、`polish`（布尔，默认 false，只对文本消息有效）。幂等以请求体里的 `client_msg_id` 为准，前端拦截器带的 `Idempotency-Key` 头服务端不读【我的决定】。

同步段（api，目标 < 100ms）：
1. 校验：调用者在该房间有座位，否则 404 `ROOM_NOT_FOUND`；文本去掉首尾空白后 1–500 个字符（按 Unicode 字符计）；`phrase_id` 必须存在；私聊对象必须是同一房间的其他在席成员，且**双方都是成年人**，否则 403 `AGE_RESTRICTED`；格式问题 400 `VALIDATION_FAILED`
2. 限流：每用户每分钟 20 条、每日 1000 条（后端 TRD 12.1），429 `RATE_LIMITED`；Redis 不可用时退化为进程内计数（复用 `ratelimit.NewFallback`）
3. 前置审核：调用 `Moderator`（B5 为全部放行，5.9）
4. 润色开关：`polish=true` 且是文本消息时，检查并扣发送者的润色配额（5.7）；配额不足**不拒绝消息**，只是 `polish_requested=false`，客户端据此提示“今日润色次数已用完”
5. 一个事务内：以 `(sender_id, client_msg_id)` 幂等写入 `messages`；为每个接收者写一行 `deliveries`。接收者 = 群聊时该房间除发送者外的所有在席成员，私聊时 `to_user_id` 一人
   - 与发送者同语言的接收者：`base_text = 原文`，`translation_status = done`
   - 短语消息：`base_text` = 该短语在接收者语言下的文字，`done`
   - 去掉空白、标点和 emoji 后什么都不剩的文本（如“！！”、“👍👍”）：`base_text = 原文`，`done`，不调用 AI【我的决定】
   - 其余：`pending`
6. 提交后：给每个接收者发一条 `message.new`（各自的 delivery），给发送者也发一条（`delivery` 为空，用于同步发送者的其他设备）；有 `pending` 的就入队 `translate:{message_id}`（asynq 的 TaskID 用 message_id 去重）。入队失败走 5.5 的进程内翻译
7. 返回 **202** 与发送者视角的 `MessageView`（5.4）

**重放**：同一 `client_msg_id` 再次提交，直接返回已有消息的当前 `MessageView`（202），不再扣配额、不重新发事件、不重新入队。新的请求体与原来不同也照样返回原消息【我的决定】。

接收者列表在发送那一刻确定：之后入房的人看不到之前的消息，之后离开的人保留已经收到的。

### 5.3 拉取 `GET /api/v1/rooms/{id}/messages`

- 参数：`before`（消息 id）、`after`（消息 id）、`limit`（默认 30，最大 100）。二者都不给：最新的 `limit` 条；给 `before`：紧挨着它之前的 `limit` 条；给 `after`：紧挨着它之后的 `limit` 条。`before` 和 `after` 不能同时给
- 返回 `{ items: MessageView[], has_more }`，`items` **一律按 id 从旧到新**；`has_more` 表示该方向还有更多
- 只返回调用者能看的：自己发的，或有自己 delivery 的（私聊只有双方能看到）。调用者必须当前在该房间有座位，否则 404 `ROOM_NOT_FOUND`
- 用途：进入聊天页先渲染本地缓存，再用 `after` 补齐；向上滚动用 `before`；重连并重新订阅后用 `after` 补断线期间的消息（4.3）

### 5.4 `MessageView` 与实时事件

`MessageView`（REST 响应和三种事件共用同一结构）：

```
id, room_id,
sender: { user_id, nickname, pet_type },
scope: room | dm, to_user_id (可空),
kind: text | phrase, phrase_id (可空),
src_lang, text (原文),
client_msg_id (只在发送者自己的视图里出现，其他人为空),
polish_requested,
created_at,
delivery: null | { id, target_lang, base_text (可空), translation_status, polished_text (可空), polish_status }
```

发送者的视图 `delivery` 为空（发送者看原文即可）；接收者的视图只有自己那一行 delivery。

| 事件 | 何时 | 发给谁 | 负载 |
| :-- | :-- | :-- | :-- |
| `message.new` | 消息落库后（api） | 每个接收者，以及发送者本人 | `room_id`、`message`（各自视角的 `MessageView`） |
| `message.translated` | 某个 delivery 的翻译结束，成功或失败（worker，或 api 进程内翻译） | 该接收者 | 同上 |
| `message.polished` | 某个 delivery 的润色结束（B5b） | 该接收者 | 同上 |

- 三种事件**都不带 seq**，都按房间投递：只投给订阅了该房间的连接（B4 Hub 的 `KindTransient` 规则）【我的决定：被移除时 Hub 会取消订阅，保证被移除的人收不到之后的事件；用户最多 2 间房，客户端本来就会把两间都订阅上，另一间房的聊天也能实时更新】
- worker 发事件前确认接收者仍在席，不在席就不发
- **客户端规则（写入前端 TRD）**：三种事件都是“按 `message.id` 插入或更新”，到达顺序无关。delivery 的状态只前进不后退：`translation_status` 按 `pending < failed < done`（失败后自动重试成功可以变成 `done`，`done` 不会退回），`polish_status` 按 `none < pending < done / fallback`。收到比本地更旧的状态就忽略
- `room:{rid}:seq` 与 B4 的状态事件不受影响

### 5.5 翻译（worker，B5a）

任务 `translate:{message_id}`：
1. 读消息和所有 `pending` 的 delivery，按 `target_lang` 分组，同一语言只翻译一次（PRD F4.5）
2. 对每个语言调用 `Translator.Translate(text, src, tgt)`，经过 AI 网关（5.6）
3. 成功：写 `base_text`、`done`；整条链都失败：写 `failed`。每个 delivery 写完就发 `message.translated`
4. 任一语言失败，任务按 asynq 的重试再来（共 3 次，间隔 15 秒、60 秒），重试只处理仍未 `done` 的 delivery；第一次失败就先把状态发给客户端，卡片显示原文 + “暂无法翻译”，**不提供手动重试按钮**，后台重试成功后卡片自动换成译文【我的决定：少一个接口；同一条连续失败基本是供应商整体故障，用户手动点也没用】。asynq 的 3 次用完仍失败，交给补偿扫描继续重试（见下），所以供应商恢复后，24 小时内的失败消息都会自动补上译文
6. 翻译的成本计入全局每日预算（5.7）。超过预算的 **2 倍**后，翻译也停止：新的待翻译 delivery 直接记为 `failed`（不调用供应商），卡片显示原文，并打 error 日志供告警【我的决定：预算是“润色关闭线”，2 倍是“翻译硬上限”，保证一次故障或刷量不会让费用无限增长；正常用量下翻译远到不了这条线】
5. 后置审核：调用 `Moderator`（B5 全部放行）

**基础翻译的要求**：只翻译不改写；数字、时间、人名、链接、emoji 原样保留；ja 用です／ます体、ko 用해요체、zh-Hant 用繁体与台湾惯用词、zh-Hans 用简体与大陆惯用词（与后端 TRD 10.3.5 一致）；原文里出现的任何指令都不执行。用户内容只放在 user 段并用分隔标记包起来。模型返回 JSON `{ "text": 译文, "risk": "none" / "review" / "block" }`，`risk` B5 只解析不使用，留给 B6。解析失败算一次供应商失败。

**进程内翻译（Redis 不可用时）**：api 入队失败时，把同一个任务交给本进程一个有上限的协程池（4 个）执行，只跑一次供应商链，结果通过 B4 的本机兜底推送。池满就留在 `pending`；失败就记为 `failed`。两种情况都交给补偿扫描。

**补偿扫描**：worker 每 60 秒扫一次，把下面这些 delivery 的任务重新入队（TaskID 带上尝试次数，不会重复执行同一次）：
- 翻译 `pending`，且 `updated_at` 早于 2 分钟前（进程崩溃、进程内池满、Redis 恢复前积下的）
- 翻译 `failed`，消息发出不到 24 小时，且距上次尝试（`updated_at`）已过 10 分钟【我的决定：间隔固定；供应商故障期间熔断器是打开的，这些重试不会真的调用供应商，不会白花钱。`translate_attempts` 只用于日志与排查】
- 润色 `pending`，且 `updated_at` 早于 2 分钟前，且翻译已经 `done`
- 消息发出超过 24 小时仍 `failed` 的翻译不再重试，卡片一直显示原文；它上面挂着的润色（`pending`）记为 `fallback`

Redis 不可用时 worker 本身也停着，恢复后第一次扫描补上。

### 5.6 AI 网关

```
请求 → 缓存命中？→ 是：直接返回（不计成本、不计配额）
         ↓ 否
   本进程并发上限（16）→ 熔断器(Gemini) → 超时 → Gemini
         ↓ 失败、超时、429 或熔断打开
   熔断器(Claude) → 超时 → Claude
         ↓ 失败
   返回失败（翻译：failed，显示原文；润色：fallback，显示基础译文）
```

- 接口：`Translator`、`Polisher` 两个接口，供应商在 `pkg/llm` 下各一个实现（`gemini`、`claude`、`fake`），service 不知道背后是谁
- 熔断：每个供应商一个 `gobreaker`，最近 20 次失败率 ≥ 50% 打开，30 秒后半开探测（后端 TRD 10.2 不变）；翻译与润色共用该供应商的熔断器
- 超时：翻译 3 秒，润色 6 秒（后端 TRD 10.2 不变）
- 缓存：`tr:{sha256(规范化原文)}:{src}:{tgt}:{pv}`，7 天；规范化 = Unicode NFC、去首尾空白、连续空白合为一个；`pv` 是提示词版本号（代码常量，改提示词就升）。润色缓存见 5.7。Redis 不可用时直通
- **模型**：都写在配置里，代码不写死（5.10）。Claude 默认翻译和润色都用 `claude-haiku-5-5`；Gemini 的模型在 AI Studio 里选当时免费层可用的型号，写进 `.env`【我的决定】。调用 Claude Haiku 5.5 时 `effort` 设为 `low`（翻译和轻档润色不需要深度思考，省 token、降延迟）
- **结构化输出**：Gemini 用 JSON schema 约束的响应格式，Claude 用 `output_config.format`；两家都要求只返回约定的 JSON
- **隐私**：发给供应商的只有原文、基础译文、语言和（润色时）接收方 MBTI，不带任何用户标识（后端 TRD 10.2）
- **Gemini 免费层只能用于开发**：免费层的输入输出会被 Google 用来改进产品，并可能有人工审阅（Gemini API 附加条款），只能发测试消息。配置项 `GEMINI_FREE_TIER`（默认 true）；`APP_ENV=production` 时，只要 Gemini 在链上（主或备用）就必须为 false，否则拒绝启动。有真实用户之前要么升级 Gemini 付费层，要么把 `AI_PRIMARY` 改成 `claude`
- **主备顺序**：`AI_PRIMARY=gemini | claude`（默认 gemini），另一家自动当备用。live 模式下主供应商的 key 必填；备用的 key 可空，空着就没有备用，主供应商失败即视为整条链失败。所以 production 可以只配 Claude、不配 Gemini【我的决定】
- 假供应商：两个假实现分别占主、备两个位置，`FAKE_AI_FAIL_RATE` 控制主供应商的失败比例、`FAKE_AI_SECONDARY_FAIL_RATE` 控制备用；翻译结果为“[目标语言] 原文”，润色结果为“基础译文 ~”（B5b 实现时改：“[MBTI] 基础译文”必然触发 10.3.7 的类型名校验）

### 5.7 润色（B5b）

**两个入口**（后端 TRD 7.4，共用缓存和配额）：
- **发送方开关**：`POST /messages` 带 `polish=true`。扣发送者**一次**配额（不论有几个接收者）【我的决定】；发送时就把每个接收者 delivery 的 `polish_status` 设为 `pending`、`polish_by = 发送者`（这样补偿扫描能找回崩溃中丢掉的润色）。翻译完成（`done`）后由翻译任务入队 `polish:{delivery_id}`；翻译最终放弃（24 小时）时润色记为 `fallback`。配额不退还【我的决定：失败很少见，退还要多一套记账】。极短文本在发送时就判断，直接 `done`，不入队
- **接收方按钮**：`POST /api/v1/deliveries/{id}/polish`，只有该 delivery 的接收者能调用（否则 404 `DELIVERY_NOT_FOUND`）。扣接收者一次配额，`polish_status` 设为 `pending`，`polish_by = 接收者`，入队，返回 202 与接收者视角的 `MessageView`
  - 已经 `pending` / `done` / `fallback`：直接返回当前视图，不再扣配额
  - 翻译还没 `done`，或是短语消息：409 `POLISH_NOT_READY`（客户端只在可润色时显示按钮，正常不会触发）
  - 配额或预算不足：429 `QUOTA_EXCEEDED`，`details.reason` 为 `daily_quota` / `minute_limit` / `global_budget` / `disabled`；`minute_limit` 时带 `Retry-After` 头

**配额与预算**：
- 每人每日 30 次（`AI_POLISH_DAILY_QUOTA`），“每日”按用户自己的时区（复用 `LocalDayBounds`），键 `quota:polish:{uid}:{yyyymmdd}`；每人每分钟 6 次，键 `rl:polish:u:{uid}`
- 极短文本（后端 TRD 10.3.1：去掉标点与 emoji 后不超过 3 个字，英文不超过 2 个词）：润色结果直接等于基础译文、`done`，**不扣配额、不调用模型**；这个判断在扣配额之前
- 全局每日预算 `AI_DAILY_BUDGET_USD`：每次真实调用（翻译和润色）后按返回的 token 数和代码里的价目表估算成本，累加到 `ai:cost:{yyyymmdd}`（UTC 日期）；超过预算后新的润色一律拒绝（`global_budget`），基础翻译继续（后端 TRD 12.2），直到 2 倍预算的翻译硬上限（5.5 第 6 步）。Gemini 免费层的调用成本记为 0。成本累加与硬上限在 **B5a** 就做（翻译从 B5a 起就花钱），`ai_usage_daily` 的明细在 B5b
- `AI_POLISH_ENABLED=false` 时润色整体关闭（`disabled`），供应急使用；改环境变量后重启生效【我的决定：`app_config` 表还不存在，B5 不建它；到了需要热切换时再做】
- Redis 不可用时，配额、限流、预算都退化为进程内计数（单节点下准确，重启清零）

**润色任务** `polish:{delivery_id}`：按后端 TRD 10.3 执行：
- 改写对象是基础译文；发送方与接收方同语言时改写原文（10.3.1）
- 提示词由公共规则、目标语言片段、档位片段、接收方 MBTI 四个维度片段拼成（10.3.8），片段写在代码文件里，改任何片段都升 `prompt_version`
- 缓存键 `po:{sha256(规范化基础译文)}:{src}:{tgt}:{接收方MBTI}:{档位}:{pv}`，7 天
- 程序校验按 10.3.7 的五项；第 1 项（解析失败）算供应商失败，换备用；第 2–5 项（改写越界）直接 `fallback`，不换供应商重试
- 两家都失败也记为 `fallback`。`fallback` 时 `polished_text` 为空，卡片显示基础译文并标“未润色”
- 结束后写 delivery、发 `message.polished`；每次真实调用都计入 `ai_usage_daily`（`user_id = polish_by`）和全局预算；校验不通过按校验项分类记录日志与指标
- 档位只有“轻”（10.3.4）

**B5b 实现时补充的决定**（2026-11-22，详见后端 TRD v1.12 与计划 `docs/superpowers/plans/2026-11-22-b5b-polish-cost.md`）：重放在扣配额之前判定，并发首发最多多扣 1 次、不退还；接收方按钮的认领与扣配额在同一个事务里，扣配额被拒就回滚，连点只扣 1 次；极短文本的发送方开关不扣配额、`polish_requested=true`；润色入队失败交给补偿扫描，不在 api 进程内润色；网关缓存先于预算检查；长度校验另设 4 个字符的下限；中日文数字只在紧跟单位字时换算，单独一个“一”和“十分”（非常 / 足够）不算数字；MBTI 类型名只拦截基础译文里没有的；`ai_usage_daily` 用 UTC 日期、`user_id` 无外键。

**评测工具**（B5b）：`cmd/tools/polisheval` 读评测集（每种目标语言至少 40 句，覆盖 10.3.9 列出的各类句子），对 16 型逐一调用指定供应商，输出 10.3.7 校验通过率，并导出一份供母语者人工打分的表格。评测集与工具在 B5b 交付；**达到 10.3.9 门槛是有真实用户之前的上线条件，不是 B5b 的完成条件**（人工打分要找母语者）。

翻译的成本同样计入 `ai_usage_daily`（`kind = translate`，`user_id` = 发送者），在 B5b 加上；B5a 只打日志和指标。

### 5.8 短语 `GET /api/v1/config/phrases?v=`

- 短语表是代码常量（`internal/service/phrases.go`），带版本号 `version`（字符串，改任何文字就升）
- 先定 8 个：`good_morning`、`good_night`、`thank_you`、`sorry`、`ok`、`see_you`、`congrats`、`take_care`，每个有 ja / ko / zh-Hant / zh-Hans / en 五种文字。文字在实现时写好、给用户过目【我的决定】
- `v` 等于当前版本时返回 **304**；否则 200 `{ version, phrases: [{ id, texts: { ja, ko, zh-Hant, zh-Hans, en } }] }`
- 前端随包带一份同样的表作为兜底（前端 TRD 已有）；发送时只传 `phrase_id`，服务端按版本无关的 id 取文字，旧客户端发的 id 只要还存在就能用
- 路由在“登录且已 onboarding”分组（后端 TRD 4.4）

### 5.9 为 B6 留的接口

- `Moderator` 接口：`CheckOutgoing(text, lang)`、`CheckTranslated(text, lang, risk)`，返回 通过 / 拒绝 / 待审。B5 的实现全部放行；`CONTENT_BLOCKED`（HTTP 422）错误码只定义不触发【我的决定：422 表示请求格式没问题、内容不被接受，与 400 的格式错误区分开】
- 屏蔽关系：B5 不检查，B6 加在 5.2 第 1 步

### 5.10 配置（替换后端 TRD 15.4 的 AI 网关一段）

```env
AI_PROVIDER=fake                     # fake | live；production 必须为 live
FAKE_AI_LATENCY_MS=300
FAKE_AI_FAIL_RATE=0                  # 假主供应商的失败比例 0~1
FAKE_AI_SECONDARY_FAIL_RATE=0        # 假备用供应商的失败比例 0~1
AI_PRIMARY=gemini                    # gemini | claude，另一家当备用
GEMINI_API_KEY=                      # Gemini 当主时必填；当备用时可空（空着就没有备用）
GEMINI_MODEL_TRANSLATE=              # 配了 GEMINI_API_KEY 就必填，在 AI Studio 选
GEMINI_MODEL_POLISH=
GEMINI_FREE_TIER=true                # production 且 Gemini 在链上时必须为 false
ANTHROPIC_API_KEY=                   # Claude 当主时必填；当备用时可空
CLAUDE_MODEL_TRANSLATE=claude-haiku-5-5
CLAUDE_MODEL_POLISH=claude-haiku-5-5
AI_POLISH_ENABLED=true
AI_POLISH_DAILY_QUOTA=30
AI_DAILY_BUDGET_USD=5
WORKER_CONCURRENCY=10                # asynq 并发数
```

- 删除原来的 `LLM_PRIMARY_*`、`LLM_SECONDARY_*`、`MT_FALLBACK_API_KEY`
- 协议与容量参数写成代码常量：消息 500 字、每分钟 20 条、每日 1000 条、润色每分钟 6 次、进程内 AI 并发 16、进程内翻译池 4、补偿扫描 60 秒 / 2 分钟、asynq 重试 3 次
- API key 只来自环境变量，不进日志、不进仓库；`.env.example` 只放空值

### 5.11 进程与代码落点

- `cmd/worker/main.go`：读配置、连 PostgreSQL 与 Redis、启动 asynq 服务端（队列权重：`translate` 6、`polish` 3、`default` 1）、补偿扫描；收到 SIGTERM 时等正在执行的任务结束再退出
- `internal/task/`：`translate_task.go`、`polish_task.go`、`sweep_task.go`，只调用 service
- `internal/service/`：`chat_service.go`（发送、拉取、润色入口）、`translation_service.go`（翻译任务）、`polish_service.go`（润色任务、10.3 校验）、`ai_gateway.go`（缓存、并发、熔断、供应商链）、`quota_service.go`、`phrases.go`、`moderation_service.go`（放行实现）
- `internal/repository/`：`message_repo.go`、`delivery_repo.go`、`ai_usage_repo.go`
- `internal/handler/chat_handler.go`、`config_handler.go`
- `pkg/llm/`：接口、`gemini/`、`claude/`、`fake/`；`gobreaker` 包在网关里
- `internal/platform/queue/`：asynq 客户端封装
- worker 发布事件用 B4 的 `EventPublisher`；worker 没有 Hub，所以 Redis 不可用时 worker 发的事件会丢，客户端靠重连后的拉取补上（worker 此时也没法工作，只影响在途的那一刻）
- 本地运行多一个终端：`task worker`

## 6. 文档与契约同步

本次（写计划之前）完成：
- **PRD v1.8**：8.1 去掉“语言识别”，源语言取发送者母语；8.3 降级链改为“主供应商 → 备用供应商 → 显示原文并自动重试”，MVP 不接机器翻译；13 章数据使用补一句：开发期使用的免费 AI 额度会被供应商用于改进产品，只能发测试数据，有真实用户前必须切换到不用于训练的服务；15.2 补待确认问题
- **后端 TRD v1.9**：第 3 章选型补 Gemini / Claude SDK；4.1、4.5② 改为落库即推 `message.new`；第 5 章目录；6.1 表字段；7.4 按 5.2–5.7 重写；第 8 章接口与新错误码（`DELIVERY_NOT_FOUND`、`POLISH_NOT_READY`、`QUOTA_EXCEEDED` 的 `details.reason`）、已读移到 B7；第 9 章三种消息事件与客户端规则；10.1、10.2 按 5.6 改（供应商、模型、免费层限制、去掉机翻）；第 11 章删 `room:{rid}:events`、加 `ai:cost:{day}`；12.2 降级矩阵；15.4 配置；15.5 本地运行与冒烟步骤；第 17 章 B5 拆为 B5a / B5b；18.1 假设
- **前端 TRD v1.9**：4.5② 时序图；第 7 章 reducer 的消息规则（按 id 插入或更新、状态只前进）；9.2 重新订阅后用 `after` 补消息；11.4 信使卡片的状态（翻译中、暂无法翻译且无手动重试、润色按钮的显示条件、发送方润色配额不足的提示）；12 章消息缓存用 `after` 增量拉取；8.2 与第 18 章新增错误码文案
- 三份变更记录各加一节；两个仓库根目录的六个文件保持完全一致（md5 校验）；前端仓库只暂存这六个文件

随路由在计划中完成：
- `api/openapi.yaml`：新增 `POST/GET /rooms/{id}/messages`、`POST /deliveries/{id}/polish`、`GET /config/phrases` 与 `MessageView` 等 schema、新错误码。契约测试要求文档路径与路由一一对应，所以与 B4 一样跟着路由一起改，不在本次文档同步里做

## 7. 测试

- **发送**：群聊为每个在席接收者各写一行 delivery；私聊只有对方一行；同语言接收者直接 `done`；短语按接收者语言取文字；只有 emoji 的文本不进队列；未成年人私聊 403；不在席 404；超长、空文本、未知 `phrase_id` 400；同一 `client_msg_id` 并发提交只写一条、都返回同一个 id
- **拉取**：`before` / `after` / 无游标三种方向、`has_more`；看不到自己入房之前的消息；看不到别人的私聊；离开后 404
- **事件**：发送后接收者和发送者各收到 `message.new`；翻译后接收者收到 `message.translated`；被移除者收不到之后的事件；状态只前进（单元测试覆盖客户端规则依赖的服务端不变式：`done` 不会被改回）
- **翻译任务**：同语言只调用一次供应商；主供应商失败切备用；两个都失败写 `failed`，重试成功变 `done`；缓存命中不调用供应商；熔断打开后直接走备用
- **Redis 不可用**：入队失败时进程内翻译并通过本机兜底推送；补偿扫描能捡起停在 `pending` 的 delivery
- **润色**（B5b）：两个入口各扣一次配额，重复点击不重复扣；极短文本不扣配额；配额、每分钟上限、全局预算、关闭开关各返回对应 `reason`；10.3.7 五项校验各有用例（数字变了、长度超了、新增 emoji、出现 MBTI 类型名、JSON 坏了）；越界直接 `fallback` 不换供应商；`ai_usage_daily` 累加正确
- **供应商实现**：Gemini 与 Claude 客户端用本地假 HTTP 服务器测请求格式、JSON 解析、超时与 429 处理，不访问外网
- **配置**：production 下 Gemini 在链上且 `GEMINI_FREE_TIER=true`、或 `AI_PROVIDER=fake`，拒绝启动；live 缺主供应商 key 拒绝启动；`AI_PRIMARY=claude` 且不配 Gemini 可以启动；日志不含 key
- **重试与预算**：asynq 重试用完后补偿扫描按间隔继续重试 `failed`，24 小时后放弃并把挂着的润色记为 `fallback`；超过 2 倍预算后新翻译直接 `failed` 且不调用供应商
- **契约测试**：新路由都在 `openapi.yaml` 中
- **冒烟**：`scripts/smoke.ps1` 补“A 发消息 → 拉取能看到”；WS 事件以端到端测试为准
- **人工验证**（不进自动化）：配上真实 key，A（日语）与 B（韩语）互发消息，看到真实译文；把 Gemini key 改错，确认切到 Claude；两个 key 都改错，确认显示原文并在恢复后自动换成译文

## 8. 留给实现者

- 提示词的具体措辞（须满足 5.5、5.7 与后端 TRD 10.3 的规则，改了就升 `prompt_version`）
- Gemini 与 Claude SDK 调用的具体写法、重试与 429 的识别方式
- 价目表的组织方式（按模型名查，未知模型记 0 并打警告）
- asynq 任务负载的 JSON 结构、队列名
- 进程内并发上限、翻译池的实现方式
- 各接口的事务与加锁细节（遵守全局加锁顺序 `users → invite_codes → rooms → room_members`，messages / deliveries 排在其后）
- 8 个短语的五语言文字（写好后给用户过目）
