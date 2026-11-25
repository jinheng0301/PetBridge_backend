# B6b 设计：打卡与装扮

日期：2026-11-25
状态：草稿，待用户审阅
上游：后端 TRD v1.14（6.1、7.2、7.3、7.5、7.8、8、9 章）、前端 TRD v1.12（第 7、10、11.5 章）、PRD v1.10 F5.1 / F5.2、B6a spec

标注【我的决定】的地方用户没有明确说过，审阅时请重点看。

## 1. 背景与目的

TRD 17 章的 B6 拆成 B6a 合规（已完成）和 B6b 养成（本文）。PRD F5.1：个人打卡产生“小屋能量”，大家共同累计解锁装扮；F5.2：10–15 件基础装扮，由能量里程碑解锁，不可交易。

用户决定：

- 打卡要有**合作感**（2026-11-24）：看得见同屋的人今天有没有打卡，全员打卡有额外能量。
- 全员奖励**按 24 小时窗口**判断（2026-11-25）：每人按自己的日期打卡，不要求大家的“今天”是同一天。
- 用户确认了这几条复述：打卡按小屋算；装扮自动摆在固定位置；单人小屋能打卡但没有全员奖励；退出、被移除、解散都不清能量和装扮；打卡提醒推送留到 B7。

## 2. 现状（B6a 之后）

- 迁移到 `0006`。`checkins`、`room_progress`、`room_decor` 只写在 TRD 6.1 里，还没建。
- `users.tz` 在 onboarding 时写入，目前没有修改入口；`LocalDayBounds(now, tz)` 已有，缺失或无效的时区按 UTC。
- 房间状态由 WS `room.snapshot` 提供，`GET /rooms/{id}` 没有实现。前端 TRD 11.5 写的“按钮状态来自 `GET /rooms/{id}`”要改。
- `model.MemberPresence` 只用于快照和 `member.joined`；`GET /rooms/mine` 用的是 `MemberInfo`。
- 事件分三种：`KindState`（带 seq）、`KindTransient`（房间内、不带 seq）、`KindPersonal`。
- **前端现有 reducer**（`room_reducer.dart`）对不认识的事件直接忽略，**不推进 `last_seq`**。新的带 seq 事件会让它之后每次收到成员事件都判成缺口，等 1 秒再重新订阅。
- 被封禁的账号席位不动（B6a），所以“全员”不能简单等于“所有在席的人”。
- `LoadProfile(lock=true)` 用的是 `FOR NO KEY UPDATE`，不会挡住外键检查的 `FOR KEY SHARE`。
- 前端：宠物和房间背景都是代码绘制，`assets/` 里没有图片；前端 TRD 10.3 仍写装扮用 WebP。背景里已有窗户、置物架加盆栽、木地板、地毯，空座位画成坐垫。

## 3. 目标、非目标、反目标

**目标**
1. `POST /rooms/{id}/checkin`：一人一间小屋一天一次，加能量，可能触发全员奖励和解锁装扮。
2. 快照带上能量、已解锁装扮、每个成员的打卡状态。
3. 两个新 WS 事件：`checkin.updated`、`decor.unlocked`。
4. `GET /rooms/{id}/decor`：完整装扮目录加这间小屋的解锁情况。
5. 目录、数值、五语种文案在本文定死。

**非目标**
- 连续打卡天数、补签、打卡排行。
- 用户自己摆放或收起装扮、装扮交易、付费。
- 打卡提醒推送、Widget 显示（B7）。
- 宠物心情（F5.3，P1）、喂食洗澡（第二版）。
- 数值的后台配置（改数值要发版）。
- 数据库慢时拒绝打卡的降级开关（B8）。
- 前端实现（F4 做）。B6b 只交付后端、契约和文档。

**反目标（做到了也算失败）**
- 同一个人一天在同一间小屋拿到两次打卡能量，或同一批打卡领到两次全员奖励。
- 并发的最后两个人打卡，谁都没触发全员奖励。
- 已解锁的装扮因为改了数值而消失。
- 现有前端因为新事件反复重新订阅。

## 4. 方案比较

用户把“怎么做”交给了我（“跟着你的计划走”），选 A。

| 方案 | 做法 | 取舍 |
| :-- | :-- | :-- |
| **A 事务内累计（选定）** | 打卡时锁房间行，一个事务里写 `checkins`、加 `room_progress.energy_total`、写 `room_decor`，提交后发事件 | 并发好推理，读快照便宜，解锁记录永久保留；能量有一份冗余的总数 |
| B 只存打卡流水 | 只有 `checkins`，能量每次求和，装扮按阈值现算 | 表最少，没有冗余；但改阈值会让已解锁的装扮消失（反目标），流水永远不能清理，奖励能量没处记 |
| C Redis 计数 | 能量放 Redis，异步落库 | 写最快；但本项目 Redis 是可选的，挂了要降级，能量可能丢。打卡量很小，用不着 |

## 5. 规则与数值

数值放代码常量（`internal/service/decor.go`）。【我的决定：不放 `app_config`，内测阶段改动不频繁】

### 5.1 打卡

- 每次打卡 **+10** 能量。
- “一天”按打卡者自己的时区：`local_day` 是他当地的日期，`(room_id, user_id, local_day)` 唯一。
- 在席成员都能打卡，包括一个人的小屋。同一天在两间小屋各打一次，各算各的。
- **打卡状态**用 `checkin_expires_at` 表示：今天已打卡时，是这个人当地的下一个零点；没打卡是 `null`。客户端只要比较“现在 < `checkin_expires_at`”就知道勾该不该亮，过零点不需要服务端推事件。

### 5.2 全员奖励

- 每次成功打卡后判断，满足时小屋 **+20** 能量（固定值，与人数无关）。【我的决定：文案好说，小房间也有盼头】
- “全员”= 在席且 `users.status = 'active'` 的成员，**至少 2 人**。被封禁的人不算，否则奖励永远凑不齐。【我的决定】
- 条件：全员每人都有一条**最近 24 小时内、还没被奖励用掉**的本小屋打卡。满足后，每人最早的那一条标记为已用（`bonus_at`）。
- 为什么要“用掉”：只看 24 小时的话，A 早上打、B 晚上打，第二天 A 早上一打卡，B 昨晚那条还在窗口内，会再发一次，等于一天两次。每次奖励用掉每人一条，所以**全员奖励次数不会超过任何一个人的打卡次数**。
- 只在打卡时判断。有人退出、被移除或被封之后，即使剩下的人都打过了，也不会当场补发，要等下一次打卡。【我的决定】
- 头顶的勾看“当地今天”，奖励看“24 小时内没用过的”，两者偶尔不一致：比如全员的勾都亮着但没有奖励，因为其中一条昨天已经用过了。所以界面**不解释规则**，只在发奖励时提示一句。
- 有一个人长期不来，全员奖励就一直发不出来。MVP 不做“不活跃成员豁免”，房主可以移除他。【我的决定，请确认】

### 5.3 装扮目录（12 件）

`threshold` 是小屋累计能量。能量只增不减。

| # | item_id | 阈值 | 建议位置 |
| :-- | :-- | :-- | :-- |
| 1 | `wall_clock` | 10 | 窗户右侧墙面 |
| 2 | `picture_frame` | 40 | 左上墙面 |
| 3 | `floor_lamp` | 80 | 左后墙角地面 |
| 4 | `string_lights` | 140 | 墙面顶部横向 |
| 5 | `potted_tree` | 220 | 右后墙角地面 |
| 6 | `wall_map` | 320 | 窗户下方墙面 |
| 7 | `woven_basket` | 440 | 前排左下角 |
| 8 | `bookshelf` | 580 | 右侧墙面下部 |
| 9 | `mailbox` | 740 | 后墙根中右 |
| 10 | `yarn_ball` | 920 | 前排右下角 |
| 11 | `record_player` | 1120 | 后墙根左侧 |
| 12 | `wind_chime` | 1340 | 窗帘杆右端 |

节奏：第一次打卡就解锁第一件。集齐全部，2 人天天全员打卡（每天 40）约 34 天，5 人（每天 70）约 20 天，一个人（每天 10）约 134 天。【我的决定】

装扮由**前端代码绘制**，与宠物、背景一致，不用 WebP；剪影由同一套绘制填单色得到。【我的决定：没有美术资源，`assets/` 里一张图都没有】后端只认 `item_id`，具体坐标和画法留给 F4。

解锁规则：每次打卡（含奖励）后，凡是 `threshold ≤ energy_total` 且还没有记录的都写入 `room_decor`。以后加新装扮、或调低阈值，会在下一次打卡时补上；调高阈值不会收回。

### 5.4 成员变化

- 退出、被移除、解散：能量、装扮、打卡记录都不动。解散后房主的小屋仍带着原来的能量和装扮，TRD 7.8“与注册时的初始状态一致”要补上这个例外。
- 打完卡退出、当天又回来：当天不能再打（唯一键），之前那条仍算数。
- 屏蔽不影响打卡和打卡状态的显示（与 B6a 6.3 第 5 条一致）。

## 6. 数据（迁移 0007）

```sql
create table checkins (
    room_id    uuid not null references rooms(id),
    user_id    uuid not null references users(id),
    local_day  date not null,
    energy     int not null check (energy > 0),
    bonus_at   timestamptz,          -- 被哪次全员奖励用掉；null = 还没用
    created_at timestamptz not null, -- 由 service 的 now() 传入
    primary key (room_id, user_id, local_day)
);
create index idx_checkins_room_created on checkins(room_id, created_at);

create table room_progress (
    room_id      uuid primary key references rooms(id),
    energy_total int not null default 0 check (energy_total >= 0),
    updated_at   timestamptz not null
);

create table room_decor (
    room_id     uuid not null references rooms(id),
    item_id     text not null,
    unlocked_at timestamptz not null,
    primary key (room_id, item_id)
);
```

- 没有 `room_progress` 行等于能量 0，不给老房间回填。
- TRD 6.1 里 `room_progress.level` 去掉，等级没有用处。【我的决定】
- `checkins` 不做定期清理，每人每天最多两行。【我的决定】

## 7. 打卡事务

`CheckinService.Checkin(ctx, userID, roomID)`，用 `retryTx`：

```
1. LockRooms(roomID)                 → 没有这间房：ROOM_NOT_FOUND
2. 查调用者在席                       → 不在席：ROOM_NOT_FOUND（不区分）
3. 读调用者 tz，算 local_day 与当地下一个零点
4. INSERT checkins ON CONFLICT DO NOTHING
     → 0 行：今天打过了，读当前进度，按“重复打卡”返回，不发事件
5. 判断全员奖励（5.2）；满足则标记每人最早那条 bonus_at = now
6. room_progress 加 10（或 30），取回 energy_total
7. 写入新达标的 room_decor，取回新解锁的 item_id
8. 提交后发 checkin.updated；每件新装扮发一条 decor.unlocked
```

- 锁房间行让同一间小屋的打卡排队，也和入房、退出、踢人、解散互斥，所以第 5 步看到的成员是稳定的，“最后两个人同时打卡”一定有一个看得到另一个。
- 加锁顺序接在现有顺序后面：`users → invite_codes → rooms → room_members → checkins → room_progress → room_decor`。打卡不锁 `users`；写 `checkins` 时外键对 `users` 取 `FOR KEY SHARE`，与入房的 `FOR NO KEY UPDATE` 不冲突。
- 时间一律用 service 注入的 `now()`，便于测试时区和窗口。窗口是 `created_at > now - 24h`。
- `roomID` 大小写不敏感，格式不对按 `ROOM_NOT_FOUND`。

## 8. 接口

都需登录。未 onboarding 的人不可能在席，自然得到 `ROOM_NOT_FOUND`。

### `POST /rooms/{id}/checkin`（无请求体）→ 200

```json
{
  "already": false,
  "energy_gained": 10,
  "bonus_gained": 20,
  "checkin_expires_at": "2026-11-25T15:00:00Z",
  "energy_total": 140,
  "next_threshold": 220,
  "unlocked": ["string_lights"]
}
```

- 重复打卡返回同样结构：`already: true`，`energy_gained`、`bonus_gained` 为 0，`unlocked` 为 `[]`，其余是当前值。【我的决定：幂等返回 200，不用 409，网络超时重试和连点都不用特殊处理】
- `next_threshold`：下一个还没达到的阈值；全部达到为 `null`。
- 限流：每用户每分钟 10 次，超出 429 `RATE_LIMITED`（沿用 `RateLimitUser`，Redis 不可用时按现有规则放行或用进程内计数）。
- 没有新错误码：`ROOM_NOT_FOUND` 404、`RATE_LIMITED` 429。

### `GET /rooms/{id}/decor` → 200

```json
{
  "energy_total": 140,
  "next_threshold": 220,
  "items": [
    { "item_id": "wall_clock", "threshold": 10, "unlocked_at": "2026-11-20T01:02:03Z" },
    { "item_id": "potted_tree", "threshold": 220, "unlocked_at": null }
  ]
}
```

按目录顺序返回全部 12 件。不在席 → `ROOM_NOT_FOUND`。

### `room.snapshot` 新增字段

- `room.progress`：`{ "energy_total", "next_threshold" }`，没打过卡的房间是 `0` 和第一个阈值。
- `room.decor`：`[{ "item_id", "unlocked_at" }]`，只含已解锁的，没有则 `[]`。
- `room.members[].checkin_expires_at`：时间或 `null`。
- 这些只在快照里，`GET /rooms/mine` 和 `member.joined` 不带。【我的决定】`member.joined` 不带，所以“打完卡退出、当天又回来”的人，别人在下次快照前看不到他的勾；极少见，不处理。
- 快照为此多两三条查询，查询失败按现有规则返回错误。

### WS 事件

| t | 负载 | 发给 |
| :-- | :-- | :-- |
| `checkin.updated` | `room_id`、`user_id`（谁打的卡）、`checkin_expires_at`、`energy_total`、`next_threshold`、`energy_gained`、`bonus_gained` | 全部在席成员，含打卡者（同步其他设备） |
| `decor.unlocked` | `room_id`、`item_id`、`unlocked_at` | 同上 |

两个事件都用 `KindTransient`，**不带 seq**。【我的决定】理由：
- 它们的状态只增不减，乱序、重复、与快照重叠都能正确合并（见第 9 章），不需要 seq 排序。
- 连接断开会重新订阅拿快照，Redis 断线恢复会发 `resync`，不会永久丢状态。消息事件也是这样做的。
- 带 seq 会让现有前端 reducer 反复判缺口（第 2 章）。

## 9. 前端要做的（F4，本次只写进前端 TRD）

- 能量条、打卡按钮在第一份快照到达前不显示。WS 断开时按钮仍可用（走 REST），按响应更新。
- 快照照旧整体替换房间状态；下面的合并规则只用于事件和 REST 响应叠加到当前状态上。
- 合并规则：`energy_total` 取较大值，`next_threshold` 跟着较大的那份走；`checkin_expires_at` 取较晚的；装扮按 `item_id` 取并集；不认识的 `item_id` 忽略。
- 自己打卡的庆祝动画只由 REST 响应触发（`already` 为 false 时）；收到 `user_id` 是自己的 `checkin.updated` 只更新数值。别人打卡：他的宠物头上亮勾，能量条增长。
- `bonus_gained > 0`：显示全员奖励提示（自己打卡时来自响应，别人打卡时来自事件）。
- 新装扮的解锁动画：只由响应的 `unlocked` 与 `decor.unlocked` 触发，那一件此前不在本地集合里才播；快照带来的不播。
- 打卡失败：`ROOM_NOT_FOUND` 走现有“小屋已不存在”处理；`RATE_LIMITED` 和网络错误走通用提示。
- 能量条右侧点开装扮列表页（`GET /rooms/{id}/decor`），未解锁的画剪影。

### 文案

| 用途 | zh-Hans | zh-Hant | ja | ko | en |
| :-- | :-- | :-- | :-- | :-- | :-- |
| 打卡按钮 | 打卡 | 打卡 | チェックイン | 출석하기 | Check in |
| 已打卡 | 今天已打卡 | 今天已打卡 | 今日はチェックイン済み | 오늘 출석 완료 | Checked in today |
| 打卡成功 | 小屋能量 +{n} | 小屋能量 +{n} | おうちエネルギー +{n} | 하우스 에너지 +{n} | House energy +{n} |
| 全员奖励 | 大家都打卡了！小屋能量 +{n} | 大家都打卡了！小屋能量 +{n} | みんなチェックイン完了！おうちエネルギー +{n} | 모두 출석했어요! 하우스 에너지 +{n} | Everyone checked in! House energy +{n} |
| 解锁 | 解锁了新装扮：{name} | 解鎖了新裝扮：{name} | 新しい飾りを手に入れました：{name} | 새 장식을 얻었어요: {name} | New decoration: {name} |
| 装扮页标题 | 小屋装扮 | 小屋裝扮 | おうちの飾り | 하우스 장식 | Decorations |
| 未解锁 | 能量达到 {n} 解锁 | 能量達到 {n} 解鎖 | エネルギー {n} で解放 | 에너지 {n}에 열려요 | Unlocks at {n} energy |
| 全部解锁 | 装扮已全部解锁 | 裝扮已全部解鎖 | すべての飾りを集めました | 모든 장식을 모았어요 | All decorations unlocked |
| 无障碍标签 | {name}今天已打卡 | {name}今天已打卡 | {name}さんは今日チェックイン済み | {name}님 오늘 출석 완료 | {name} checked in today |

装扮名称：

| item_id | zh-Hans | zh-Hant | ja | ko | en |
| :-- | :-- | :-- | :-- | :-- | :-- |
| `wall_clock` | 挂钟 | 掛鐘 | 掛け時計 | 벽시계 | Wall clock |
| `picture_frame` | 相框 | 相框 | フォトフレーム | 액자 | Picture frame |
| `floor_lamp` | 落地灯 | 落地燈 | フロアランプ | 스탠드 조명 | Floor lamp |
| `string_lights` | 串灯 | 串燈 | ストリングライト | 줄조명 | String lights |
| `potted_tree` | 盆栽小树 | 盆栽小樹 | 鉢植えの木 | 화분 나무 | Potted tree |
| `wall_map` | 世界地图 | 世界地圖 | 世界地図 | 세계 지도 | World map |
| `woven_basket` | 藤编篮 | 藤編籃 | 編みかご | 바구니 | Woven basket |
| `bookshelf` | 小书架 | 小書架 | 小さな本棚 | 작은 책장 | Bookshelf |
| `mailbox` | 小邮筒 | 小郵筒 | 小さなポスト | 작은 우체통 | Mailbox |
| `yarn_ball` | 毛线球 | 毛線球 | 毛糸玉 | 털실 뭉치 | Yarn ball |
| `record_player` | 唱片机 | 唱片機 | レコードプレーヤー | 턴테이블 | Record player |
| `wind_chime` | 风铃 | 風鈴 | 風鈴 | 풍경 | Wind chime |

名称和文案只在前端 ARB 里，后端不返回。

## 10. 文档同步

- 后端 TRD v1.15：6.1（三张表的实际字段、去掉 `level`）；7.2 加锁顺序；7.5 重写；7.8 解散的例外；8 章接口；9 章快照字段与两个事件；17 章 B6b 完成；第 18 章末尾的决策列表加一条。
- 前端 TRD v1.13：第 7 章 reducer 规则（两个不带 seq 的事件与合并规则）、`roomLiveProvider` 的说明；10.1 / 10.3 装扮改为代码绘制；11.5 重写（含文案）；4.5 时序图里两个事件的归类；引用的后端 TRD 与 PRD 版本号。顺带改掉 8.2 里过时的“（B6 起才会出现）”。
- PRD v1.11：F5.1 / F5.2 补具体规则（+10、+20、24 小时窗口、12 件、解散不清零）；里程碑注明 B6b。
- 三份变更记录；六个文件两个仓库保持一致。先审阅，不提交。

## 11. 测试要点

- 打卡：首次 +10 并解锁第一件；重复返回 `already`，能量不变、不发事件；不在席、房间不存在、格式错误的 id 都是 404；两间小屋各算各的。
- 时区：东京与洛杉矶同一时刻的 `local_day` 不同；跨当地零点可以再打；夏令时切换日的 `checkin_expires_at`；tz 为空按 UTC。
- 全员奖励：2 人、5 人；1 人不发；窗口刚好 24 小时的边界；“早、晚、早、晚”四次只发两次；被封禁的成员不算；中途入房的人要求也打卡；有人退出后不补发，下次打卡时发。
- 并发：5 个协程同时打卡，恰好一次奖励，能量 = 5×10 + 20；同一人 20 个并发请求只算一次；打卡与踢人、入房并发不死锁。
- 装扮：一次跨多个阈值时全部解锁、每件一个事件；达到最后一个阈值后 `next_threshold` 为 null；目录阈值递增且 `item_id` 不重复。
- 快照：新字段；没打过卡的房间；打过卡的人过了当地零点后为 null。
- 事件：发给全部在席成员含自己，不带 seq；重复打卡不发。
- 解散、退出后能量和装扮还在。
- 契约：`openapi.yaml` 与实现一致。
- 回归：B1–B6a 全部测试。长的 service 测试放后台跑，同一时间只跑一个测试进程。

## 12. 留给实现者的事

- 仓储接口怎么拆（一个 `CheckinStore` 还是分开）、SQL 写法、全员判断用一条还是多条查询。
- 快照的新查询怎么合并。
- handler 放在新的 `checkin_handler.go`（TRD 目录里已有这个名字）。
