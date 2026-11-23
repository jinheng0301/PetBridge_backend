# B6a 内容审核、举报与屏蔽 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- 发送前用本地词库拦截高危内容（422 `CONTENT_BLOCKED`）。
- 译文和润色结果再审核一次：
  - 只有词库命中译文时，只隐藏那一组语言。
  - 模型判原文高危时，整条消息撤回。
- 缓存同时保存 `risk`。
- 昵称过滤。
- 屏蔽：双向不投递，对方无感知。
- 举报落库。
- `modctl` CLI：复核审核记录、处理举报、封禁账号。

**Architecture:**
- 新包 `pkg/wordfilter`：归一化文本，用 Aho-Corasick 匹配。
- `service.WordlistModerator` 取代 `PassModerator`，返回带来源和命中词的 `ModResult`。
- 迁移 `0006`：
  - 新建表：`mod_flags`（人工复核队列）、`blocks`、`reports`。
  - 新增列：`deliveries.mod_status`。
- 屏蔽关系的处理分布在四处：发送时过滤接收者；历史查询时双向隐藏；worker 推送前检查；润色按钮扣配额前检查。
- 整条撤回用一条 SQL 完成，并给每个接收者发 `message.removed`。
- 封号：账号状态检查的范围从 HTTP 扩大到 WS 的 ping 循环和消息收件人查询。

**Tech Stack:** Go、Gin、pgx、go-redis v9、asynq、`golang.org/x/text/unicode/norm`（已是依赖）。无新依赖，Aho-Corasick 自己写。

**Spec:** `docs/superpowers/specs/2026-11-23-b6a-moderation-safety-design.md`。以下各处以它为准：行为细节（第 5–8 章）、反目标（第 3 章）、文档同步清单（第 10 章）。

## Global Constraints

- **不提交、不推送**：
  - 每个任务做完、测试通过后停下，改动留在工作区。
  - 全部完成后交给用户审阅，同意后再按任务分阶段提交：conventional commits，带 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`，推送到 `jinheng main`。
- **分层不变**：
  - 依赖方向：`handler / task → service → repository / platform`。
  - service 不依赖 Gin、asynq、`internal/platform/*`。
  - `pkg/*` 不依赖 `internal/*`。
  - `cmd/tools/modctl` 可以直接用 pgx 写 SQL（与 `cmd/tools/seed` 一样）。
- **测试**：
  - 自动化测试不访问外网。
  - 数据库测试用 `dbtest.Reset(t)`。
  - 集成测试串行执行：`go test -p 1 ./...`。
  - Redis 测试沿用 `TEST_REDIS_URL`，不可用时跳过。
- **日志里永远不出现**：消息正文、译文、润色文、昵称、命中的词条。只记 id、语言、verdict、命中词数量。
- **审核取值**：
  - `messages.mod_status` 和 `deliveries.mod_status` ∈ `ok` / `review` / `block`。
  - `mod_flags.stage` ∈ `outgoing` / `translation` / `polish` / `nickname`。
  - `verdict` ∈ `review` / `block`；`source` ∈ `wordlist` / `model` / `both`。
  - `terms` 最多 10 个。
- **整条撤回的判定**：只看模型自己的结论（`ModResult.Model == ModBlock`，即 `risk = "block"`）。词库判 block、模型判 review 的情况只隐藏这一组。
- **`message.removed`**：
  - `KindTransient`，`To` 只有该接收者。
  - `Data = {"room_id": rid, "message_id": mid}`。
  - 只发给仍在席、且与发送者没有屏蔽关系的接收者。
- **错误码**：
  - `CONTENT_BLOCKED` 422：发送时不带 `details`；昵称被拦时带 `details: {"fields": ["nickname"]}`。
  - `BLOCK_LIMIT` 409。
  - 举报超限时返回 `RATE_LIMITED` 429，带 `Retry-After`，值为到下一个 UTC 零点的秒数。
- **数值常量**：
  - 每人屏蔽上限 500。
  - 举报：每人每 UTC 日 20 次；`note` 去首尾空白后不超过 500 字。
  - `reason` ∈ `harassment` / `spam` / `sexual` / `self_harm` / `minor_safety` / `other`。
- **WS 关闭码**：新增 `model.CloseAccountDisabled = 4403`，被封或删号的连接在 ping 循环里用这个码关闭。

**本计划自己拍板的决定**（都会在 Task 10 同步进文档）：
1. `ModResult` 增加一个字段 `Model ModVerdict`（模型自己的判断）。整条撤回看这个字段，而不是 `Source`，这样“词库 block + 模型 review”就不会被误判为整条撤回。
2. 整条撤回时，所有 delivery 的 `translation_status` 一律置为 `done`，`base_text` 不动。这样补偿扫描自然不会再捡起它们，不用改扫描的 SQL。
3. 举报去重用部分唯一索引 `(reporter_id, target_id, coalesce(message_id, 全零 uuid)) where status = 'open'`，并发重复举报时也只会有一条。
4. 昵称检查前先用 `users.State` 读一次 onboarded：已经 onboarding 过的直接走原来的幂等路径，不检查新昵称。
5. 新 WS 关闭码 4403：前端收到后不重连，回到登录页。
6. `WordlistModerator` 在 `cmd/api` 和 `cmd/worker` 启动时加载词库，加载失败就拒绝启动。

## Review Focus

1. **模型撤回整条消息时，同语言的接收者和已经翻译完的其他语言组也要收到 `message.removed`**，历史里也看不到；之后的重试和扫描不再调用模型。（Task 5 `TestTranslate_ModelBlockWithdrawsWholeMessage`）
2. **屏蔽之前发出、屏蔽之后才翻译完的消息**：不推 `message.translated`；润色按钮返回 404，且不扣配额。（Task 6 `TestTranslate_SkipsPublishToBlocked`、`TestPolishButton_BlockedPairNotFound`）
3. **被隐藏的 delivery 上点润色按钮**：返回 404，不扣配额、不入队。（Task 5 `TestPolishButton_HiddenDeliveryNotFoundNoCharge`）
4. **变体写法**：全角、大小写、片假名、插空格或分隔符、零宽字符都要能命中；英文整词匹配不误伤（`class`、`assistant` 不命中 `ass`）。（Task 1 `TestMatch_*` 表驱动用例）
5. **被封的账号仍坐在小屋里**：不再生成 delivery；已建立的 WS 在下一次 ping 时以 4403 关闭。（Task 8 `TestActiveRecipients_SkipsBanned`、`TestWs_BannedConnectionClosedOnPing`）

---

## File Structure

| 文件 | 职责 |
| :-- | :-- |
| `pkg/wordfilter/wordfilter.go`、`normalize.go`、`wordfilter_test.go`（新） | 归一化、词表解析、Aho-Corasick 匹配 |
| `internal/service/modwords/{ja,ko,zh-Hant,zh-Hans,en}.txt`（新） | 种子词库（embed） |
| `internal/service/moderation_service.go`（改） | `ModResult`、`WordlistModerator`、`LoadModerator`、`recordFlag` |
| `db/migrations/0006_moderation_safety.sql`（新） | `deliveries.mod_status`、`messages.mod_status` 约束、`mod_flags`、`blocks`、`reports` |
| `internal/model/moderation.go`（新） | 审核、屏蔽、举报相关的常量与结构体 |
| `internal/repository/mod_flag_repo.go`、`block_repo.go`、`report_repo.go`（新） | 三张新表的读写 |
| `internal/repository/message_repo.go`（改） | `mod_status` 读写、`WithdrawMessage`、隐藏与屏蔽过滤、只取 active 的收件人 |
| `internal/service/ai_gateway.go`（改） | 缓存值改为 JSON、block 结果不缓存 |
| `internal/service/chat_service.go`、`user_service.go`（改） | 发送前审核、昵称审核、屏蔽过滤、润色按钮守卫 |
| `internal/service/translation_service.go`、`polish_service.go`（改） | 译后审核、撤回、`message.removed`、屏蔽检查 |
| `internal/service/block_service.go`、`report_service.go`（新） | 屏蔽、举报的业务逻辑 |
| `internal/service/events.go`、`errors.go`、`ports.go`（改） | 新事件、新错误、新存储接口 |
| `internal/model/errcode.go`、`event.go`（改） | 新错误码、新事件与关闭码 |
| `internal/handler/block_handler.go`、`report_handler.go`（新）；`chat_handler.go`、`user_handler.go`、`ws_handler.go`（改） | 接口、错误映射、ping 时检查账号状态 |
| `cmd/tools/modctl/main.go`、`modctl.go`、`modctl_test.go`（新） | CLI |
| `cmd/api/main.go`、`cmd/worker/main.go`（改） | 装配 |
| `api/openapi.yaml`（改） | 契约 |
| 两个仓库里的三份主文档和三份变更记录（改） | Task 10 |

---

### Task 1: `pkg/wordfilter`

**Files:**
- Create: `pkg/wordfilter/normalize.go`, `pkg/wordfilter/wordfilter.go`
- Test: `pkg/wordfilter/wordfilter_test.go`

**Interfaces:**
- Produces:
  ```go
  type Level int
  const ( None Level = iota; Review; Block )
  type Entry struct { Term string; Level Level }
  func ParseList(r io.Reader) ([]Entry, error)   // 每行 "block\t词" / "review\t词"；跳过空行和 # 注释；其他 level 报错并带行号
  func Normalize(s string) string                 // NFKC → 小写 → 片假名转平假名 → 去零宽字符（U+200B–U+200D、U+2060、U+FEFF）
  func New(entries []Entry) *Filter               // 同一词条重复出现时取最高级别；归一化后为空的词条忽略
  func (f *Filter) Match(text string) (Level, []string) // 返回最高级别，以及命中的词条（归一化后，按首次出现顺序去重，最多 10 个）
  ```

- [ ] **Step 1：写失败的测试**
  - `TestNormalize`（表驱动）：
    - `"ＫＩＬＬ"` → `"kill"`
    - `"シネ"` → `"しね"`
    - `"a​b"` → `"ab"`
  - `TestMatch_ASCIIWordBoundary`：词条为 `{ass, Block}`。
    - `"you ass!"` 命中，返回 `Block`。
    - `"class"`、`"assistant"`、`"ass1"` 都返回 `None`。
  - `TestMatch_CJKIgnoresSeparators`：词条为 `{死了算了, Block}`。
    - `"死 了 算 了"`、`"死-了_算.了"`、`"死・了、算了"` 都命中。
  - `TestMatch_Variants`：
    - 词条 `{しね, Block}` 能命中 `"シネ"`。
    - 词条 `{kill, Review}` 能命中 `"ＫＩＬＬ you"`。
  - `TestMatch_HighestLevelAndTerms`：同一文本同时命中 review 和 block 时，返回 `Block`，`terms` 两个都有且按出现顺序；命中 12 个不同词条时只返回 10 个。
  - `TestParseList`：
    - 能解析注释行和空行。
    - `"bad\tx"` 报错，错误信息里带 `line 1`。
  - `TestMatch_Fast`：500 字的混合文本，在 200 个词条上匹配 1000 次，总耗时小于 1 秒。

- [ ] **Step 2：确认测试失败**
  - 运行：`go test ./pkg/wordfilter/`
  - 期望：编译失败（包还不存在）。

- [ ] **Step 3：实现**
  - 自己写一个 Aho-Corasick（rune 级的 goto map 加 fail 指针）。
  - 词条分两类：
    - 全是 ASCII 字母或数字的词条：匹配 `Normalize(text)` 的结果，命中后检查两侧词边界（两侧不能是 ASCII 字母或数字）。
    - 其余词条：匹配 `Normalize(text)` 再去掉空白、`.-_*·・、,，。` 之后的文本，不检查边界。
  - 两类各建一个自动机。

- [ ] **Step 4：确认测试通过**
  - 运行：`go test ./pkg/wordfilter/`
  - 期望：PASS。

---

### Task 2: `ModResult`、`WordlistModerator`、种子词库

**Files:**
- Create: `internal/service/modwords/ja.txt`, `ko.txt`, `zh-Hant.txt`, `zh-Hans.txt`, `en.txt`
- Modify: `internal/service/moderation_service.go`，以及三个调用点（`chat_service.go`、`translation_service.go`、`polish_service.go`，这一步只改成读 `.Verdict`，行为不变）；`cmd/api/main.go`、`cmd/worker/main.go`
- Test: `internal/service/moderation_service_test.go`

**Interfaces:**
- Consumes: Task 1 的 `wordfilter`。
- Produces:
  ```go
  const ( ModSourceWordlist = "wordlist"; ModSourceModel = "model"; ModSourceBoth = "both" )
  type ModResult struct {
      Verdict ModVerdict // 最终结论
      Model   ModVerdict // 模型自己的结论（CheckOutgoing 恒为 ModPass）
      Source  string     // Verdict 为 ModPass 时为 ""
      Terms   []string
  }
  type Moderator interface {
      CheckOutgoing(ctx context.Context, text, lang string) (ModResult, error)
      CheckTranslated(ctx context.Context, text, lang, risk string) (ModResult, error)
  }
  func NewWordlistModerator(f *wordfilter.Filter) *WordlistModerator
  func LoadModerator() (*WordlistModerator, error) // 用 go:embed 读 modwords/*.txt，合成一个 Filter
  ```
  `PassModerator` 保留，返回 `ModResult{}`。

- [ ] **Step 1：写失败的测试**
  - `TestCheckTranslated_MergeTable`（表驱动）：测试用的 `Filter` 只有两个词条，`{bad, Block}` 和 `{meh, Review}`。

    | 文本 | risk | Verdict | Model | Source |
    | :-- | :-- | :-- | :-- | :-- |
    | `"ok"` | `none` | Pass | Pass | `""` |
    | `"ok"` | `review` | Review | Review | model |
    | `"ok"` | `block` | Block | Block | model |
    | `"meh"` | `block` | Block | Block | both |
    | `"bad"` | `review` | Block | Review | both |
    | `"bad"` | `none` | Block | Pass | wordlist |
    | `"ok"` | `weird` | Pass | Pass | `""` |

  - `TestCheckOutgoing_TermsReturned`。
  - `TestLoadModerator_EmbeddedListsParse`：`LoadModerator()` 不报错，并且每个语种文件至少有 30 条。

- [ ] **Step 2：确认测试失败**
  - 运行：`go test ./internal/service/ -run 'TestCheckTranslated|TestCheckOutgoing|TestLoadModerator'`
  - 期望：FAIL。

- [ ] **Step 3：起草种子词库**
  - 每个语种 30–60 条，覆盖 spec 5.1 列出的五类。
  - 只有明确的高危才标 `block`，含糊的标 `review`。
  - 文件头写注释说明格式。
  - 这批词条要在最后的审阅里**单独请用户过目**。

- [ ] **Step 4：实现并改调用点**
  - 三个调用点：`verdict` 改为 `res.Verdict`，其他不动（第 4、5 个任务再改行为）。
  - `cmd/api`、`cmd/worker`：用 `LoadModerator()` 替换 `PassModerator{}`，出错时 `log.Error` 并退出。

- [ ] **Step 5：确认测试通过**
  - 运行：`go build ./... && go test -p 1 ./internal/service/ ./pkg/...`
  - 期望：PASS。

---

### Task 3: 迁移 0006 与仓储层

**Files:**
- Create: `db/migrations/0006_moderation_safety.sql`, `internal/model/moderation.go`, `internal/repository/mod_flag_repo.go`, `internal/repository/block_repo.go`, `internal/repository/report_repo.go`
- Modify: `internal/repository/message_repo.go`, `internal/model/message.go`, `internal/service/ports.go`
- Test: `internal/platform/db/migrate_test.go`, `internal/repository/moderation_repo_test.go`, `internal/repository/message_repo_test.go`

**Interfaces:**
- Produces（`internal/model/moderation.go`）：
  ```go
  const ( ModOK = "ok"; ModReview = "review"; ModBlock = "block" )
  const ( StageOutgoing = "outgoing"; StageTranslation = "translation"; StagePolish = "polish"; StageNickname = "nickname" )
  type ModFlag struct {
      Stage, Verdict, Source string
      Terms  []string
      UserID string
      RoomID, MessageID, DeliveryID, TargetLang, Text *string
  }
  type BlockedUser struct {
      UserID    string    `json:"user_id"`
      Nickname  *string   `json:"nickname"`
      PetType   *string   `json:"pet_type"`
      BlockedAt time.Time `json:"blocked_at"`
  }
  var ReportReasons = []string{"harassment", "spam", "sexual", "self_harm", "minor_safety", "other"}
  type NewReport struct {
      ReporterID, TargetID, RoomID string
      MessageID                    *string
      Reason                       string
      Note                         *string
  }
  ```
- `model.NewMessage` 增加 `ModStatus string`（空字符串按 `ok` 写入）。`model.PolishWork` 增加 `ModStatus string`（delivery 的审核结论）。
- Produces（`service/ports.go`）：
  ```go
  type ModFlagStore interface { Insert(ctx context.Context, f model.ModFlag) error }
  type BlockStore interface {
      Block(ctx context.Context, blockerID, blockedID string) error   // 已存在时什么也不做
      Unblock(ctx context.Context, blockerID, blockedID string) error
      IsBlocking(ctx context.Context, blockerID, blockedID string) (bool, error)
      Count(ctx context.Context, blockerID string) (int, error)
      List(ctx context.Context, blockerID string) ([]model.BlockedUser, error) // 按 blocked_at 倒序
      Between(ctx context.Context, a, b string) (bool, error)                 // 任一方向
      BlockedAmong(ctx context.Context, userID string, others []string) (map[string]bool, error) // 任一方向
  }
  type ReportStore interface {
      // Insert 在已有一条相同的 open 举报时，返回那一条的 id 和时间。
      Insert(ctx context.Context, r model.NewReport) (id string, createdAt time.Time, err error)
      // FindOpen 返回同一 (reporter, target, message_id) 的 open 举报；没有则返回 model.ErrNotFound。
      FindOpen(ctx context.Context, reporterID, targetID string, messageID *string) (id string, createdAt time.Time, err error)
      CountSince(ctx context.Context, reporterID string, since time.Time) (int, error)
      // MessageReportable：消息属于 roomID、发送者是 senderID，且 reporterID 有这条消息的 delivery。
      MessageReportable(ctx context.Context, messageID, roomID, senderID, reporterID string) (bool, error)
  }
  ```
- `MessageStore` 的变更：
  ```go
  SetTranslation(ctx, deliveryID string, baseText *string, status, modStatus string, now time.Time) (changed bool, err error)
  // WithdrawMessage：一条语句完成 messages.mod_status='block'；该消息所有 delivery 置 mod_status='block'，
  // 未完成的 translation_status 置 'done'，pending 的润色置 'fallback'；返回全部 receiver_id。
  WithdrawMessage(ctx context.Context, messageID string, now time.Time) ([]string, error)
  ```
  - `InsertMessage` 写入 `mod_status`。
  - `PolishWork` 读出 `d.mod_status`。
  - `ViewsOf` 跳过 `mod_status = 'block'` 的 delivery。
  - `ListForViewer` 的 left join 加条件 `and d.mod_status <> 'block'`（屏蔽过滤留给 Task 6）。

- [ ] **Step 1：写迁移（带 Down）**
  - `deliveries.mod_status`：`text not null default 'ok'`，check 约束为 `ok/review/block`。
  - `messages` 增加 check 约束 `mod_status in ('ok','review','block')`。
  - `mod_flags`：
    - 字段同 spec 5.3。
    - `terms text[] not null default '{}'`。
    - 外键只给 `user_id`（引用 `users`），其他 id 不加外键（消息可能被清理）。
    - 索引 `(status, created_at)`。
  - `blocks`：同 spec 6.1。
  - `reports`：
    - 字段同 spec 7.1，`reason` 和 `status` 加 check 约束。
    - 索引 `(status, created_at)`、`(reporter_id, created_at)`。
    - 部分唯一索引 `uq_reports_open`：`(reporter_id, target_id, coalesce(message_id, '00000000-0000-0000-0000-000000000000'::uuid)) where status = 'open'`。

- [ ] **Step 2：写失败的测试**
  - `TestMigrateUp_CreatesModerationTables`：三张表和 `deliveries.mod_status` 都存在；现有的 `TestMigrateDownThenUp_RoundTrip` 仍然通过。
  - `TestSetTranslation_WritesModStatus`。
  - `TestWithdrawMessage`：两个接收者，一个已 done、一个 pending 并带着 pending 的润色。
    - 两行的 `mod_status` 都是 `block`。
    - pending 的那行变成 `done`，润色变成 `fallback`。
    - `messages.mod_status = 'block'`。
    - 返回两个接收者 id。
    - 之后 `StuckTranslations` 不再返回这条消息。
  - `TestViewsOf_SkipsBlockedDelivery`。
  - `TestListForViewer_HidesBlockedDelivery`：被隐藏的接收者看不到这条消息，发送者照常能看到。
  - `TestPolishWork_ReadsModStatus`。
  - `TestBlockRepo`：
    - 重复 `Block` 不报错。
    - `Between` 和 `BlockedAmong` 两个方向都能查到。
    - `List` 按时间倒序，带昵称。
    - `Unblock` 后查不到。
  - `TestReportRepo_InsertDedupesOpen`：
    - 同样的 (reporter, target, 无 message) 插两次，返回同一个 id。
    - 第一条改成 `resolved` 后再插，得到新 id。
    - 带 message 和不带 message 的举报互不影响。
  - `TestReportRepo_MessageReportable`：发送者不对、没有 delivery、房间不对，三种情况都返回 false。
  - `TestModFlagRepo_Insert`：`terms` 和可空字段能原样读回。

- [ ] **Step 3：确认测试失败**
  - 运行：`go test -p 1 ./internal/repository/ ./internal/platform/db/`
  - 期望：FAIL。

- [ ] **Step 4：实现三个新 repo 并修改 `message_repo`**
  - `ReportRepo.Insert`：先 `insert … on conflict do nothing returning id, created_at`；没插进去时，再按同样的键查出那条 open 的举报返回。
  - 修好所有因 `SetTranslation` 签名变化而编译失败的调用点：
    - 调用方暂时都传 `model.ModOK`。
    - 测试里的假 store 也一并改。

- [ ] **Step 5：确认测试通过**
  - 运行：`go build ./... && go test -p 1 ./internal/repository/ ./internal/platform/db/ ./internal/service/`
  - 期望：PASS。

---

### Task 4: 缓存保存 risk

**Files:**
- Modify: `internal/service/ai_gateway.go`
- Test: `internal/service/ai_gateway_test.go`

**Interfaces:**
- `AICache` 接口不变，值的格式改为 JSON `{"t": text, "r": risk}`。
- `AIOutcome` 的注释改为：命中缓存时返回存下的 `risk`。

- [ ] **Step 1：写失败的测试**
  - `TestGateway_CacheKeepsRisk`：供应商返回 `risk = review`；第二次调用命中缓存，`Risk == "review"`，`Cached == true`，供应商只被调用一次。
  - `TestGateway_LegacyCacheValueIsMiss`：预先写入纯文本值，调用时仍然请求供应商，并用 JSON 覆盖原值。
  - `TestGateway_BlockNotCached`：供应商返回 `risk = block`，再调用一次仍然请求供应商。
  - 润色走同一个 `run`，补一个 `TestGateway_PolishCacheKeepsRisk`。

- [ ] **Step 2：确认测试失败**
  - 运行：`go test ./internal/service/ -run TestGateway_`
  - 期望：新增的测试 FAIL。

- [ ] **Step 3：实现**
  - 读缓存：JSON 解析失败，或 `t` 为空，都当作未命中。
  - 写缓存：`res.Risk == llm.RiskBlock` 时不写。

- [ ] **Step 4：确认测试通过**
  - 运行：`go test ./internal/service/ -run 'TestGateway_|TestTranslate|TestPolish'`
  - 期望：PASS。

---

### Task 5: 发送前、译后、润色后审核，`message.removed`，昵称

**Files:**
- Modify: `internal/service/chat_service.go`, `translation_service.go`, `polish_service.go`, `user_service.go`, `moderation_service.go`, `events.go`, `errors.go`；`internal/model/errcode.go`, `event.go`；`internal/handler/chat_handler.go`, `user_handler.go`
- Test: `internal/service/moderation_flow_test.go`（新）、`user_service_test.go`、`internal/handler/chat_handler_test.go`、`user_handler_test.go`

**Interfaces:**
- Consumes: Task 2 的 `ModResult`；Task 3 的 `ModFlagStore`、`WithdrawMessage`、`SetTranslation(…, modStatus, …)`、`PolishWork.ModStatus`。
- Produces:
  ```go
  // errors.go
  type ContentBlockedError struct{ Fields []string } // Error() = "content blocked"
  // model/errcode.go
  CodeContentBlocked = "CONTENT_BLOCKED"
  // events.go / model/event.go
  EventMessageRemoved = "message.removed";  FrameMessageRemoved = "message.removed"
  func messageRemovedEvent(roomID, messageID, to string) Event
  // moderation_service.go：写入时使用 context.WithoutCancel；失败只打 warn
  func recordFlag(ctx context.Context, flags ModFlagStore, log *slog.Logger, f model.ModFlag)
  // 以下 With* 传 nil 时，表示不写 mod_flags（给现有测试用）
  func (s *ChatService) WithModFlags(f ModFlagStore) *ChatService
  func (s *TranslationService) WithModFlags(f ModFlagStore) *TranslationService
  func (s *PolishService) WithModFlags(f ModFlagStore) *PolishService
  func (s *UserService) WithModeration(m Moderator, f ModFlagStore) *UserService // m 为 nil 时不检查昵称
  ```

- [ ] **Step 1：写失败的测试**

  测试统一用 `NewWordlistModerator(wordfilter.New({bad, Block}, {meh, Review}))`。翻译用的假 translator 扩展为 `risk map[string]string`（按目标语言给）和 `text func(lang) string`。

  **发送前**
  - `TestSend_BlockedWordRefused`：
    - 返回 `*ContentBlockedError`，且 `Fields` 为空。
    - `messages` 表里没有新行；润色配额没扣（发送时开着润色开关）。
    - `mod_flags` 有一条：`stage = outgoing`、`verdict = block`，`text` 是原文，`terms` 为 `{bad}`。
  - `TestSend_BlockedReplayStillBlocked`：同一个 `client_msg_id` 再发一次，仍然返回 `ContentBlockedError`。
  - `TestSend_ReviewWordStoredAndFlagged`：
    - `messages.mod_status = 'review'`，接收者照常收到 `message.new`。
    - flag 上有 `message_id`，`text` 为空。
  - `TestSend_PhraseSkipsModeration`：用一个所有内容都判 block 的 stub moderator，短语消息照样发送成功。

  **译后**
  - `TestTranslate_WordlistBlockHidesGroupOnly`：
    - 房间里有 ja、ko 两位接收者；ja 那份译文含 `bad`，`risk = none`；ja 的 delivery 带着 pending 的润色。
    - ja 那行：`mod_status = block`，状态为 done，润色变成 fallback；ja 收到 `message.removed`，没有收到 `message.translated`。
    - ko 照常收到 `message.translated`。
    - 每行一条 flag，`source = wordlist`。
  - `TestTranslate_ModelBlockWithdrawsWholeMessage`：
    - 房间里有 ja、ko，还有一位和发送者同语言的接收者；ko 的 `risk = block`，且 ja 组先处理。
    - 三人都收到 `message.removed`；`messages.mod_status = block`。
    - 只有一条 flag：`message_id` 有值，`delivery_id` 为空。
    - 再跑一次 `Process`，翻译器的调用次数不变。
  - `TestTranslate_ReviewFlagsAndShows`。

  **润色后**
  - `TestPolish_BlockFallsBackAndFlags`：
    - 状态为 fallback，`polished_text` 为空，接收者收到 `message.polished`（`fallback`）。
    - flag 的 `stage = polish`，`text` 是被拒的润色结果。
  - `TestPolish_ReviewDoneAndFlags`。
  - `TestPolish_HiddenDeliverySkipsModel`：delivery 的 `mod_status = block` 且润色 pending，结果置 fallback，不调用润色模型，也不发布任何事件。
  - `TestPolishButton_HiddenDeliveryNotFoundNoCharge`：返回 `ErrDeliveryNotFound`，配额计数不变，`polish_status` 仍是 `none`。

  **昵称**
  - `TestOnboard_NicknameBlocked`：
    - 返回 `*ContentBlockedError{Fields: ["nickname"]}`，用户仍未 onboarding。
    - flag 的 `stage = nickname`，`text` 是昵称，并且这条 flag 在返回错误后仍然存在。
  - `TestOnboard_NicknameReviewAllowed`。
  - `TestOnboard_RepeatIgnoresNickname`：已经 onboarding 的用户再提交一个含 `bad` 的昵称，返回已存结果，不写 flag。

  **handler**
  - `TestSend_ContentBlocked422`：响应 body 为 `{"error":{"code":"CONTENT_BLOCKED",…}}`，没有 `details`。
  - `TestOnboarding_ContentBlocked422`：`details.fields == ["nickname"]`。

- [ ] **Step 2：确认测试失败**
  - 运行：`go test -p 1 ./internal/service/ ./internal/handler/`
  - 期望：FAIL。

- [ ] **Step 3：实现发送前审核和昵称审核**
  - 发送前：位置不变（重放查找之后、`senderPolish` 之前）。
    - 短语消息跳过。
    - Block：写 flag，返回 `&ContentBlockedError{}`。
    - Review：`m.ModStatus = model.ModReview`，插入成功后写 flag。
  - 昵称：在 `Onboard` 进入事务之前做。
    1. 用 `users.State` 读 onboarded，已 onboarding 就跳过检查。
    2. 跑 `ValidateOnboarding`，出错就返回。
    3. 对 trim 过的昵称调用 `CheckOutgoing`。

- [ ] **Step 4：实现译后审核**

  `translate` 改为返回 `(text, status, modStatus string, res ModResult, err error)`。`Process` 里每个语言组按下面的顺序判断：
  1. `res.Model == ModBlock`：
     - 调用 `WithdrawMessage`。
     - 给返回的每个接收者发 `message.removed`，但只发给仍在席的。
     - 写一条整条消息的 flag。
     - 返回 nil，本次运行里已经变更的接收者**不再**发 `message.translated`。
  2. `res.Verdict == ModBlock`：
     - 本组每行调用 `SetTranslation(…, done, "block")`。
     - 有 pending 润色的行，再调用 `SetPolish(nil, fallback)`。
     - 每行写一条 flag。
     - 把这些接收者放进 `removed` 列表，最后发 `message.removed` 而不是 `message.translated`。
  3. `res.Verdict == ModReview`：本组每行 `modStatus = "review"`，每行写一条 flag，其余照常。

- [ ] **Step 5：实现润色守卫和润色后审核**
  - 润色按钮：在 seated 检查之后、看 `PolishStatus` 之前，`w.ModStatus == model.ModBlock` 就返回 `ErrDeliveryNotFound`。
  - `PolishService.Process` 开头：`w.ModStatus == model.ModBlock` 时，调用 `SetPolish(nil, fallback)` 后直接返回，不发布任何事件。
  - 润色后审核：
    - Block：结果置 fallback，写 flag（带润色文本）。
    - Review：结果置 done，写 flag（带润色文本）。
  - handler：`failChat` 和 `UserHandler.fail` 增加 `ContentBlockedError` → 422；`Fields` 非空时才带 `details`。

- [ ] **Step 6：确认测试通过**
  - 运行：`go test -p 1 ./internal/service/ ./internal/handler/`
  - 期望：PASS，B5 的测试全部仍然通过。

---

### Task 6: 屏蔽

**Files:**
- Create: `internal/service/block_service.go`, `internal/handler/block_handler.go`
- Modify: `internal/service/chat_service.go`, `translation_service.go`, `polish_service.go`, `errors.go`；`internal/repository/message_repo.go`（`ListForViewer`）；`internal/model/errcode.go`
- Test: `internal/service/block_service_test.go`, `internal/handler/block_handler_test.go`

**Interfaces:**
- Consumes: Task 3 的 `BlockStore`。
- Produces:
  ```go
  const blockLimit = 500
  var ErrBlockLimit = errors.New("block limit reached") // model.CodeBlockLimit = "BLOCK_LIMIT"，409
  func NewBlockService(users UserStore, blocks BlockStore) *BlockService
  func (s *BlockService) Block(ctx context.Context, userID, targetID string) error
  func (s *BlockService) Unblock(ctx context.Context, userID, targetID string) error
  func (s *BlockService) List(ctx context.Context, userID string) ([]model.BlockedUser, error)
  func (s *ChatService) WithBlocks(b BlockStore) *ChatService            // nil 表示不过滤
  func (s *TranslationService) WithBlocks(b BlockStore) *TranslationService
  func (s *PolishService) WithBlocks(b BlockStore) *PolishService
  func NewBlockHandler(svc *service.BlockService, authn gin.HandlerFunc) *BlockHandler // POST /blocks、DELETE /blocks/:user_id、GET /blocks
  ```
  - `BlockService` 的三个方法都先用 `users.State` 检查调用者：未 onboarding 返回 `ErrOnboardingRequired`。
  - `targetID` 先转小写，以下情况返回 `&ValidationError{Fields: ["user_id"]}`：不是 uuid、是自己、用户不存在、对方未 onboarding。
  - 已经屏蔽过的直接返回 nil，不计入上限。
  - 未屏蔽且 `Count >= 500` 时返回 `ErrBlockLimit`。
  - `ListForViewer` 增加条件：
    ```sql
    and not exists (select 1 from blocks b where (b.blocker_id = $2 and b.blocked_id = m.sender_id) or (b.blocker_id = m.sender_id and b.blocked_id = $2))
    ```

- [ ] **Step 1：写失败的测试**
  - `TestBlock_Validation`：自己、乱写的 id、不存在的用户、未 onboarding 的用户，都返回 `user_id` 字段错误；大写的 uuid 能正常处理。
  - `TestBlock_IdempotentAndLimit`：预先插入 500 条，再屏蔽新的人返回 `ErrBlockLimit`；再屏蔽已有的人返回 nil。
  - `TestSend_GroupSkipsBlockedEitherWay`：A 屏蔽了 B。
    - B 发的群聊不给 A 生成 delivery，A 也收不到 `message.new`。
    - A 发的群聊也不给 B 生成 delivery。
    - C 两边都照常收到。
  - `TestSend_DMToBlockedPeerSilent`：B 私聊 A，返回 202 视图，消息入库，但 deliveries 为 0 行。
  - `TestList_HidesBlockedBothWaysAndRestoresOnUnblock`：
    - 屏蔽之前 B 发给 A 的消息，屏蔽后 A 拉历史看不到，B 也看不到 A 屏蔽前发的消息。
    - 解除屏蔽后，两边都恢复。
    - 屏蔽期间发的消息，解除后也看不到。
  - `TestTranslate_SkipsPublishToBlocked`：先发消息，再屏蔽，然后跑 `Process`。A 那行照常写成 done，但 A 收不到 `message.translated`。
  - `TestPolishButton_BlockedPairNotFound`：返回 `ErrDeliveryNotFound`，不扣配额。
  - `TestPolish_BlockedPairFallsBackWithoutModel`。
  - handler：
    - `TestBlockRoutes`：三个接口的成功路径；`GET` 返回 `{"items":[…]}`。
    - 超过上限返回 409 `BLOCK_LIMIT`。
    - 未 onboarding 返回 403。

- [ ] **Step 2：确认测试失败**
  - 运行：`go test -p 1 ./internal/service/ ./internal/handler/ ./internal/repository/`
  - 期望：FAIL。

- [ ] **Step 3：实现**
  - `Send`：算出 receivers 之后调用一次 `BlockedAmong(userID, ids)`，把命中的人过滤掉。
    - 私聊对象被过滤掉以后，`receivers` 为空，`toUser` 照常保留。
    - 确认 receivers 为空时，流程不会入队，也不会出错。
  - `TranslationService.publish`、`PolishService.publish`：发布前调用 `Between(senderID, receiverID)`，有屏蔽关系就跳过。
  - 润色按钮：在 Task 5 的 `mod_status` 守卫之后，调用 `Between(w.SenderID, userID)`，有屏蔽关系就返回 `ErrDeliveryNotFound`。
  - `PolishService.Process`：有屏蔽关系就置 fallback，不调用模型，也不发布事件。

- [ ] **Step 4：确认测试通过**
  - 运行：`go test -p 1 ./internal/...`
  - 期望：PASS。

---

### Task 7: 举报

**Files:**
- Create: `internal/service/report_service.go`, `internal/handler/report_handler.go`
- Test: `internal/service/report_service_test.go`, `internal/handler/report_handler_test.go`

**Interfaces:**
- Consumes: Task 3 的 `ReportStore`、`model.NewReport`、`model.ReportReasons`；`RoomMemberStore.EverMember`；`UserStore.State` 和 `UserStore.LoadProfile`。
- Produces:
  ```go
  type ReportInput struct { TargetUserID, RoomID, MessageID, Reason, Note string } // MessageID、Note 可为空字符串
  type ReportResult struct { ID string `json:"id"`; CreatedAt time.Time `json:"created_at"` }
  func NewReportService(users UserStore, members RoomMemberStore, reports ReportStore, now func() time.Time, log *slog.Logger) *ReportService
  func (s *ReportService) Report(ctx context.Context, userID string, in ReportInput) (ReportResult, error)
  func NewReportHandler(svc *service.ReportService, authn gin.HandlerFunc) *ReportHandler // POST /reports → 201
  ```
- 处理顺序：
  1. 调用者未 onboarding：返回 `ErrOnboardingRequired`。
  2. 字段校验，`ValidationError` 一次列出所有出错的字段：
     - `target_user_id`：必须是 uuid，且不是自己。
     - `room_id`：必须是 uuid。
     - `reason`：必须在 `ReportReasons` 里。
     - `note`：去首尾空白后不超过 500 字，空字符串存成 NULL。
     - `message_id`：给了就必须是 uuid。
  3. 举报人和被举报人都要 `EverMember(room)`，否则 `ValidationError{"room_id"}`。
  4. 带了 `message_id` 时，`MessageReportable` 必须为真，否则 `ValidationError{"message_id"}`。
  5. 先查 `FindOpen`：已有相同的 open 举报就直接返回它，不检查限额。
  6. 每日限额：`CountSince(UTC 今日零点) >= 20` 时返回 `&RateLimitedError{RetryAfter: 到下一个 UTC 零点}`。
  7. `Insert`：并发重复提交时，它也会返回已有的那一条。
  8. `reason = minor_safety`，或者任一方的 `age_band` 是 `13_17` 时，打 `log.Warn("report needs priority review", "report_id", …, "reason", …)`。

- [ ] **Step 1：写失败的测试**
  - `TestReport_Validation`（表驱动）：每个字段出错的情况各一个用例。
  - `TestReport_MustShareRoomEver`：对方已经退出房间，仍然可以举报；对方从没进过这个房间，返回 `room_id` 字段错误。
  - `TestReport_MessageChecks`：消息的发送者不是被举报人、举报人没有这条消息的 delivery，都返回 `message_id` 字段错误。
  - `TestReport_DedupesOpen`：返回同一个 id。
  - `TestReport_DailyLimit`：第 21 次返回 `RateLimitedError`，`RetryAfter` 等于到下一个 UTC 零点的时长（固定 `now` 来断言）；对已有的 open 举报重复提交，不受限额影响。
  - handler `TestReportRoute`：
    - 成功返回 201 `{id, created_at}`。
    - 字段错误返回 400 `VALIDATION_FAILED`。
    - 超限返回 429，带 `Retry-After`。

- [ ] **Step 2：确认测试失败**
  - 运行：`go test -p 1 ./internal/service/ -run TestReport && go test ./internal/handler/ -run TestReportRoute`
  - 期望：FAIL。

- [ ] **Step 3：实现**

- [ ] **Step 4：确认测试通过**
  - 运行：同 Step 2。
  - 期望：PASS。

---

### Task 8: 封号的效果与 `modctl`

**Files:**
- Create: `cmd/tools/modctl/main.go`, `cmd/tools/modctl/modctl.go`
- Modify: `internal/repository/message_repo.go`（`ActiveRecipients`）、`internal/handler/ws_handler.go`、`internal/model/event.go`
- Test: `cmd/tools/modctl/modctl_test.go`, `internal/repository/message_repo_test.go`, `internal/handler/ws_handler_test.go`

**Interfaces:**
- Produces:
  ```go
  // model/event.go
  CloseAccountDisabled = 4403
  // modctl.go：main 只负责读配置、开连接池，然后调用 Run
  func Run(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, now func() time.Time) error
  ```
- `modctl` 子命令（参数用标准库 `flag`，每个子命令一个 `FlagSet`）：

  | 子命令 | 行为 |
  | :-- | :-- |
  | `flags [-status open] [-stage S] [-since 24h]` | 列出审核记录 |
  | `flags ack <id>...` | 标记为 reviewed |
  | `reports [-status open]` | 列出举报 |
  | `reports resolve <id> [-note N]`、`reports dismiss <id> [-note N]` | 处理举报 |
  | `ban <user_id>` | 封号 |
  | `unban <user_id>` | 解封 |

  - `flags` 列表：正文优先取 `mod_flags.text`，没有时联表取 `messages.body` 或 `deliveries.base_text`。
  - `reports` 列表：
    - `minor_safety`，或任一方是 `13_17` 的举报排在最前，其余按时间倒序。
    - 显示双方昵称和年龄段。
    - 有消息时，显示原文和该举报人那份 `base_text`。
  - `ban`：在一个事务里完成：
    - `users.status = 'banned'`，只改 `active` 的账号；deleted 的账号报错。
    - 该用户所有未吊销的 refresh token 置 `revoked_at = now`。
  - `unban`：只能把 `banned` 改回 `active`。
  - 未知子命令和参数错误：返回 error，内容带用法说明。
- `ActiveRecipients` 增加条件 `and u.status = 'active'`。
- `WsHandler.pingLoop`：ping 成功并刷新 presence 之后，调用 `h.users.State(ctx, conn.user)`。读取成功且 `status != "active"` 时，`conn.Close(model.CloseAccountDisabled, "account disabled")` 并返回；读取出错时只打 warn，不断开。

- [ ] **Step 1：写失败的测试**
  - `TestActiveRecipients_SkipsBanned`。
  - `TestWs_BannedConnectionClosedOnPing`：
    - 测试配置把 `Heartbeat` 设为 50ms。
    - 连接建立后把用户改成 `banned`，客户端在 1 秒内收到关闭码 4403。
  - `TestModctl_BanRevokesAndUnban`：
    - ban 之后：状态是 `banned`，refresh token 全部吊销。
    - unban 之后：状态回到 `active`。
    - 对 deleted 的账号执行 ban 返回 error。
  - `TestModctl_FlagsListAndAck`：
    - 输出里有 `mod_flags.text` 的正文，也有从 `messages.body` 联表取到的正文。
    - ack 之后，默认的 open 列表里不再有这一条。
  - `TestModctl_ReportsOrderAndResolve`：`minor_safety` 的举报排在第一行；resolve 之后 `status`、`handled_at`、`handled_note` 都写上了。
  - `TestModctl_UnknownCommand`。

- [ ] **Step 2：确认测试失败**
  - 运行：`go test -p 1 ./cmd/tools/modctl/ ./internal/repository/ ./internal/handler/ -run 'Modctl|SkipsBanned|BannedConnection'`
  - 期望：FAIL。

- [ ] **Step 3：实现**

- [ ] **Step 4：确认测试通过**
  - 运行：同 Step 2。
  - 期望：PASS。

---

### Task 9: 装配与契约

**Files:**
- Modify: `cmd/api/main.go`, `cmd/worker/main.go`, `api/openapi.yaml`, `Taskfile.yml`（如果现有工具在这里有任务，就给 modctl 也加一个）
- Test: `go build ./...`、现有的 openapi 校验（如果有）

- [ ] **Step 1：装配 api**
  - 新建 `ModFlagRepo`、`BlockRepo`、`ReportRepo`。
  - `ChatService`、`TranslationService`（inline 用的那个）都调用 `.WithModFlags(flags).WithBlocks(blocks)`。
  - `UserService` 调用 `.WithModeration(mod, flags)`。
  - 注册 `BlockHandler`、`ReportHandler`，用户级限流沿用聊天路由的写法。

- [ ] **Step 2：装配 worker**
  - `TranslationService`、`PolishService` 调用 `.WithModFlags(flags).WithBlocks(blocks)`。

- [ ] **Step 3：改 openapi**
  - 新增 `/api/v1/blocks`（GET、POST）、`/api/v1/blocks/{user_id}`（DELETE）、`/api/v1/reports`（POST），以及对应的 schema。
  - `POST /rooms/{id}/messages` 和 `POST /me/onboarding` 增加 422 响应。
  - 错误码枚举里加上 `CONTENT_BLOCKED`、`BLOCK_LIMIT`。
  - 按 TRD 8 章的规则：可空的枚举要把 `null` 写进 `enum`。

- [ ] **Step 4：验证**
  - 运行：`go build ./... && go vet ./... && go test -p 1 ./...`
  - 期望：全部 PASS。
  - 手动冒烟测试（本地 api、worker 加 seed）：
    1. 发一条含种子词库里 block 词的消息，得到 422。
    2. 屏蔽后对方的消息不再出现。
    3. 提交一条举报，`modctl reports` 能看到。
    4. `modctl ban` 之后，WS 在约 25 秒内断开。

---

### Task 10: 文档同步

**Files:** 两个仓库根目录下的六份文件：
- 三份主文档：`产品需求文档（PRD）：跨国 MBTI 情感 AI 电子宠物.md`、`后端技术设计文档（TRD）：PetBridge.md`、`前端技术设计文档（TRD）：PetBridge（Flutter）.md`
- 三份变更记录：同名的 `…变更记录.md`

- [ ] **Step 1：后端 TRD 升到 v1.13**，按 spec 第 10 章逐项修改：
  - 6.1 表一览。
  - 7.1 昵称过滤。
  - 7.4 同步段第 1、3 步，以及 worker 的译后审核。
  - 第 8 章接口和错误码。
  - 第 9 章 `message.removed` 和关闭码 4403。
  - 第 11 章缓存的值改为 JSON。
  - 第 13 章内容安全一行。
  - 第 17 章 B6 拆成 B6a / B6b。
  - 正文里的标注写成“（v1.13）”，并在版本列表的最上面加一行。

- [ ] **Step 2：前端 TRD 升版**
  - 补充：`CONTENT_BLOCKED` 的两种文案、`message.removed` 的处理、关闭码 4403 不重连、屏蔽和举报的接口约定、举报原因枚举。
  - 把它引用的后端 TRD 版本号改为 v1.13。

- [ ] **Step 3：PRD**
  - 8.4、13 章补一句 MVP 的实现边界。
  - 14.2 里程碑注明 B6 拆分。
  - 版本记录表加一行。

- [ ] **Step 4：写变更记录**
  - 三份变更记录的最上面各加一节 `## V1.x 变更说明`。
  - 内容包括本计划“自己拍板的决定”第 1–6 条，以及用户在设计阶段的四项决定：熟人内测、拆分、方案 A、屏蔽方案 2 和按来源区分撤回。

- [ ] **Step 5：复制到前端仓库并校验**
  - 把六份文件复制到 `../PetBridge_frontend/`。
  - 运行：`md5sum` 对比两边的六个文件。
  - 期望：两两一致。

- [ ] **Step 6：交给用户审阅**
  - 列出：改动的文件、建议的分阶段提交方式（每个任务一个 commit，文档单独一个）、种子词库（请用户单独过目）。
  - 在用户同意之前不提交。
