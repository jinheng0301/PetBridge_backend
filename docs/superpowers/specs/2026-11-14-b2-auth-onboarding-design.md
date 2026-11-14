# B2 设计：登录、令牌、onboarding、宠物映射

日期：2026-11-14　开发方式：直接在 `main` 上开发（不开 PR、不提交，由用户审查后提交）
依据：后端 TRD v1.2 的 7.1、8、13、15.6 节；PRD v1.4 的 4.3、第 7 章；前端 TRD 的 6.1、11.1。

## 1. 意图

让前端能完整走通"登录 → 填资料 → 拿到宠物和自己的小屋 → 用种子码联调"。前端靠 `api/openapi.yaml` 生成客户端，所以接口契约和实现同等重要。Apple/Google 没有开发者账号，只搭接口，不做真实校验。

## 2. 目标 / 非目标 / 反目标

**目标**
- `POST /api/v1/auth/login/dev`、`/login/apple`、`/login/google`、`/refresh`、`/logout`
- `GET /api/v1/me`（前端判断是否需要 onboarding）、`POST /api/v1/me/onboarding`
- 鉴权中间件（Bearer access token）
- MBTI → 宠物映射、onboarding 事务内建小屋与 signup 码
- 种子数据：`task seed` 写入 6 个种子用户和零号码 `DEVA2345`
- `api/openapi.yaml` 补全以上接口，并有测试保证实现与契约不漂移（至少校验路径、状态码、错误码）

**非目标（留给后续）**
- Apple/Google 真实 ID Token 校验（接口返回 501）
- 登录限流（TRD 12.1，等 B3 一起做限流基础设施）
- `PATCH /me`、`/me/mbti-change`、`DELETE /me`、`devices`
- 入房、奖励码、`FULL2345`（B3）
- WebSocket

**反目标**：dev 登录在非 local 环境出现；日志或错误里出现 token、邮箱之外的凭据；refresh token 明文入库。

## 3. 约束

- 沿用 B1：Gin、pgx、统一响应 `{data}` / `{error:{code,message,request_id}}`，业务路径前缀 `/api/v1`。
- 本地 PostgreSQL + Memurai，无 Docker；数据库测试用 `petbridge_test`，`-short` 跳过。
- 不修改 `0001_core_schema.sql`，改动走新迁移 `0002`。
- 代码不自动提交，留给用户审查。

## 4. 方案比较

**A. 登录只建空 users 行，onboarding 做 UPDATE（采用）**。`users` 资料列本来就可空，`onboarded_at` 为空即"待 onboarding"。代价：库里会有从未完成 onboarding 的空用户，可由后续清理任务回收。
**B. 登录不建行，把身份暂存在 Redis/临时表，onboarding 才建 users**。库里干净，但 Redis 是可丢数据（TRD 11 章），丢了用户就要重新登录；token 的 `sub` 也没有稳定的 user id。弃用。
**C. 把登录与 onboarding 合成一个接口**。与 TRD 和前端状态机（`needsOnboarding`）不符。弃用。

选 A。用户已同意。

## 5. 已定的设计

### 5.1 登录
- dev 登录：body `{ "email": "..." }`。**未见过的邮箱自动创建用户**（用户已确认），身份记为 `auth_identities(provider='dev', subject=小写邮箱)`。路由只在 `APP_ENV=local` 且 `DEV_LOGIN_ENABLED=true` 时注册（配置层已强制非 local 不能开）。
- apple/google：路由始终注册；定义 `IDTokenVerifier` 接口，B2 的实现恒返回 `PROVIDER_NOT_CONFIGURED`（HTTP 501）。接口形状固定为 body `{ "id_token": "..." }`，成功响应与 dev 登录一致，这样接真实校验时契约不变。
- 登录响应：`{ access_token, refresh_token, expires_in, user: { id, onboarded, ... } }`。`onboarded` 由 `onboarded_at` 是否非空得出。
- `users.status` 为 `banned` 或 `deleted` 时拒绝登录与刷新（`ACCOUNT_DISABLED`，403）。
- 并发首次登录同一身份：靠 `unique(provider, subject)`，冲突后回读，保证只建一个用户。

### 5.2 令牌
- access：JWT HS256，claims 为 `sub`（user id）、`iat`、`exp`（`ACCESS_TOKEN_TTL`，默认 15m）；密钥为 `JWT_SIGNING_KEY`。校验时固定算法，拒绝 `none` 与其他算法。
- refresh：`crypto/rand` 32 字节、base64url；库里只存 SHA-256 哈希；有效期 `REFRESH_TOKEN_TTL`。
- **轮换与重放检测**：每次 `/refresh` 作废旧 token 并签发新的，同属一个 `family_id`。若收到的 token 已被作废（重放），把该 family 全部吊销并返回 `REFRESH_REUSED`（401）。需要迁移 `0002`：`refresh_tokens` 加 `family_id uuid not null`，加 `replaced_by uuid`。并发刷新同一 token 用 `UPDATE ... WHERE revoked_at IS NULL` 的行数判断赢家，输家按重放处理。
- `/logout`：吊销请求所带 refresh token 所在的整个 family；幂等，token 未知也返回 204。
- **我的取舍**：access token 无状态，登出后最多 15 分钟内仍可用，不做黑名单（Redis 可丢，不能承担安全语义）。
- 鉴权中间件：缺失/格式错/过期/签名错 → `UNAUTHORIZED`（401）。前端 401 时会单飞刷新，所以过期必须是 401 而不是 403。

### 5.3 onboarding
`POST /me/onboarding`，body：`native_lang`、`mbti`、`nickname`、`age_band`、`tz`。需登录。

校验：`native_lang` ∈ {ja,ko,zh-Hant,zh-Hans,en}；`mbti` 匹配 `^[EI][NS][TF][JP]$`（大写，服务端不做大小写容错）；`age_band` ∈ {13_17,18_plus}（未满 13 岁由客户端终止，后端不接收）；`nickname` 去首尾空白后 1–20 个字符；`tz` 必须是 `time.LoadLocation` 能识别的 IANA 名。失败返回 `VALIDATION_FAILED`（400），`details` 列出字段名。昵称敏感词过滤放 B6，本期不做。

**一个事务内**（先 `SELECT ... FOR UPDATE` 锁住 users 行，沿用 TRD 的加锁顺序 users 在先）：
1. 若 `onboarded_at` 已非空 → 直接返回现有结果，**不看本次 body**（TRD 7.1 第 4 点；前端失败后原样重试）。
2. UPDATE users 资料列、写 `pet_type`、`onboarded_at=now()`。
3. INSERT rooms（owner=自己，member_count=1）。
4. INSERT room_members（seat 1，role owner）。
5. INSERT invite_codes（source=`signup`，`issued_to`=自己，`expires_at`=now()+`CODE_TTL_DAYS`，默认 7）。码冲突（主键）时重试，最多 5 次。

响应：`{ user, pet: { type, mbti }, room: { id, member_count, capacity }, invite_code: { code, expires_at } }`。`code` 以 8 位无连字符大写返回，展示用的 `XXXX-XXXX` 由客户端格式化。

**并发**：同一用户同时发两次 onboarding，靠行锁串行，第二次走幂等分支，只会建一间小屋（`rooms.owner_id` 也有 unique 兜底）。

### 5.4 MBTI → 宠物
`pet_type` 落库用稳定的英文 slug，宠物名称与文案由前端 i18n 负责（PRD 说宠物名待美术确认，用 slug 才不会因改名迁移数据）：

| MBTI | slug | MBTI | slug |
| :-- | :-- | :-- | :-- |
| INTJ | lone_leopard | ENTJ | leader_lion |
| INTP | thinker_owl | ENTP | clever_fox |
| INFJ | guardian_deer | ENFJ | guide_dolphin |
| INFP | healing_rabbit | ENFP | happy_shiba |
| ISTJ | steady_beaver | ESTJ | order_sheepdog |
| ISFJ | guardian_penguin | ESFJ | warm_otter |
| ISTP | calm_blackcat | ESTP | daring_falcon |
| ISFP | easygoing_alpaca | ESFP | party_parrot |

用 Go 里的 map 实现，单元测试覆盖 16 型全部映射且无重复。

### 5.5 `GET /me`
返回 `{ id, nickname, native_lang, mbti, pet_type, age_band, tz, onboarded }`，未 onboarding 时资料字段为 null、`onboarded=false`。

### 5.6 种子数据（`task seed`）
`cmd/tools/seed`，**幂等**，可重复运行。直接调用与 onboarding 相同的 service 方法，保证种子用户走的就是真实路径：

| 邮箱 | 母语 | MBTI | 年龄段 |
| :-- | :-- | :-- | :-- |
| dev-a@local | ja | ENFP | 18_plus |
| dev-b@local | ko | INFP | 18_plus |
| dev-c@local | zh-Hant | INTJ | 18_plus |
| dev-d@local | zh-Hans | ESTJ | 18_plus |
| dev-e@local | en | ISFP | 18_plus |
| dev-f@local | en | ESFP | 13_17 |

另外为 A 的小屋写入 `DEVA2345`（source=`zero`，`issued_to` 为空，有效期 10 年，避免联调期间过期）。`FULL2345` 依赖入房逻辑，留到 B3。

### 5.7 错误码（B2 新增）
`UNAUTHORIZED` 401、`VALIDATION_FAILED` 400、`ACCOUNT_DISABLED` 403、`REFRESH_REUSED` 401、`REFRESH_INVALID` 401（未知/过期）、`PROVIDER_NOT_CONFIGURED` 501、`NOT_FOUND` 404（dev 登录路由未注册时由 Gin 默认处理）。

### 5.8 配置新增
`CODE_TTL_DAYS`（默认 7）加入 `config.Config`；其余沿用 B1 已有项。

### 5.9 代码结构
`internal/auth`（token、中间件、登录 handler、verifier 接口）、`internal/user`（onboarding service、宠物映射、GET /me）、`internal/invite`（仅码生成与格式，B3 复用）。SQL 先用手写 pgx，不引入 sqlc（只有十来条查询，B1 计划里写的"B2 引入"改为"B3 查询变多再引入"）。

## 6. 测试要求
- 单元：宠物映射 16 型；码生成字符集与长度；JWT 签发/校验（过期、篡改、`alg=none`）；onboarding 入参校验。
- 集成（`petbridge_test`）：dev 登录自动建用户且二次登录同一用户；refresh 轮换；重放旧 token 吊销整条 family；并发刷新只有一个成功；logout 后 refresh 失效；onboarding 事务产出 1 房 + 1 座位 + 1 码；重复调用幂等；并发两次 onboarding 只建一间小屋；banned 用户被拒；seed 运行两次结果一致。
- 契约：`openapi.yaml` 能被解析，且路由表中的 `/api/v1` 路径与文档一致。
- 手动冒烟：`scripts/smoke.ps1` 扩展为登录 → onboarding → `GET /me`。

## 7. 留给实现者
文件内部分层、表驱动测试写法、JWT 库选型（TRD 指定 `golang-jwt/jwt/v5`）、OpenAPI 契约测试用哪个解析库。
