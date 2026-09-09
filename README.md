# WorkBuddy2API

> WorkBuddy CN（CodeBuddy / copilot.tencent.com）与 WorkBuddy Global（www.workbuddy.ai）的 OpenAI 兼容反向代理网关：OAuth 登录、多账号池轮转、工具调用与流式/非流式转发。

> **使用前必读（合规）**：本项目是 **非官方** 网关，使用腾讯系 CodeBuddy / WorkBuddy 账号作为 API 上游。仅限本人授权账号、本机/私有环境测试使用。详见 [安全与合规](#安全与合规)。

## 目录

- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [配置说明](#配置说明)
- [账号轮换与冷却策略](#账号轮换与冷却策略)
- [Redis（Upstash）镜像](#redisupstash镜像)
- [请求级日志](#请求级日志)
- [API 端点](#api-端点)
- [安全与合规](#安全与合规)
- [工具脚本](#工具脚本)
- [稳定性设计](#稳定性设计)
- [开发](#开发)
- [免责声明](#免责声明)
- [License](#license)

## 功能特性

- **OAuth 登录** — 通过 `/v2/plugin/auth/state?platform=CLI` 设备授权流程获取凭证，运行时自动刷新 token
- **多账号轮转** — 三因子加权随机选号（credits ×10 + 闲置补偿 + 成功率 ×3），Top-5 短名单 + 防惊群（100ms 窗口）
- **工具调用** — 支持 OpenAI tools/tool_choice，出站 `tool_choice` 按上游约定归一化为字符串，流式 `tool_calls` 按 index 合并
- **流式 + 非流式** — 上游 SSE 透传（逐帧规范化）；出站一律强制 `stream:true`（`prepareBody` 改写），非流式响应由本地 `Aggregate` 聚合为单个 OpenAI 响应
- **定时任务** — 每日 09:00 / 21:00 签到 + 余额查询，冷却账号在余额恢复后解冻；每日 22:00 刷新 token 保活
- **账号池** — 熔断器（指数退避）+ 在途租约 + 三因子加权 + 会话粘性路由
- **积分监控** — `credit.sh` 一键查询全部账号剩余/总量/百分比
- **登录工具** — `login.sh` 交互式登录，落盘 auth 文件并重启容器
- **Docker 部署** — `docker compose up -d --build`，healthcheck 常驻（`wget /healthz`）
- **请求级日志** — 每个 `/v1/chat/completions` 请求打一行表格日志（序号/模型/模式/状态码/uid/TTFB/tok/tok每秒/total）
- **健康检查** — `/healthz` 在无可用账号（healthy 且未占满在途）时返回 503，可接负载均衡器

## 快速开始

### 1. 克隆 & 配置

```bash
git clone https://github.com/Sliverkiss/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
# 编辑 config.json，至少设置 api_key（留空 = 不鉴权，公网部署务必设置）
```

### 2. 添加账号

```bash
./login.sh
# 打开浏览器登录 → 回来按 y → 自动签到 → 落盘 auths/workbuddy-<uid>.json → 重启容器
```

### 3. 启动服务

```bash
docker compose up -d --build
```

### 4. 验证

```bash
# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 账号状态（汇总 + 每账号详情 + in_flight_full/sticky_sessions/redis_mode）
curl -s http://localhost:7863/status -H "Authorization: Bearer your-api-key"

# 健康检查（无可用账号时 503）
curl -s http://localhost:7863/healthz

# 聊天补全（流式）
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 聊天补全（非流式，本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 配置说明

完整字段与默认值以 [`config.example.json`](config.example.json) 为样例；下表为各字段含义。

```json
{
  "listen": ":7863",
  "api_key": "your-api-key-here",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "region": "cn",
  "cooldown": {
    "soft_rate": "60s"
  },
  "schedule": {
    "checkin_hours": [9, 21],
    "keepalive_hours": [22]
  },
  "upstream": {
    "timeout_seconds": 120,
    "header_timeout_seconds": 120,
    "idle_timeout_seconds": 300
  },
  "features": {
    "sanitize_blacklist_fingerprints": true
  },
  "upstash": {
    "url": "",
    "token": ""
  },
  "pool": {
    "max_in_flight": 3,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "breaker_cooldown_max": "6h",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0
  },
  "session_sticky": {
    "enabled": true,
    "ttl": "30m",
    "gc_interval": "5m"
  }
}
```

### 默认值与字段说明

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | HTTP 监听地址；`WB2A_LISTEN` 可覆盖 |
| `api_key` | `""`（空） | 网关鉴权密钥；**空 = 不鉴权直接放行**；`WB2A_API_KEY` 可覆盖 |
| `auth_dir` | `./auths` | 账号凭证目录；`WB2A_AUTH_DIR` 可覆盖 |
| `state_file` | `./data/state.json` | 池状态持久化文件；`WB2A_STATE_FILE` 可覆盖 |
| `region` | `cn` | 只收 `cn` / `global`（决定上游 host，见[端点清单](#3-各功能访问的端点清单)）；`WB2A_REGION` 可覆盖 |
| `cooldown.soft_rate` | `60s` | 429/404 软冷却时长；`WB2A_SOFT_RATE` 可覆盖 |
| `schedule.checkin_hours` | `[9, 21]` | 每日本地时区整点签到 + 余额查询 |
| `schedule.keepalive_hours` | `[22]` | 每日本地时区整点刷新 token |
| `upstream.timeout_seconds` | `120` | 短 RPC（refresh/checkin/balance/FetchModels）总时长 |
| `upstream.header_timeout_seconds` | 回落 `timeout_seconds` | 聊天 SSE 首字节前（响应头）上限；`WB2A_HEADER_TIMEOUT_SECONDS` 可覆盖 |
| `upstream.idle_timeout_seconds` | `300` | 聊天 SSE 流中空闲上限；`WB2A_IDLE_TIMEOUT_SECONDS` 可覆盖 |
| `features.sanitize_blacklist_fingerprints` | `true` | 出站请求体黑名单指纹脱敏；`WB2A_SANITIZE_FINGERPRINTS` 可覆盖 |
| `upstash.url` / `token` | 空 | 空 = 纯内存模式（Noop 降级），见 [Redis 镜像](#redisupstash镜像) |
| `pool.max_in_flight` | `3` | 单账号最大在途请求数；`0 = 不限` |
| `pool.breaker_threshold` | `3` | 连续失败触发熔断的阈值 |
| `pool.breaker_cooldown` | `30m` | 熔断基础退避时长 |
| `pool.breaker_cooldown_max` | `6h` | 指数退避封顶 |
| `pool.idle_weight_per_hour` | `0.5` | 闲置补偿：每小时未使用 +0.5 权重 |
| `pool.idle_weight_max` | `5.0` | 闲置补偿封顶 |
| `session_sticky.enabled` | `true` | 会话粘性路由开关 |
| `session_sticky.ttl` | `30m` | 会话绑定 TTL（滚动续期） |
| `session_sticky.gc_interval` | `5m` | 过期绑定 GC 周期 |

样例未列出的未知 JSON 字段会被解析器忽略，不影响运行。

### upstream 三段超时语义

三种超时各归其位，`timeout_seconds` 不约束聊天流总时长：

| 字段 | 作用对象 | 缺省回落 | 说明 |
|---|---|---|---|
| `timeout_seconds` | 短 RPC 总时长 | `120` | refresh/checkin/balance/FetchModels；到期报错走既有换号/熔断 |
| `header_timeout_seconds` | 聊天 SSE 首字节前（响应头） | 回落 `timeout_seconds` | 到期 = `Do` err → 换号重发（「首字节前换号」行为） |
| `idle_timeout_seconds` | 聊天 SSE 流中空闲 | `300` | 活跃吐数据续命不掐；静默超过阈值才断流释放租约 |

聊天流（`stream` 无论 true/false）**没有总时长上限**：聊天使用 `Timeout=0` 的专用 client（首字节由 `Transport.ResponseHeaderTimeout` 管，流中空闲由 `IdleTimeout` 管）。长思考/长回答不受 120s 限制。

### 环境变量覆盖

`Load()` 先读 JSON，再用 `WB2A_*` 环境变量覆盖（仅当变量非空时生效）：

`WB2A_LISTEN`、`WB2A_API_KEY`、`WB2A_AUTH_DIR`、`WB2A_STATE_FILE`、`WB2A_REGION`、`WB2A_SOFT_RATE`（duration 字符串）、`WB2A_TIMEOUT_SECONDS`、`WB2A_HEADER_TIMEOUT_SECONDS`、`WB2A_IDLE_TIMEOUT_SECONDS`、`WB2A_SANITIZE_FINGERPRINTS`（布尔）。

## 账号轮换与冷却策略

### 状态机

每个账号有三个正交维度（`internal/pool/pool.go`）：

1. **健康维度**：`healthy = !disabled && !until 生效 && !breakerUntil 生效`
   - `until`：按错误类型的即时冷却（`CoolSoft` 429/404、`CoolHard` 余额耗尽到次日 04:00）
   - `breakerUntil`：连续失败（`fails`）触发熔断的指数退避截止
2. **并发维度**：`inFlight`（在途租约，运行态，不持久化）
3. **统计维度**：`successCount` / `errTotal`（累计，供成功率权重）/ `lastUsed` / `lastSuccess` / `lastErr`

```
        Healthy ──(429/404 软冷却、402 硬冷却、5xx 熔断)──▶ 冷却/熔断期
           ▲                                                    │
           │                                      (到期自动 / 签到解冻 / 成功清零)
           └────────────────────────────────────────────────────┘
        Disabled（session 死亡，永久，需人工重新登录）
```

- 软/硬冷却、熔断各自到期自动恢复；`NoteSuccess` 清零熔断（`fails`/`retryCount`/`breakerUntil`）；签到成功（余额恢复）仅解冻冷却、不动熔断。
- `Disabled` 不可自愈，需人工重登（重新执行 `login.sh` 覆盖 auth 文件）。

### 错误分类与冷却策略（`upstream.Classify` + `applyErrorPolicy`）

| 分类 (`ErrKind`) | 触发条件 | 账号处理 | 恢复方式 |
|---|---|---|---|
| `ErrHardCredit` | HTTP 402，或 body 含余额不足关键词（`insufficient credit`/`余额不足`/`积分不足` 等） | 硬冷却到次日 04:00（本地时区） | 冷却到期；签到任务（09:00/21:00）余额恢复后 `ReenableIfCredits` 解冻 |
| `ErrSoftRate` | HTTP 429 | 软冷却 `soft_rate`（默认 60s） | 到期自动恢复 |
| `ErrSessionDead` | body 含 `Offline user session not found` / `12153` | **永久禁用** | 人工重新登录 |
| `ErrNotFound` | HTTP 404 | 软冷却（默认 60s） | 到期自动恢复 |
| `ErrServer` | HTTP ≥500 | 喂连续失败计数器 `fails`，达 `breaker_threshold` 熔断 | 熔断到期 / 成功清零 |
| `ErrClient` | 其他 4xx / 业务 `code≠0` | 不处罚，仅换号重试 | 即时 |
| `ErrNone` | HTTP 2xx | 成功 | — |

**连续失败计数器（熔断信号）**：所有走即时冷却的入口（`Cooldown`，即 429/404/402）与 5xx（`NoteError`）都会喂入唯一的连续失败计数器 `fails`；累计达到 `pool.breaker_threshold`（默认 3）触发熔断，退避 `breaker_cooldown × 2^retryCount`，封顶 `breaker_cooldown_max`（`6h`）。成功（`NoteSuccess`）清零 `fails`/`retryCount`/`breakerUntil`。

### 挑选策略（三因子加权）

1. **状态过滤**：Disabled / 冷却（`until` 生效）/ 熔断（`breakerUntil` 生效）/ 在途占满 不选
2. **Top-5 候选**：按三因子权重降序取前 5（credits 只是权重的一个因子）
3. **三因子加权随机**：`weight = credits 比例 ×10 + idleWeight + successRate ×3`
   - `credits 比例` = 该号 credits / 候选集最大 credits（避免量纲爆炸）
   - `idleWeight` = min(距 lastUsed 小时数 × `idle_weight_per_hour`, `idle_weight_max`)；从未使用给满分
   - `successRate` = successCount/(successCount+errTotal)；无请求记录给中性 1.5（偏信任）
4. **防惊群**：跳过 100ms 内刚被选中的账号（除非 top5 全部刚被用过，退回 LRU）

credits 全 0 时仍按闲置 + 成功率加权（不退化均匀随机）。

### 账号池能力

- **熔断器（指数退避）**：连续 `pool.breaker_threshold` 次失败熔断，退避 `breaker_cooldown × 2^retryCount` 封顶 `breaker_cooldown_max`；成功清零。单一连续失败计数器 `fails`；签到解冻只清冷却（余额恢复）不动熔断。
- **在途租约**：单账号并发上限 `pool.max_in_flight`（`0 = 不限`），`Pick` 跳过占满账号，`Acquire` CAS 兜底并发竞态。
- **会话粘性路由**：按会话键（见[会话键](#会话键提取)）尽量绑定同一账号，TTL 滚动续期；请求失败自动解绑，请求成功后绑定跟随最终成功号。
- **全冷却兜底**：无 healthy 账号时，从「非禁用、非余额耗尽、未占满」的软冷却/熔断账号中选最早到期者顶班。

#### 会话键提取

`session.ExtractKey` 按实现顺序依次尝试：

1. `metadata.conversation_id`
2. `metadata.user_id`
3. 顶层 `conversation_id`

## Redis（Upstash）镜像

- 配置 `upstash.url/token`（空 = 纯内存模式 Noop 降级，一切功能照常，仅打一条启动日志）。
- Redis 仅做**异步镜像**（粘性会话映射防重启丢失 + 池状态快照恢复备份），**不在请求热路径同步调用**；所有写操作 fire-and-forget，读操作只发生在启动时。
- 池状态快照：每次本地 `state.json` 落盘同步镜像一份到 Redis（带 `saved_at`）；启动时**择新恢复**（`RestoreFromSnapshot`）——Redis 快照比本地新才采用，否则本地优先。
- 粘性会话绑定 `key → uid` 镜像到 Redis（前缀 `wb2api:bind:`，默认 TTL 7 天），启动时 `LoadFromStore` 恢复。
- `/status` 透出 `redis_mode`（`upstash`/`noop`）与池级 `sticky_sessions`。

## 请求级日志

每个 `/v1/chat/completions` 请求结束后打一行表格日志到 **stdout**（`os.Stdout`，无 log 时间戳前缀）：

```
| #001 | 18:31:31 | deepseek-v4 | stream | 200 | uid=0851ce35 | TTFB=801ms | tok=60 | 23.5tok/s | total=2.6s |
```

字段说明（`internal/server/logging.go`）：

| 字段 | 说明 |
|---|---|
| `#001` | 进程级请求序号（atomic 计数器） |
| `18:31:31` | 请求结束时刻（`15:04:05`） |
| `deepseek-v4` | 模型名，超过 11 字符截断 |
| `stream` / `sync` | 请求模式 |
| `200` | 最终响应状态码 |
| `uid=0851ce35` | 账号 UID 前 8 位 |
| `TTFB=801ms` | 流式首帧到达耗时（非流式显示 `-`） |
| `tok=60` | 输出 token 数（采信上游 `usage.completion_tokens`，缺失显示 `-`） |
| `23.5tok/s` | 输出 token 速率 |
| `total=2.6s` | 请求总时长 |

**敏感度**（见[安全与合规](#安全与合规)）：日志不含 `accessToken`/`refreshToken`/`api_key` 明文，但含账号 `uid` 前 8 位与模型名（半敏感）。**无落盘日志文件**——请求日志写 stdout，其余模块日志走 Go 默认 logger（stderr），容器内两者均进入 `docker logs`。

## API 端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer（`api_key` 非空时） | OpenAI 兼容聊天补全（流式/非流式）；请求体上限 8 MiB |
| `GET /v1/models` | Bearer（`api_key` 非空时） | 模型列表（动态拉取，缓存 1h；失败回落静态表 + 5min 负缓存） |
| `GET /status` | Bearer（`api_key` 非空时） | 账号状态：`total`/`healthy`/`cooling`/`disabled`/`in_flight_full`/`sticky_sessions`/`redis_mode` + 每账号详情 |
| `GET /healthz` | **无** | 健康检查：`ServableNow`（存在 healthy 且未占满在途账号）为真返回 200 `{healthy,total}`，否则 503 |

> 鉴权逻辑（`handler.withAuth`）：仅当 `api_key` 非空时才校验 `Authorization: Bearer <api_key>`；`api_key` 为空时所有走 `withAuth` 的端点**直接放行**。`/healthz` 恒无鉴权。

## 安全与合规

以下每一节都可对照代码复核，关键断言见文末[附录：关键断言 ↔ 代码出处](#附录关键断言--代码出处)。

### 1. auths 凭据管理

- **默认保存位置**：`./auths`（配置 `auth_dir`，环境变量 `WB2A_AUTH_DIR`）。运行时 `auth.LoadDir(cfg.AuthDir, cfg.Region)` 扫描 `workbuddy*.json`。
- **文件名**：`workbuddy-<uid>.json`（由 `login.sh` 落盘命名）。
- **文件内容结构**（嵌套形，`internal/auth/auth.go` 的 `SaveAtomic` / `login.sh` 均为该形）：

  ```json
  {
    "account": { "uid": "...", "enterpriseId": "...", "nickname": "..." },
    "auth": {
      "accessToken": "明文 access token",
      "refreshToken": "明文 refresh token",
      "expiresAt": 0,
      "domain": ""
    }
  }
  ```

  其中 `auth.accessToken` / `auth.refreshToken` 为**明文**，`expiresAt` 为 Unix 秒。同时兼容扁平形（CPA 面板手建的 `{"accessToken":...,"uid":...}`）。
- **运行权限语义**：
  - 容器内以 `app` 用户（uid 10001，见 `Dockerfile` 的 `adduser -D -u 10001 app` + `USER app`）运行；
  - token 刷新后由 `SaveAtomic` 以 `0600` 原子写回（tmp + rename）；`data/state.json` 亦以 `0600` 落盘；
  - `login.sh` 首次落盘用 Python `open(...,"w")`，**未显式 chmod**（遵循登录时 umask）；`scripts/sync-auths.sh` 显式 `chmod 600`。建议登录后手动 `chmod 600 auths/*.json`。
  - 宿主目录挂载：`./auths → /app/auths`、`./data → /app/data`、`./config.json → /app/config.json:ro`（见 `docker-compose.yml`）。
- **切勿提交 git**：`.gitignore` 已排除 `auths/`、`data/`、`backups/`、`config.json`、`*.key`、`*.pem`。
- **备份建议**：`auths/`（凭证）与 `data/state.json`（池状态快照：credits / 冷却 / 禁用 / 成功失败计数等）建议一并备份；配置 Upstash 后，`state.json` 每次落盘还会 fire-and-forget 镜像一份到 Redis（带 `saved_at`），启动时择新恢复。
- **登录工具**：
  - `login.sh`（CN OAuth）：`cmd/login url`（POST `/v2/plugin/auth/state?platform=CLI` 拿 `state`+`authUrl`）→ 浏览器登录 → `cmd/login poll`（GET `/v2/plugin/auth/token?state=`，再 GET `/v2/plugin/login/account?state=` 拿 uid/nickname）→ 首次签到 → 落盘 `auths/workbuddy-<uid>.json` → `docker restart workbuddy2api`。
  - `cmd/login` 等价实现（`cmd/login/main.go`），全程 CN realm，无 PKCE（state 由服务端签发）。
  - `signin.sh` / `cmd/signin`：遍历 `auths/` 全部账号批量签到（过期先 refresh）。

### 2. 网络暴露与日志敏感度

- **默认监听**：`:7863`（配置 `listen` / `WB2A_LISTEN`）。`docker-compose.yml` 端口 `"7863:7863"` 将宿主机 `0.0.0.0:7863` 暴露，默认无 TLS。
- **鉴权要求**（`internal/server/handler.go` 的 `withAuth`）：
  - 仅当 `api_key` 非空时，`/v1/chat/completions`、`/v1/models`、`/status` 要求 `Authorization: Bearer <api_key>`；
  - **`api_key` 为空时这三个端点直接放行（不鉴权）**；
  - `/healthz` 恒无鉴权。
  - 如需公网暴露：**必须设置 `api_key`**，并建议仅内网使用或前置反代/TLS。
- **请求级日志字段**（`internal/server/logging.go` 的 `logChatRow`）：序号、时刻、模型名、模式（`stream`/`sync`）、状态码、uid 前 8 位、TTFB、tok、tok/s、total。
  - **不含** `accessToken` / `refreshToken` / `api_key` 明文（日志只取 uid 前 8 位、模型名、计时/计数，不读 `Authorization` 头，不落 token）。
  - **含半敏感信息**：账号 `uid` 前 8 位与模型名。
- **日志落点**：请求表格日志写 **stdout**；其余模块日志走 Go 默认 logger（**stderr**）。容器内两者均进入 `docker logs`；**代码无任何日志文件写入（无落盘日志文件）**。

### 3. 各功能访问的端点清单

#### 上游端点（本项目调用 CodeBuddy / WorkBuddy 的接口）

| 端点 | 方法 | Host（按 region） | 用途 |
|---|---|---|---|
| `/v2/chat/completions` | POST | `chatBase`：cn=`https://copilot.tencent.com` / global=`https://www.workbuddy.ai` | chat 补全（SSE） |
| `/console/enterprises/personal/models` | GET | 同上 `chatBase` | 动态模型列表 |
| `/v2/plugin/auth/token/refresh` | POST | 同上 `chatBase` | token 刷新（仅此处携带 `X-Refresh-Token`） |
| `/v2/billing/meter/daily-checkin` | POST | `billingBase`：cn=`https://www.codebuddy.cn` / global=`https://www.workbuddy.ai` | 每日签到 |
| `/v2/billing/meter/get-user-resource` | POST | 同上 `billingBase` | 余额查询 |
| `/v2/plugin/auth/state?platform=CLI` | POST | `https://copilot.tencent.com`（仅 CN，cmd/login） | OAuth 拿 `state`+`authUrl` |
| `/v2/plugin/auth/token?state=` | GET | `https://copilot.tencent.com`（仅 CN，cmd/login） | 轮询拿 token |
| `/v2/plugin/login/account?state=` | GET | `https://copilot.tencent.com`（仅 CN，cmd/login） | 拿 uid/nickname |

> 上游访问统一携带 User-Agent `CLI/2.63.2 CodeBuddy/2.63.2`，并带 `Origin`/`Referer`（cn=`www.codebuddy.cn`、global=`www.workbuddy.ai`）；chat 请求头带 `X-User-Id`/`X-Enterprise-Id`/`X-Domain`/`X-Product: SaaS`，**chat 请求永不携带 `X-Refresh-Token`**（见 `internal/upstream/headers.go`）。

#### 公开性说明（克制表述）

- 本项目自身对外暴露的 `/v1/chat/completions`、`/v1/models`、`/status`、`/healthz` 为自建端点，接口形态对齐 OpenAI。
- 上述 `/v2/*` 上游端点为 CodeBuddy/WorkBuddy 官方 CLI/插件内部使用的接口，**未见公开 API 文档，属非公开/逆向接口**。本项目不主张任何上游接口的官方授权或稳定性承诺。

#### 调度行为

- `checkin_hours` 默认 `[9, 21]`：每日本地时区整点对非禁用账号执行签到 + 余额查询，余额 > 0 时 `ReenableIfCredits` 解冻冷却账号。
- `keepalive_hours` 默认 `[22]`：每日本地时区整点刷新全部非禁用账号 token；`ErrSessionDead` 自动禁用。
- 时区取自容器 `TZ`（`docker-compose.yml` 默认 `Asia/Shanghai`）；`nextDay4AM` 判定用本地时区。

### 4. 发布来源与合规边界

- **无预编译 release**：仓库无 GitHub Release、无 tag；产物来源 = **源码自构建**。
- **构建命令**：`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server`（`Dockerfile` 多阶段构建，构建镜像 `golang:1.23-alpine`，运行时 `alpine:3.20`）。`go.mod` 声明 `go 1.22.5`。
- 登录/积分工具：`go build -o login ./cmd/login`、`go build -o credit ./cmd/credit`（脚本在二进制缺失时自动编译）。
- **无官方校验和**：仓库无 `.sha256`/`.sig` 等产物校验文件（`.gitignore` 排除编译产物 `/login`、`/credit`、`/wb2api`、`/signin_bin`）；`go.sum` 仅为 Go 模块依赖校验，非发布产物校验。
- **Docker 镜像**：由本地 `docker compose build` 从源码生成（`docker-compose.yml` 的 `build: .`），未引用外部镜像。
- **依赖第三方商业服务**：CodeBuddy / WorkBuddy 属腾讯系商业产品；本项目是其**非官方 OpenAI 兼容网关**，使用其账号做 API 网关涉及目标平台的服务条款与账号风险。**作者不对账号封禁、条款违约或任何使用结果负责**（见[免责声明](#免责声明)）。

### 5. 授权使用边界建议

- 仅限**本人授权账号**、仅限**本机/私有环境测试**；
- 不得用于共享、商业转售、违规分发或任何违反目标平台条款的用途；
- 遵守 CodeBuddy / WorkBuddy 的平台 ToS 与所在地法律；
- 妥善保管 `auths/`（含明文 token）与本服务端口，公网暴露必须配置 `api_key`。

## 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录，落盘 auth 文件并重启容器 |
| `./credit.sh` | 积分日报（美化输出） |
| `./credit.sh -json` | 积分原始 JSON |
| `./signin.sh [auths_dir]` | 批量签到（遍历 auth 目录下所有账号，过期先 refresh） |
| `./scripts/sync-auths.sh` | 从 CPA 容器拷出 CN workbuddy auth 到 `./auths`（保留 CN、跳过 global，chmod 600） |

## 稳定性设计

- **防雪崩**：上游 4xx/5xx 轮转重试（`MaxRotate` 默认 3 次，不直接返回）
- **错误分流**：网络层错误不计失败（避免抖动连坐）；429/404/402 软硬冷却与 5xx 共同喂连续失败计数器，达 `breaker_threshold`（默认 3）触发指数退避熔断
- **请求日志**：一行表格日志（序号/模型/模式/状态码/uid/TTFB/tok/tok每秒/total）便于排查慢请求
- **连接池**：`MaxIdleConns=100`、`MaxIdleConnsPerHost=20`、`IdleConnTimeout=90s` 减少 TLS 握手
- **凭证续期**：token 临近过期（提前 `RefreshSkew` 默认 10m）自动 refresh；`ErrSessionDead` 失败禁用账号，其余失败 `NoteError`
- **状态持久化**：`data/state.json` dirty flag + 5s 周期异步落盘，进程退出前 `Flush` 强制落盘
- **防惊群**：100ms 窗口内不重复选中同一账号（高并发时打散热点）
- **header 超时**：服务端 `ReadHeaderTimeout=30s`

## 开发

### 测试

```bash
go build ./...
go test ./... -count=20  # 多次运行（无 flake）
go vet ./...
gofmt -l .  # 应为空
```

### 代码结构

```
cmd/
  server/     # 主服务入口（config + main + 路由装配）
  login/      # OAuth 登录工具（cmd/login）
  credit/     # 积分查询工具（cmd/credit）
  signin/     # 批量签到工具（cmd/signin）
internal/
  auth/       # auth 文件解析 + token 刷新 + 原子写回
  pool/       # 账号池（状态机 + 冷却/熔断 + 在途租约 + 加权挑选 + 持久化）
  scheduler/  # 定时签到 + token keepalive
  server/     # HTTP handler + 鉴权 + 请求级日志
  session/    # 会话粘性路由（双段分配 + 快/慢路径）
  upstream/   # 上游封装（chat/billing/auth/headers/sse/payload/sanitize/idle）
  redisstore/ # Upstash 持久化 + Noop 降级
```

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 WorkBuddy / CodeBuddy 的服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本仓库未包含 LICENSE 文件（`git ls-files` 中无 LICENSE）。如需使用或再分发，请向仓库所有者确认授权条款。

## 附录：关键断言 ↔ 代码出处

| # | 关键断言 | 代码出处 |
|---|---|---|
| 1 | 默认监听 `:7863`、`api_key` 空、`auth_dir ./auths`、`state_file ./data/state.json`、`region cn` | `cmd/server/config.go` `Default()` |
| 2 | `checkin_hours=[9,21]`、`keepalive_hours=[22]` | `cmd/server/config.go` `Default()` / `internal/scheduler/scheduler.go` |
| 3 | 三段超时：timeout=120、header 回落 timeout、idle=300 | `cmd/server/config.go` `normalize()` + `cmd/server/main.go` |
| 4 | 聊天流无总时长上限（`ChatHTTP.Timeout=0`，首字节走 `ResponseHeaderTimeout`，流中空闲走 `IdleTimeout`） | `internal/upstream/client.go` `New()` + `internal/upstream/idle.go` `monitorBody` |
| 5 | 三因子权重 `credits比例×10 + idleWeight + successRate×3`，无记录 successRate=1.5 | `internal/pool/pool.go` `weightOf` |
| 6 | 熔断：threshold=3、cooldown=30m、max=6h、退避 ×2^retryCount | `internal/pool/pool.go` `defaultBreaker*` + `recordBreakerFailureLocked` |
| 7 | 冷却入口（429/404/402）也喂连续失败计数器 `fails` | `internal/pool/pool.go` `Cooldown` 调 `recordBreakerFailureLocked` |
| 8 | session 死亡判定：body 含 `Offline user session not found`/`12153` → 禁用 | `internal/upstream/client.go` `sessionDeadMarkers` + `internal/server/handler.go` `applyErrorPolicy` |
| 9 | 硬冷却到次日 04:00（本地时区） | `internal/pool/pool.go` `CooldownUntilTomorrow4AM`/`nextDay4AM` |
| 10 | 会话键提取顺序：`metadata.conversation_id` → `metadata.user_id` → 顶层 `conversation_id` | `internal/session/session.go` `ExtractKey` |
| 11 | `/healthz` 用 `ServableNow`（healthy 且未满在途）判定 200/503 | `internal/server/handler.go` `healthz` + `internal/pool/pool.go` `ServableNow` |
| 12 | 日志字段不含 token；uid 截 8 位、model 截 11 位；写 stdout | `internal/server/logging.go` `logChatRow` |
| 13 | `api_key` 非空才校验 Bearer，否则放行；`/healthz` 无鉴权 | `internal/server/handler.go` `withAuth`/`NewHandler` |
| 14 | 上游 host：`copilot.tencent.com`/`www.workbuddy.ai`/`www.codebuddy.cn` | `internal/upstream/client.go` `New()` |
| 15 | auth 文件内容（嵌套形）+ `0600` 原子写回 | `internal/auth/auth.go` `SaveAtomic`/`Parse` |
| 16 | 容器 `app` 用户 uid 10001、`EXPOSE 7863`、healthcheck `wget /healthz` | `Dockerfile` |
| 17 | 挂载 `./auths:/app/auths`、`./data:/app/data`、端口 `7863:7863` | `docker-compose.yml` |
| 18 | OAuth 端点 `?platform=CLI`/`/auth/token`/`/login/account`（CN） | `cmd/login/main.go` |
| 19 | 动态模型缓存 1h + 失败负缓存 5min | `internal/server/handler.go` `dynamicModelsTTL`/`modelsFetchFailCooldown` |
| 20 | 请求体 8 MiB 上限、`MaxRotate` 默认 3、`RefreshSkew` 默认 10m | `internal/server/handler.go` |

（以上均为正文引用过的断言；如需复核更细细节，以 `config.example.json` 为配置样例、以 `.go` 源码为行为依据。）