# B6a 设计：内容审核、举报与屏蔽

日期：2026-11-23
状态：草稿，待用户审阅
上游：后端 TRD v1.12（7.4、8、9、11、13 章）、PRD F3.4 / F4.7 / 8.4 / 13 章、B5 spec 5.9

## 1. 背景与目的

TRD 17 章的 B6 是“内容审核、举报屏蔽、未成年人规则、打卡与装扮”。用户决定（2026-11-23）：

- B6 先面向**熟人内测**，不是应用商店送审。审核做到“挡住明显的坏内容、留下可查的记录”即可。
- 参照 B5 拆成两段：**B6a 合规**（本文：审核、举报、屏蔽、昵称过滤），**B6b 养成**（打卡与装扮，另写 spec）。先做 B6a。
- 审核方案选 **A：本地词库 + 模型返回的 `risk`**，不接外部审核 API。
- 屏蔽效果选 **2：双向不投递、双向不能私聊、对方无感知，不影响同屋**。

同时收掉 B5b 留给 B6 的两件事：`CheckTranslated` 判成 review/block 时照样存成 done（fail-open）；缓存只存译文，命中时 `risk` 一律为 none。

## 2. 现状（B5b 之后）

- `internal/service/moderation_service.go`：`Moderator` 接口（`CheckOutgoing`、`CheckTranslated`），实现是 `PassModerator`。三处调用（`chat_service`、`translation_service`、`polish_service`）判成非 pass 时只打日志。
- `messages.mod_status text default 'ok'` 已有，未使用；`deliveries` 没有审核字段。
- 接收者拿到的 `MessageView` 里带原文 `text`（F4.3 原文切换），`message.new` 在发送时就推给接收者。所以“译文审核不通过”时，接收者其实已经看到了原文卡片。
- 模型返回 `{text, risk}`，`risk ∈ none / review / block`；`AIGateway` 缓存命中时返回 `RiskNone`（`ai_gateway.go:178`）。
- 未成年人：入房双向隔离（B3）和私聊仅限成年人（B5，`AGE_RESTRICTED`）已经做完。B2 把昵称敏感词过滤推迟到 B6。
- 账号封禁：`users.status = 'banned'` 已被 `Auth()` 中间件和 `auth_service` 拒绝，每次请求都检查。
- `PATCH /me` 还没有实现，昵称目前只在 `POST /me/onboarding` 写入。
- 迁移到 `0005`；没有 `blocks`、`reports` 表。
- 管理后台按 TRD 19 章用 SQL + CLI 代替。

## 3. 目标、非目标、反目标

**目标**
1. 发送前审核：高危直接拒绝（422 `CONTENT_BLOCKED`），疑似照常发但留记录。
2. 翻译与润色后审核：译文高危的那份 delivery 对接收者隐藏；润色高危则回退到基础译文；疑似照常显示但留记录。
3. 缓存同时保存 `risk`，命中缓存时审核结果不变。
4. 昵称过滤（onboarding）。
5. 屏蔽：`POST /blocks`、`DELETE /blocks/{user_id}`、`GET /blocks`，即时生效，效果见第 6 章。
6. 举报：`POST /reports`，只落库。
7. 一个 CLI 工具，查看审核记录和举报、处理举报、封禁 / 解封账号。这就是 MVP 的“人工复核队列”。

**非目标（留给以后）**
- 外部审核 API（送审时再写一个新的 `Moderator` 实现接进来）。
- 管理后台页面、自动处置（按举报数自动封号等）、风控联动。
- 打卡与装扮（B6b）。
- 屏蔽影响入房、隐藏对方宠物或在线状态（用户选了方案 2，不做）。
- 词库热更新（改词库要发版重启）。

**反目标（做到了也算失败）**
- 被屏蔽的人能察觉自己被屏蔽（拿到错误码、看到“未送达”）。
- 审核让发送同步段明显变慢（目标仍是 < 100ms，词库匹配应在 1ms 量级）。
- 日志里出现消息正文、译文或昵称原文（TRD 13 章）。
- 词库误伤常见词，比如英文 `ass` 命中 `class`。

## 4. 方案比较（用户已选 A）

| 方案 | 做法 | 取舍 |
| :-- | :-- | :-- |
| **A 本地词库 + 模型 risk（选定）** | 发送前用 Aho-Corasick 匹配本地五语种词库；译文 / 润色后再过一遍词库，并结合模型返回的 `risk` | 同步段几乎不增加耗时，没有新的外部依赖；谐音、变体主要靠模型兜底 |
| B 只靠模型 | 发送前多调一次模型 | 同步段 100ms 保不住，成本增加，供应商熔断时审核随之失效 |
| C 外部审核 API | 接第三方审核服务 | 覆盖最好，但新增供应商、密钥、降级逻辑，熟人内测用不上；以后可作为新的 `Moderator` 实现接入，A 的结构不用推翻 |

屏蔽方案（用户已选 2）：
- 1 只挡单向：骚扰者仍能读到受害者的所有发言，不选。
- **2 双向不投递（选定）**。
- 3 再加禁止同屋：会让对方察觉，还要改入房事务，不选。

## 5. 内容审核

### 5.1 词库与匹配：`pkg/wordfilter`

- 新包 `pkg/wordfilter`，与业务无关：输入词条（词 + 级别），构建 Aho-Corasick 自动机，`Match(text) → (最高级别, 命中词列表)`。级别：`review`、`block`。
- **词库文件**：放在 `internal/service/modwords/`，用 `go:embed` 打进二进制，每个语种一个文件：`ja.txt`、`ko.txt`、`zh-Hant.txt`、`zh-Hans.txt`、`en.txt`。每行格式为 `block<TAB>词` 或 `review<TAB>词`，`#` 开头为注释。【我的决定：随代码发版，改词要重启；熟人内测改动不频繁，不做热更新】
- **所有语种的词合成一个自动机**，审核时不看文本声明的语言。用户可能混用语言，按语言分开匹配会漏。【我的决定】
- **种子词库**由实现者起草，每个语种 30–60 条，覆盖：自伤 / 自杀、诈骗与引流（转账、加 LINE / 微信 / KakaoTalk 等联系方式加金额）、色情、仇恨与严重辱骂、针对未成年人的不当内容。只有明确的、高危的才标 `block`，含糊的标 `review`。**词库要单独给用户过目**（提交前审阅本来就要做）。
- **归一化**（文本和词条都做同样处理）：
  1. NFKC（全角转半角、兼容字符合并），转小写。
  2. 片假名转平假名。
  3. 去掉零宽字符。
- **两种匹配方式**：
  - 词条全是 ASCII 字母 / 数字时，要求两侧是**词边界**（前后不是 ASCII 字母或数字），避免 `ass` 命中 `class`。这一类在“只做第 1–3 步、保留空格”的文本上匹配。
  - 其余词条（含 CJK、假名、韩文）做**子串匹配**，匹配文本要再去掉空白和常见分隔符（`. - _ * · ・ 、 ,` 等），挡住 “死 了 算 了” 这类插空格的写法。
  
  实现者可以用两个自动机，也可以一个自动机加边界判断，结果满足以上两条即可。
- 繁简不互转：繁体和简体词库各写一份。
- 性能要求：500 字的文本单次匹配远低于 1ms，进程启动时构建一次。

### 5.2 `Moderator` 的新实现：`WordlistModerator`

替换 `PassModerator`（`PassModerator` 保留给测试使用）。接口签名改为同时返回命中信息，供写审核记录用：

```go
type ModResult struct {
    Verdict ModVerdict // ModPass / ModReview / ModBlock
    Source  string     // "wordlist" / "model" / "both"；Pass 时为空
    Terms   []string   // 命中的词条（归一化后），最多 10 个
}
CheckOutgoing(ctx, text, lang) (ModResult, error)
CheckTranslated(ctx, text, lang, risk) (ModResult, error)
```

- `CheckOutgoing`：只看词库。
- `CheckTranslated`：词库结果和 `risk` 取更严的一个。`risk = "block"` → Block，`"review"` → Review，其他值（含 `none` 和未知值）→ 不升级。
- 本地词库不会出错，error 始终为 nil。接口保留 error，留给以后的外部 API 实现。

### 5.3 审核记录表 `mod_flags`（人工复核队列）

迁移 `0006` 新建：

| 字段 | 说明 |
| :-- | :-- |
| id uuid pk | |
| created_at timestamptz | |
| stage text | `outgoing` / `translation` / `polish` / `nickname` |
| verdict text | `review` / `block` |
| source text | `wordlist` / `model` / `both` |
| terms text[] | 命中的词条，可为空数组（纯模型判定时） |
| user_id uuid | 内容作者：发送者，或修改昵称的人 |
| room_id uuid null | |
| message_id uuid null | outgoing-review、translation、polish 时有 |
| delivery_id uuid null | translation、polish 时有 |
| target_lang text null | translation、polish 时有 |
| text text null | **只在内容没有别处可查时才存**：被拒的发送原文（消息不入库）、润色结果（block 时不入库）、昵称。其他情况为 null，到 messages / deliveries 里查 |
| status text | `open` / `reviewed`，默认 `open` |

- 索引：`(status, created_at)`。
- 只要 verdict 是 review 或 block 就写一条。写入失败只打日志，不影响主流程（审核结论本身已落在 messages / deliveries 上）。
- 保留期与消息一样是 30 天。清理任务随 TRD 13 章的消息保留任务一起做（不在 B6a）。【我的决定】

### 5.4 发送前（`chat_service.Send`）

位置不变：重放查找之后、扣润色配额之前（见 `chat_service.go:144`）。
- **短语消息跳过审核**（模板是我们自己写的）。
- **Block** → 返回 `ErrContentBlocked`（HTTP 422 `CONTENT_BLOCKED`，`details` 为空，不说明原因）。消息不入库、不扣润色配额，写一条 `mod_flags`（stage `outgoing`，存原文）。同一个 `client_msg_id` 重发会再次被拦，因为重放查不到。限流计数照常算（限流在审核之前）。
- **Review** → `messages.mod_status = 'review'`，照常投递，消息插入成功后写 `mod_flags`。
- 日志只记 `room_id`、`user_id`、verdict、命中词数量，不记原文和命中词。

### 5.5 翻译后（`translation_service`）

`deliveries` 新增 `mod_status text not null default 'ok' check (mod_status in ('ok','review','block'))`（迁移 0006）。它记录的是**这份译文**的审核结论。

一个语言组翻译成功后调用 `CheckTranslated`：
- **Pass**：与现在相同。
- **Review**：照常存 done，该组所有 delivery 的 `mod_status = 'review'`，每个 delivery 写一条 `mod_flags`。
- **Block，按来源区分**（用户决定，2026-11-23）：
  - **只有词库命中译文**（`source = wordlist`）：可能只是翻译用词碰巧命中，**只隐藏这一组**，按下面的方式处理。
  - **模型判 `risk = block`**（`source = model` 或 `both`）：说明原文有问题，**整条消息对所有接收者撤回**。具体做法：
    - `messages.mod_status = 'block'`。
    - 这条消息的所有 delivery 都置 `mod_status = 'block'`，包括同语言的、其他语言组已经完成的、还在 pending 或 failed 的。
    - 等待中的润色一律置 `fallback`。
    - 给每个仍在席的接收者发 `message.removed`。
    - 本次运行里还没处理的语言组不再调用模型。
    - 写一条 `mod_flags`，`stage = translation`，带 `message_id`，`delivery_id` 为空。
    
    之后对同一条消息的补偿扫描、重试，看到 `messages.mod_status = 'block'` 就直接跳过。
  
  只隐藏一组时：该组 delivery 存 `translation_status = 'done'`、`base_text` 照存（供复核）、`mod_status = 'block'`。
  - 有等待中的润色（`polish_status = 'pending'`）→ 直接置 `fallback`，不入队。
  - 不发 `message.translated`，改为给该接收者发新事件 **`message.removed`**（见 5.8）。
  - 每个 delivery 写一条 `mod_flags`。
- `SetTranslation` 增加 `mod_status` 参数，一次写完。

**目标语言与原文相同**的接收者不走模型（B5 现状），也就不做译后审核，因为发送前已经审过。翻译失败或放弃重试的 delivery 也不做译后审核，接收者继续看到原文；原文已经过了发送前的词库，熟人内测阶段可以接受。【我的决定】

### 5.6 润色后（`polish_service`）

- **Pass**：不变。
- **Review**：存 done，写 `mod_flags`（stage `polish`，存润色结果，便于复核）。`deliveries.mod_status` 不改，它只代表基础译文的结论。
- **Block**：不存润色结果，`polish_status = 'fallback'`（客户端继续显示基础译文，与现有 fallback 表现一致），写 `mod_flags`（存被拒的润色结果）。
- 不缓存 block 结果，见 5.7。

### 5.7 缓存存 risk

- 缓存值从纯文本改为 JSON `{"t": text, "r": risk}`。`Get` 读到非 JSON（B5 留下的旧值）一律当作未命中，重新调用模型，旧值最多 7 天后自然过期。【我的决定：不改键前缀，旧值当未命中就够了】
- 命中时返回存下的 `risk`，`Cached = true`。
- `risk = "block"` 的结果**不写缓存**，保证下一次仍走模型和词库。词库匹配本来每次都做，不依赖缓存。【我的决定：block 很少，不缓存也省不了多少钱】

### 5.8 隐藏的 delivery 对接收者的表现

- `messages.mod_status` 取值：`ok` / `review` / `block`。`block` 只会由 5.5 的整条撤回写入；发送前拦截的消息根本不入库。
- `ListForViewer`：viewer 自己的 delivery 是 `mod_status = 'block'` 时，这条消息对他不返回。整条撤回时，所有 delivery 都已经是 block，所以对所有接收者都隐藏。
- 整条撤回时，**发送者自己仍能看到这条消息**，界面上和正常消息一样，不暴露审核结论（与反目标一致）。
- `ViewsOf`：`Receivers` 里不放 block 的 delivery，worker 的 publish 自然跳过。
- **润色按钮必须在扣配额之前拦住**：`ChatService.Polish` 现在是先 `PolishWork` → 抢占 → 扣配额 → 提交 → 入队，最后才 `ViewsOf`。所以要让 `PolishWork` 带出 `mod_status`，并在抢占之前判断：`mod_status = 'block'` 或者和发送者有屏蔽关系，就返回 `DELIVERY_NOT_FOUND`，不扣配额。`PolishService.Process` 开头也做同样的检查，命中就置 `fallback`，不调用模型。【审查发现，我的决定】
- **新 WS 事件 `message.removed`**（S→C，不入 seq，与 `message.translated` 同类）：`{ room_id, message_id }`，只发给这份 delivery 的接收者。客户端收到后从列表里删掉这张卡片。这一步是必须的，因为原文卡片在 `message.new` 时已经显示过了。
- 发送者不受影响，看到的是正常已发送（不暴露审核结论，PRD 5.2“消息未能送达”只用于发送前拦截）。
- 补偿扫描：block 的 delivery 已经是 done，不会被重扫。

### 5.9 昵称

- `POST /me/onboarding` 字段校验通过后、**进入事务之前**，用 `CheckOutgoing` 检查昵称；放在事务里的话，422 会把 `mod_flags` 的写入一起回滚。已经 onboarding 过的重复调用照旧返回已存结果，不检查新昵称（与现在的幂等行为一致）。
  - Block → 422 `CONTENT_BLOCKED`，`details: {"fields": ["nickname"]}`，写 `mod_flags`（stage `nickname`，存昵称）。
  - Review → 放行，写 `mod_flags`。
- 以后实现 `PATCH /me` 改昵称时必须走同一个检查（写进 TRD 7.1）。

## 6. 屏蔽

### 6.1 数据

迁移 0006：`blocks(blocker_id uuid references users, blocked_id uuid references users, created_at timestamptz default now(), primary key (blocker_id, blocked_id))`，加索引 `(blocked_id)`，加约束 `check (blocker_id <> blocked_id)`。

### 6.2 接口（需登录且已 onboarding）

现在没有统一的 onboarding 中间件，各 handler 用 `h.authn`，onboarding 由 service 判断（`ErrOnboardingRequired` → 403 `ONBOARDING_REQUIRED`）。新接口（`/blocks*`、`/reports`）沿用同样做法。

| 接口 | 行为 |
| :-- | :-- |
| `POST /blocks` `{ "user_id": "<uuid>" }` | 204。已屏蔽时同样返回 204（幂等）。屏蔽自己、用户不存在、对方未 onboarding → 400 `VALIDATION_FAILED`（`fields: ["user_id"]`）。`user_id` 不区分大小写，存小写 |
| `DELETE /blocks/{user_id}` | 204；本来就没屏蔽也返回 204 |
| `GET /blocks` | 200 `{ "items": [ { "user_id", "nickname", "pet_type", "blocked_at" } ] }`，按 `blocked_at` 倒序，不分页 |

- 每人最多屏蔽 500 人，超出返回 409 `BLOCK_LIMIT`。【我的决定：防止滥用，正常用户碰不到】
- 屏蔽时不发任何 WS 事件，对方无感知；屏蔽者其他设备下次拉取时生效。

### 6.3 效果（“有屏蔽关系”= 任一方向存在 blocks 行）

1. **发群聊**：生成 deliveries 时，去掉与发送者有屏蔽关系的接收者。用一条查询取出“这些在席成员里与发送者有屏蔽关系的人”。`message.new` 也就不发给他们。
2. **发私聊**：对方与发送者有屏蔽关系时，照常返回 202，消息入库，但**不生成 delivery**，对方什么也收不到，发送方看起来一切正常。私聊的成年人校验（`AGE_RESTRICTED`）照旧在前面。
3. **拉历史**（`ListForViewer`）：发送者与 viewer 有屏蔽关系的消息不返回（viewer 自己发的消息照常返回）。屏蔽前已经收到的消息也一起隐藏，解除屏蔽后重新出现；屏蔽期间发的消息没有 delivery，解除后也看不到。
4. **在途的翻译 / 润色**：屏蔽前发出、屏蔽后才完成的，worker 推 `message.translated` / `message.polished` 前检查屏蔽关系，有就不推。润色按钮遇到有屏蔽关系的 delivery 返回 `DELIVERY_NOT_FOUND`。
5. **不受影响**：小屋快照、presence、`pet.tap`、入房、房主管理。

## 7. 举报

### 7.1 数据

迁移 0006：`reports`

| 字段 | 说明 |
| :-- | :-- |
| id uuid pk | |
| reporter_id、target_id uuid | |
| room_id uuid | 举报发生的房间 |
| message_id uuid null | 举报某条消息时有 |
| reason text | `harassment` / `spam` / `sexual` / `self_harm` / `minor_safety` / `other` |
| note text null | 补充说明，≤ 500 字 |
| status text | `open` / `resolved` / `dismissed`，默认 `open` |
| created_at、handled_at、handled_note | |

索引：`(status, created_at)`、`(reporter_id, created_at)`。

### 7.2 接口 `POST /reports`

请求体：`{ target_user_id, room_id, message_id?, reason, note? }`，成功返回 201 `{ id, created_at }`。

校验（失败返回 400 `VALIDATION_FAILED`，列出字段）：
- `target_user_id ≠ 自己`。
- 举报人和被举报人都曾在 `room_id` 有过席位（`room_members` 有记录，不要求现在仍在席），对应成员列表入口和已经退出的人。
- 带 `message_id` 时，该消息属于 `room_id`、发送者是 `target_user_id`，并且举报人能看到它（举报人有这条消息的 delivery）。

其他规则：
- **去重**：同一举报人、同一目标、同一 `message_id`（含都为空）已有 `open` 举报时，直接返回那一条（201，同一个 id），不新建。
- **限额**：每人每天（UTC）20 次，超出返回 429 `RATE_LIMITED`。用数据库计数即可，不用 Redis。
- 举报不自动屏蔽；客户端可以在举报后顺便问用户要不要屏蔽。
- `reason = minor_safety`，或任一方是 `13_17` 时，日志用 warn 级别（不含正文），CLI 列表里置顶。

## 8. CLI：`cmd/tools/modctl`

直连数据库（读 `DATABASE_URL`），供运营者在终端使用。命令：

- `modctl flags [--status open] [--stage ...] [--since 24h]`：列出审核记录，显示正文。正文来自 `mod_flags.text`，没有时联表 messages / deliveries 取。
- `modctl flags ack <id>…`：标记为 reviewed。
- `modctl reports [--status open]`：列出举报，带举报人、被举报人的昵称和年龄段，有消息时带原文与该举报人那份译文。
- `modctl reports resolve|dismiss <id> [--note ...]`。
- `modctl ban <user_id>`：`users.status = 'banned'`，吊销其全部 refresh token。`Auth()` 每次请求都查状态，HTTP 立即失效。另外两处配合（【审查发现，我的决定】）：
  - **WebSocket**：`ws_handler` 的 ping 循环（每 25 秒）顺带查一次账号状态，不是 `active` 就关闭连接。所以已建立的连接最迟约 25 秒断开，不用做跨进程通知。
  - **收消息**：`ActiveRecipients` 只取 `status = 'active'` 的成员，被封的人不再生成 delivery，也不再花翻译费用。
  - **席位不动**：被封者的席位和自己的小屋保持原样。房主可以手动移除；需要时运营者也可以用 SQL 处理。
- `modctl unban <user_id>`：恢复为 `active`。

输出用普通文本表格。CLI 显示正文是有意为之：只在运营者本机终端里出现，不进日志。

## 9. 错误码与契约改动

- 新增并触发：`CONTENT_BLOCKED` 422（发送前 block；onboarding 昵称 block，带 `details.fields`）。B5 只在文档里定义过，代码里的 `errcode.go` 和 `openapi.yaml` 都还没有，要补上。
- 新增：`BLOCK_LIMIT` 409。
- 新 WS 事件：`message.removed`。
- `api/openapi.yaml`：新增 `/blocks`、`/blocks/{user_id}`、`/reports`；把 422 加到 `POST /rooms/{id}/messages` 和 `POST /me/onboarding`；补 `BLOCK_LIMIT`。
- 前端要处理的：发送时遇到 `CONTENT_BLOCKED` 显示“消息未能送达”（已有文案）；onboarding 昵称遇到 `CONTENT_BLOCKED` 显示新文案；收到 `message.removed` 删掉卡片；屏蔽列表页；举报入口（成员列表、消息卡片）。

## 10. 文档同步（按用户约定，提交前审阅）

- 后端 TRD 升到 v1.13：
  - 6.1 表一览：新增 `mod_flags`、`blocks`、`reports` 的实际字段，以及 `deliveries.mod_status`。
  - 7.1：昵称过滤。
  - 7.4：同步段第 1、3 步，worker 的译后审核。
  - 8 章：接口与错误码。
  - 9 章：`message.removed`。
  - 11 章：`tr:*`、`po:*` 的值改为 JSON。
  - 13 章：内容安全一行。
  - 17 章：B6 拆成 B6a / B6b。
- 前端 TRD：错误码文案、`message.removed`、屏蔽 / 举报界面的接口约定，并把它引用的后端 TRD 版本号改为 v1.13（顺带修掉 B5a 留下的版本号落后问题）。
- PRD：8.4 / 13 章补一句 MVP 的实现边界（本地词库 + 模型风险标记 + CLI 复核）；里程碑注明 B6 拆分。
- 两份变更记录，两个仓库保持一致。

## 11. 测试要点

- `pkg/wordfilter`：归一化（全角、大小写、片假名、零宽字符、插入分隔符）；英文词边界（`class` 不命中 `ass`）；最高级别合并；命中词去重与上限。
- `WordlistModerator`：词库与 `risk` 的合并表。
- 发送：block → 422、不入库、不扣配额；review → `mod_status = review` 加 flag；短语跳过审核。
- 翻译：
  - 词库 block：只有这一组隐藏，等待中的润色置 fallback，推 `message.removed` 而不推 `message.translated`。
  - 模型 block：整条消息撤回，同语言的接收者和已完成的其他组也会收到 `message.removed`；之后重试和补偿扫描都跳过这条消息。
  - review：照常显示，加 flag。
- 润色：block → fallback 加 flag；review → done 加 flag。
- 缓存：存和读 risk；旧的纯文本值当未命中；block 不写缓存。
- 屏蔽：群聊双向不投递；私聊静默不投递；历史双向隐藏，解除后恢复；在途的翻译不推送；润色按钮返回 404；幂等；上限；`GET /blocks`。
- 举报：各项校验、去重、每日限额。
- `modctl`：至少 `ban` / `unban` 与 `reports resolve` 有集成测试。
- 回归：B5 全部测试仍通过。

## 12. 留给实现者的事

- 种子词库的具体词条（写完单独给用户审阅）。
- 双自动机还是单自动机加边界判断。
- `modctl` 的参数解析库（沿用 `cmd/tools` 现有写法）。
- 各查询的具体 SQL 和索引细节，只要满足上面的行为。
