# PetBridge Backend

跨国 MBTI 情感 AI 电子宠物的后端服务（Go 模块化单体）。设计见仓库根目录的 PRD 与后端 TRD。

## 本地依赖

- Go（以 `go.mod` 为准）
- PostgreSQL 16（库 `petbridge`、`petbridge_test`，用户/密码 `petbridge`）
- Redis 兼容服务（本机为 Memurai，手动启动）
- [go-task](https://taskfile.dev)：`go install github.com/go-task/task/v3/cmd/task@latest`

## 首次启动

```powershell
copy .env.example .env    # 本地配置；.env 不进仓库。端口默认 8090，可改 APP_PORT
task up                   # 启动 Redis，并检查 PostgreSQL 服务
task migrate              # 建表
task dev                  # 启动 API
task smoke                # 另开终端：检查 /healthz 与 /readyz
```

Redis 不是系统服务，**重启电脑后需要重新执行 `task up`**。

## 常用命令

| 命令 | 作用 |
| :-- | :-- |
| `task test` | 单元测试（不需要外部服务） |
| `task test-int` | 全部测试（需要 PostgreSQL 与 Redis） |
| `task lint` | `go vet` |
| `task down` | 停掉 Redis |

## 探活

- `GET /healthz`：进程存活。
- `GET /readyz`：PostgreSQL 不可用返回 503；仅 Redis 不可用返回 200 且 `status` 为 `degraded`。
