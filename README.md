<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关</b><br>
  OAuth 登录 · 账号池轮转 · 熔断与冷却 · 会话粘性 · 定时签到/活跃/旅行/保活 · 流式/非流式
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker_Compose-2496ED?logo=docker&logoColor=white&style=flat-square">
  <img alt="Transport" src="https://img.shields.io/badge/Transport-SSE%20%2F%20Streaming-0DBD8B?style=flat-square">
</p>

---

## 📖 项目简介

WorkBuddy2API 是一个自托管的 **OpenAI 兼容反向代理网关**，将腾讯 CodeBuddy（`copilot.tencent.com`）账号包装为统一的 `/v1/chat/completions` 服务。

- 官方不提供 OpenAI 形态的开放 API，本项目通过 **OAuth 设备授权** 获取账号凭证，在网关侧做 token 自动刷新、账号池调度与流量治理；
- 面向 **个人多账号** 场景：多账号共享、单号故障自动换号、冷却/熔断防止雪崩、会话粘性保证多轮上下文不跳号；
- 对客户端只暴露 OpenAI 兼容接口，现有 SDK / 前端 / 工具 **零改造接入**。

> ⚠️ 合规须知：本项目是**非官方**网关，使用 CodeBuddy 账号作为上游，**仅限本人授权账号、本机/私有环境测试**。详细边界见 [安全与合规](#-安全与合规)。

## ✨ 核心能力

| 能力 | 说明 |
|---|---|
| 🔑 **OAuth 一键登录** | `login.sh` 设备授权流程（无 PKCE），自动落盘凭证并重启容器 |
| 🔄 **多账号池** | 三因子加权随机选号（积分比例 ×10 + 闲置补偿 + 成功率 ×3），Top-5 候选 + 防惊群 |
| 🛡️ **熔断与冷却** | 429/限流文案软冷却 600s 起指数退避（封顶 `soft_rate_max`）、404 固定 60s 短冷却、402/余额不足硬冷却至次日 04:00、连续失败指数退避熔断、在途租约限流 |
| 🧲 **会话粘性** | 同一会话（`conversation_id`）尽量绑定同一账号，TTL 滚动续期，失败自动解绑 |
| ⏰ **定时任务** | 签到（09/21 点）+ 活跃上报（10 点，点亮连登/解锁领养）+ 猫猫旅行（9 点，独立排程）+ token 保活（22 点），四类各自独立开关 |
| ⚡ **流式 + 非流式** | 上游 SSE 逐帧规范化透传；出站强制 `stream:true`，非流式由本地聚合为单响应 |
| 🧠 **推理模型兼容** | `reasoning_content` 白名单保留、工具调用（`tool_calls`）按 index 合并、effort 自动降级 |
| 📊 **可观测** | 每请求一行表格日志（TTFB/token 速率/uid）；`/healthz` 带 `service` 身份标识可接负载均衡/宿主探活 |
| 💾 **状态持久化** | 池状态本地原子落盘 + Upstash Redis 异步镜像（可选），重启择新恢复 |
| 🗑️ **指纹脱敏** | 出站请求体黑名单指纹字段清洗（可关闭）；同时兜底用户/assistant 上下文中的指纹串 |
| 💬 **系统提示词** | 网关自有系统提示词替换客户端 system（默认 `custom`），从源头消灭 system 来源的指纹误报；`passthrough` 模式遇内容拦截误报时自动降级重试 |

## 🗺️ 架构总览

```mermaid
flowchart LR
    Client["客户端 / SDK\nOpenAI 兼容请求"] --> H

    subgraph GWI["WorkBuddy2API 网关 :7863"]
        H["HTTP Handler\n鉴权 · 日志 · 换号轮转"] --> P
        H --> S
        P["账号池\n三因子加权 · 熔断 · 冷却 · 租约"] --> U
        S["会话粘性路由"] -.绑定镜像.-> REDIS
        T["定时调度\n签到 09/21 · 旅行 09 · 活跃 10 · 保活 22"] --> P
        U["上游 Client\nChatHTTP 流式 · 短 RPC"]
    end

    P -. "读凭证 (0600)" .-> AUTH[("auths/*.json")]
    P -. "状态镜像" .-> REDIS[("Upstash Redis\n可选")]
    U -->|"v2/chat/completions (SSE)"| CB["CodeBuddy\ncopilot.tencent.com"]
    U -->|"billing / auth / models"| CB
```

## 🚀 快速开始

### 环境要求

- **Docker + Docker Compose**（推荐部署方式，镜像内已含 `app` 低权限用户）
- 一个（或多个）已注册的 CodeBuddy 账号，用于 OAuth 登录
- 宿主机 Go ≥ 1.22（仅本地直接编译时需要）

### 1. 克隆并配置

```bash
git clone https://github.com/Sliverkiss/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
```

编辑 `config.json`，**至少设置 `api_key`**（`留空 = 不鉴权`，公网部署务必设置）：

```bash
# 用编辑器把 "api_key" 改成你自己的强随机串
```

### 2. 登录添加账号

```bash
./login.sh
# 1) 脚本输出授权 URL
# 2) 浏览器打开完成登录
# 3) 回到终端按 y → 自动签到 → 落盘 auths/workbuddy-<uid>.json → 重启容器
```

多账号只需重复执行；账号池自动发现 `auths/` 下新增凭证文件（容器启动时 `SyncToDir` 对齐）。

### 3. 启动服务

```bash
docker compose up -d --build
```

### 4. 验证

```bash
# 健康检查（无可用账号时 503）；service 字段用于确认打到的是本网关
curl -s http://localhost:7863/healthz
# {"healthy":2,"total":3,"service":"workbuddy2api"}

# 模型列表
curl -s http://localhost:7863/v1/models \
  -H "Authorization: Bearer your-api-key"

# 账号状态（汇总 + 每账号详情）
curl -s http://localhost:7863/status \
  -H "Authorization: Bearer your-api-key"

# 流式聊天
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 非流式聊天（本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## ⚙️ 配置说明

完整字段以 [`config.example.json`](config.example.json) 为样例（下表为各字段含义）。

```json
{
  "listen": ":7863",
  "api_key": "your-api-key-here",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "server": { "max_body_mb": 8 },
  "cooldown": { "soft_rate": "600s", "soft_rate_max": "2h" },
  "schedule": {
    "checkin_hours": [9, 21],
    "keepalive_hours": [22],
    "checkin_enabled": true,
    "keepalive_enabled": true
  },
  "upstream": {
    "timeout_seconds": 120,
    "header_timeout_seconds": 120,
    "idle_timeout_seconds": 300,
    "user_agent": ""
  },
  "features": { "sanitize_blacklist_fingerprints": true },
  "prompt": { "mode": "custom", "file": "" },
  "upstash": { "url": "", "token": "" },
  "pool": {
    "max_in_flight": 3,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "breaker_cooldown_max": "6h",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0
  },
  "session_sticky": { "enabled": true, "ttl": "30m", "gc_interval": "5m" }
}
```

### 字段速查

| 字段 | 默认 | 说明 |
|---|---|---|
| `server.max_body_mb` | `8` | 聊天请求体大小上限（MB，0/负数会启动报错）。超限直接返回 **413 `request_body_too_large`**，不再静默截断喂给上游（多图/超长上下文会话请调大此项） |
| `listen` | `:7863` | HTTP 监听地址 |
| `api_key` | 空 | 网关鉴权密钥；**空 = 不鉴权直接放行**（公网必须设置） |
| `auth_dir` | `./auths` | 账号凭证目录 |
| `state_file` | `./data/state.json` | 账号池状态持久化文件 |
| `cooldown.soft_rate` | `600s` | 软限流（429/限流文案）冷却**基数**；同一账号连续触发按 2 倍指数退避 |
| `cooldown.soft_rate_max` | `2h` | 软冷却指数退避的封顶时长 |
| `schedule.checkin_hours` | `[9, 21]` | 每日本地时区整点签到 + 余额查询解冻。**空数组/`null` = 未配置回落默认**（不是禁用） |
| `schedule.travel_hours` | `[9, 21]` | 每日本地时区整点推进猫猫旅行状态机（领养/派出/领奖）。空数组/`null` 同上 |
| `schedule.activity_hours` | `[10]` | 每日本地时区整点对话活跃上报（点亮连登 + 解锁 first_buddy）。空数组/`null` 同上 |
| `schedule.keepalive_hours` | `[22]` | 每日本地时区整点刷新 token 保活。空数组/`null` 同上 |
| `schedule.checkin_enabled` | `true` | 签到**总开关**；`false` 真正关掉签到 |
| `schedule.travel_enabled` | `true` | 猫猫旅行**总开关**；`false` 完全停旅行（独立于签到） |
| `schedule.activity_enabled` | `true` | 活跃上报**总开关**；`false` 停活跃上报 |
| `schedule.keepalive_enabled` | `true` | token 保活总开关；`false` 关掉保活 |
| `upstream.timeout_seconds` | `120` | 短 RPC（刷新/签到/余额/模型）总时长上限 |
| `upstream.header_timeout_seconds` | 回落 `timeout_seconds` | 聊天首字节前（响应头）上限 |
| `upstream.idle_timeout_seconds` | `300` | 聊天流中空闲上限（活跃续命，静默断流） |
| `upstream.user_agent` | 空 | 出站 User-Agent 覆盖（空 = 现状 `CLI/2.63.2 CodeBuddy/2.63.2`）。全部出站请求生效（chat/refresh/checkin/balance/report/travel/模型列表）。官网「使用端」列按出站 UA 服务端归因：官方 WorkBuddy 桌面 UA 为 `WorkBuddy/<version>`，想让它显示 WorkBuddy 可配 `"WorkBuddy/2.x.x"`。默认保持现状（指纹净化考虑，可配而非改死） |
| `features.sanitize_blacklist_fingerprints` | `true` | 出站请求体黑名单指纹脱敏 |
| `prompt.mode` | `custom` | 系统提示词模式：`custom` = 网关用自有提示词替换客户端 system；`passthrough` = 透传客户端原始 system（降级重试仍切中性提示词） |
| `prompt.file` | 空 | 提示词文件路径；空 = 内置默认（~2KB）；路径非空但不可读 → 启动报错 |
| `upstash.url` / `token` | 空 | 空 = 纯内存模式（Noop 降级，功能照常） |
| `pool.max_in_flight` | `3` | 单账号最大在途请求数（`0` = 不限） |
| `pool.breaker_threshold` | `3` | 连续失败触发熔断阈值 |
| `pool.breaker_cooldown` | `30m` | 熔断基础退避时长 |
| `pool.breaker_cooldown_max` | `6h` | 指数退避封顶 |
| `pool.idle_weight_per_hour` | `0.5` | 闲置补偿：每小时未使用 +0.5 权重 |
| `pool.idle_weight_max` | `5.0` | 闲置补偿权重封顶 |
| `session_sticky.enabled` | `true` | 会话粘性路由开关 |
| `session_sticky.ttl` | `30m` | 会话绑定 TTL（滚动续期） |
| `session_sticky.gc_interval` | `5m` | 过期绑定 GC 周期 |

### 上游超时语义（三段各归其位）

| 字段 | 作用对象 | 默认 | 行为 |
|---|---|---|---|
| `timeout_seconds` | 短 RPC（token 刷新 / 签到 / 余额 / 模型列表） | `120` | 总时长硬上限，到期报错走换号/熔断 |
| `header_timeout_seconds` | 聊天 SSE **首字节前** | `120` | 由 `Transport.ResponseHeaderTimeout` 约束；超时 = 换号重发 |
| `idle_timeout_seconds` | 聊天 SSE **流中空闲** | `300` | 活跃吐数据**续命**不掐；静默超时才断流释放租约 |

聊天流（`stream` true/false 均同）**没有总时长上限**：聊天使用 `Timeout=0` 的专用 client，长思考/长输出（如超长 reasoning）不会被 120s 掐断。

### 环境变量覆盖

加载顺序：JSON 文件 → `WB2A_*` 环境变量（变量非空才覆盖）：

`WB2A_LISTEN` · `WB2A_API_KEY` · `WB2A_AUTH_DIR` · `WB2A_STATE_FILE` · `WB2A_MAX_BODY_MB` · `WB2A_SOFT_RATE`（duration） · `WB2A_SOFT_RATE_MAX`（duration） · `WB2A_TIMEOUT_SECONDS` · `WB2A_HEADER_TIMEOUT_SECONDS` · `WB2A_IDLE_TIMEOUT_SECONDS` · `WB2A_USER_AGENT`（非空覆盖出站 UA，同 `upstream.user_agent`） · `WB2A_SANITIZE_FINGERPRINTS`（bool） · `WB2A_PROMPT_MODE` · `WB2A_PROMPT_FILE`

## 💬 系统提示词

客户端（Claude Code / Codex 等 CLI）会在 system prompt 注入固定模板句，上游内容审核按**逐字精确匹配**误杀合法流量（HTTP 400 + 审核文案）。网关支持用自有系统提示词从源头消灭 system 来源的指纹误报，由 `prompt.mode` 控制：

| 模式 | 语义 |
|---|---|
| `custom`（默认） | 出站前用网关自有提示词**替换**客户端 system/developer 消息（删除全部 system/developer，头部插入单条 system）；user/assistant/tool 消息逐字不动 |
| `passthrough` | 透传客户端原始 system，不做改写 |

### 降级重试

`passthrough` 模式请求被上游内容策略拦截（HTTP 400 + `blocked by security policy` / `unapproved channel` / `illegal api invocation` 文案）时，判定为 system 指纹误报：**同请求内**换 Degraded 中性提示词重试一次；第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端，并如实报给调用方。

- 触发降级后持续到**次日 00:00 CST**（Asia/Shanghai）重置；期间 `passthrough` 请求直达中性提示词，不再先撞 400（已在降级期不续期，保持最早触发点的 00:00 重置）
- 降级状态是**进程内存态**，重启清零
- 内容问题非账号问题：`ErrContentBlocked` 不罚账号（无冷却/熔断/计错），由网关降级重试消化

### 提示词内容

内置默认提示词约 2KB（`internal/prompt/defaultprompt.md`，嵌入二进制）。`prompt.file` 指向自定义提示词文件（自定义人格/人设）即整体替换内置默认；**留空 = 内置默认**。路径非空但不可读 → **启动报错**（fail fast，不回落到内置默认）。`custom` 模式下提示词随每次请求出站，token 成本由文件体积决定——内置仅 ~2KB，自定义文件需自行权衡（无独立计费保护）。

> 与指纹脱敏的关系：本小节解决 **system/developer 来源**的指纹误报；用户/assistant 消息里的指纹串仍由 `features.sanitize_blacklist_fingerprints` 清洗——两层叠加、互不替代。

## 🧠 账号池与流量治理

### 账号状态机

每个账号由三个正交维度描述：

| 维度 | 字段 | 说明 |
|---|---|---|
| 健康 | `disabled` / `until` / `breakerUntil` | `healthy = !disabled && !until && !breakerUntil` |
| 并发 | `inFlight` | 在途租约（运行态，不持久化），上限 `max_in_flight` |
| 统计 | `successCount` / `errTotal` / `lastUsed` | 供成功率权重与闲置补偿 |

```text
  Healthy ──429/404 软冷却 / 402 硬冷却 / 5xx 熔断──▶ 冷却·熔断期
     ▲                                                │
     │       到期自动恢复 / 签到余额解冻 / 成功清零     │
     └────────────────────────────────────────────────┘

  Disabled（session 死亡，永久，需人工重新 login.sh）
```

### 错误分类与处置

| 分类 | 触发条件 | 账号处置 | 恢复 |
|---|---|---|---|
| 余额不足 | HTTP 402 / body 含余额关键词 | 硬冷却到**次日 04:00**（本地时区） | 签到（09/21 点）余额恢复自动解冻 |
| 频控 | HTTP 429 / 限流文案（不限状态码） | 软冷却 `soft_rate`（600s 起，连续触发指数退避，封顶 `soft_rate_max`）。**`code 6004`（模型级）带「将在 … 重置」时收窄**到上游重置墙钟并豁免切模型（见[常见问题](#429-code6004模型级限流的冷却语义)） | 到期自动恢复 / 成功清零退避 |
| Session 失效 | body 含 `Offline user session not found` / `12153` | **永久禁用** | 人工重新登录 |
| 上游 404 | HTTP 404 | 软冷却固定 60s（不随 `soft_rate`、不单独退避） | 到期自动恢复 |
| 服务端错误 | HTTP ≥500 | 喂连续失败计数，达阈值熔断 | 熔断到期 / 成功清零 |
| 请求体解析失败 | HTTP 400 + `Unmarshal chat params failed` / code `11101` | **不罚账号，但仍轮转**（客户端畸形 JSON，换号照样 400） | 即时 |
| 客户端错误 | 其余 4xx / 业务 `code≠0` | 不处罚，换号重试 | 即时 |

请求体解析失败（`11101`）与内容拦截一样**不罚账号**：问题在请求内容而非账号健康，冷却好号只会
增加下一次真正限流时的风险。`11101` 的上游 body 在 503 兜底文案中原样透传，便于排查。触发
`11101` 的**网关侧截断已由 `server.max_body_mb` 的 413 消灭**——剩余的 11101 只可能是客户端
发来的畸形 JSON，属客户端需自检。

**熔断器**：所有冷却入口（429/404/402）与 5xx 共用唯一连续失败计数器 `fails`；累计达 `breaker_threshold`（默认 3）触发熔断，退避 `breaker_cooldown × 2^retryCount`，封顶 `6h`；成功清零。

**软冷却指数退避**（与熔断器并存的第二条升级线）：软限流的**冷却时长**本身也按连续次数退避——同一账号连续触发软冷却时 `soft_rate × 2^(连续次数-1)`，封顶 `soft_rate_max`。计数 `soft_streak` 独立于熔断器的 `fails`，只在**成功**或**签到解冻**时清零，随 `state.json` 持久化。两者别混淆：软退避管"近期被限流"（600s→1200s→2400s…，分钟~小时级），熔断管"病态反复失败"（30m→1h→2h→6h）。

### 选号策略

1. 过滤：禁用 / 冷却 / 熔断 / 在途占满账号不参与
2. 取 **Top-5** 候选（按三因子权重降序，积分只是因子之一）
3. 三因子加权随机：
   `weight = credits 比例 ×10 + idleWeight + successRate ×3`
   - `credits 比例` = 该号积分 / 候选集最大积分
   - `idleWeight` = `min(闲置小时 × idle_weight_per_hour, idle_weight_max)`，从未使用给满分
   - `successRate` = `successCount/(successCount+errTotal)`，无记录给中性 1.5
4. 防惊群：跳过 100ms 内刚被选中的账号；全冷却时从非禁用、非余额耗尽的软冷却/熔断账号中选最早到期者顶班

### 会话粘性

同一会话尽量复用同一账号，多轮对话不跳号：

- 会话键提取顺序：`metadata.conversation_id` → `metadata.conversationId` → `metadata.user_id` → 顶层 `conversation_id` → 顶层 `conversationId`（snake_case 优先于 camelCase）
- TTL 滚动续期（默认 30m），GC 周期 5m；绑定可镜像到 Redis（7 天）防重启丢失
- 请求失败自动解绑；成功后绑定跟随最终成功账号

### 定时任务

| 任务 | 开关 | 时刻（本地时区） | 行为 |
|---|---|---|---|
| 签到 | `schedule.checkin_enabled` 默认 `true` | `checkin_hours` 默认 `[9, 21]` 整点 | 签到 + 余额查询；余额恢复则解冻冷却账号 |
| 活跃上报 | `schedule.activity_enabled` 默认 `true` | `activity_hours` 默认 `[10]` 整点 | 对话活跃上报（`/v2/report`）；点亮连登 + 解锁 `first_buddy`；每号每天 1 次 |
| 猫猫旅行 | `schedule.travel_enabled` 默认 `true` | `travel_hours` 默认 `[9, 21]` 整点 | 独立排程：无猫领养 / idle 派出 / arrived 领奖；**已从签到剥离** |
| 保活 | `schedule.keepalive_enabled` 默认 `true` | `keepalive_hours` 默认 `[22]` 整点 | 全账号刷新 token；session 失效自动禁用 |

四类任务各自独立排程、各有开关，互不影响。容器时区由 `TZ` 控制（compose 默认 `Asia/Shanghai`）。

#### 关闭定时任务

用 `schedule.*_enabled` 显式关闭，四者互相独立：

```json
"schedule": {
  "checkin_hours": [9, 21],
  "travel_hours": [9, 21],
  "activity_hours": [10],
  "keepalive_hours": [22],
  "checkin_enabled": false,
  "travel_enabled": true,
  "activity_enabled": true,
  "keepalive_enabled": true
}
```

上例：**只关签到，旅行/活跃/保活照常跑**。四个都设 `false` 则调度器无任何时点可等，
`Run` 不空转、直接阻塞等待退出信号。

几条必须知道的语义：

- **为什么用独立开关，而不是把小时数组留空**：空数组与 `null` 一贯表示
  **「未配置 → 回落默认」**，不是「禁用」。真正关闭请用 `*_enabled: false`。
- **禁用不会擦除小时配置**：`*_hours` 原样保留，改回 `true` 即恢复原时点。
- **小时值必须是 0-23**：非法值在启动时直接报错并提示改用开关（不做静默兜底）。
- 开关只影响本进程的定时排程；独立的一次性工具（`signin.sh`）是另一个进程，不受约束。
- **关签到的连带影响**：签到的余额查询会「余额恢复即解冻」被硬冷却的账号（402），
  关掉后这类账号只能等硬冷却次日 04:00 自然到期回池。

#### 活跃上报（独立排程）

对池内每个可用账号在 `activity_hours`（默认 `[10]` 整点）发送一条对话活跃上报
（`POST /v2/report`，事件 `chat_request_send`，body 为数组，事件必须含 `userId`）：

- 一条上报同时点亮 growth 连登 + 解锁 `first_buddy` 任务（领养前置）
- 每号每天 1 次即可（`activity_hours` 单时点）：日活跃奖励按天去重，重复上报无额外收益
- `conversationId` 由网关生成（`wb2api-<ms>`），无需真实会话
- 限速：账号间间隔 800ms（与旅行同口径）
- 关闭：`schedule.activity_enabled: false`
- 手动诊断/补跑用 `python3 scripts/probe_active.py`（只读探测；写操作默认 dry-run，需 `--yes`）

#### 猫猫旅行（独立排程）

对池内每个可用账号在 `travel_hours`（默认 `[9, 21]` 整点）单趟推进一次，
每趟只做一个动作，不轮询不等待。默认两趟闭环：9 点领昨日到站奖励并派出，
21 点领当日到站奖励（`daily_limit_reached` 自动挡住二次派出）。

| 探测结果 | 动作 |
|---|---|
| 无猫（`buddy` 为 `null`） | 先同意协议（幂等），再尝试领养；过门槛则 +300 积分并获得猫 |
| `state=idle` 且今日未派出 | 派出 `location_id=4`（古镇客栈；4 个地点收益/时长区间相同，无最优解） |
| `state=arrived` | 领取到站奖励（带 `record_id`） |
| `state=traveling` / 今日已达上限 / 未知状态 | 跳过 |

- 领养门槛未达标时上游返回 HTTP 400，每账号每自然日只尝试一次（跨日重试，记录仅存内存）；
  门槛可用活跃上报解除
- 限速：账号间间隔 800ms（46 个账号约 40s）
- 每自然日 1 次派出：按 CST（Asia/Shanghai）自然日重置，与容器 `TZ` 无关
- 失败隔离：单账号失败只跳过该账号当趟；401 不强刷（token 刷新交保活时点）
- 关闭：`schedule.travel_enabled: false`

签到与保活配到同一小时（如都含 22 点）时，两类任务都会执行。

## 🔌 API 端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer（`api_key` 非空时） | OpenAI 兼容补全；流式/非流式；请求体上限 `server.max_body_mb`（默认 8 MB） |
| `GET /v1/models` | Bearer（`api_key` 非空时） | 模型列表（动态拉取，缓存 1h；失败回落静态表 + 5min 负缓存） |
| `GET /status` | Bearer（`api_key` 非空时） | 账号状态汇总 + 每账号详情（积分/冷却/熔断/在途/粘性） |
| `GET /healthz` | 无 | 健康检查：有 healthy 且未占满账号返回 200，否则 503；响应带身份标识（见下） |

> 鉴权规则：仅当 `api_key` 非空才校验 `Authorization: Bearer <api_key>`；**`api_key` 为空时上述端点直接放行**；`/healthz` 恒无鉴权。

`/healthz` 响应示例（200/503 同结构，仅状态码与计数变化）：

```json
{"healthy": 2, "total": 3, "service": "workbuddy2api"}
```

响应同时带 `X-Service: workbuddy2api` 头。这两个身份标识用于区分**本网关**与同端口上
可能残留的其他服务——后者即使返回 2xx 也不会带该字段/头，宿主探测据此避免"假成功"。

### 宿主健康探测指引

宿主程序（如 workbuddy-switch 托管网关子进程）探活时，**"端口通 + 返回 2xx" 不足以
证明打到了自己的网关**：同端口可能残留旧版本进程或别的服务，对方返回 2xx 会造成假成功。
按校验强度从高到低有两种做法：

**① 强校验（推荐）：`/status` + `api_key`**

```bash
# 期望 200；若返回 401 则说明对面的 /status 不认这个 api_key —— 不是自己的网关
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer <api_key>" \
  http://127.0.0.1:7863/status
```

`/status` 挂在鉴权中间件上：只有持有正确 `api_key` 的本网关才会返回 200，旧服务/其他服务
只会返回 401（或 404）。**注意前提**：本网关 `api_key` 非空才具备这个判别力；
`api_key` 为空时 `/status` 直接放行，退化为弱校验。

宿主判定建议：`200` → 健康；`401` → 不是自己的网关（端口被占）；连接失败 → 未就绪；
`5xx` → 网关已就位但池不可服务（可再叠加 `/healthz` 的 503 语义）。

**② 弱校验（无凭据场景）：`/healthz` + `service` 字段**

```bash
# 必须同时校验 service 字段；只判断 HTTP 状态码仍可能假成功
curl -s http://127.0.0.1:7863/healthz | grep -q '"service":"workbuddy2api"'
```

适合负载均衡器 / 容器编排这类**不该持有 api_key** 的探活方（`/healthz` 恒无鉴权，
200=可服务、503=池内无可服务账号）。判据是响应体 `service == "workbuddy2api"`；
响应头 `X-Service` 可用于只读头部的探活实现。若对面返回 2xx 但缺该标识 → 判为异常。

> 容器自带的 `HEALTHCHECK` 用的就是 ②（仅进程内自检，够用）；
> 宿主做**跨进程归属确认**时用 ①。

### 流式行为细节

- 出站请求强制 `stream:true`；SSE 帧按 OpenAI 规范**白名单重建**（`reasoning_content` 保留、工具调用按 index 合并、未知字段剥离）
- 保证恰好一个 `data: [DONE]`（上游漏发时兜底补写）；空流先写一帧 `error` 再补 `[DONE]`；`error` 帧原样透传

## ❓ 常见问题

### 429 `code=6004`（模型级限流）的冷却语义？

上游的 `429` + `code 6004` 是**该模型的使用量超限**（msg 通常带「将在 YYYY-MM-DD HH:MM:SS
UTC+8 重置」），**不是账号整体被限流**。网关的处理：

- **冷却到上游重置时间**：msg 带「将在 … 重置」时，账号冷却 `until` 精确等于该墙钟
  （按 UTC+8 解释），并封顶 `soft_rate_max`（默认 2h）——不再因为 600s 起的指数退避
  无视上游明说的恢复时刻。
- **切模型立即可用**：冷却由 6004 触发时会记录触发模型；同一账号改用**其他模型**请求
  时视为可用（模型级限流不该拉黑账号的其它模型）。同模型或未记录模型的冷却回到现状。
- **退回指数退避**：6004 无「将在 … 重置」文案，或非 6004 的普通软限流 → 仍是
  `soft_rate`（600s 起，连续触发指数退避，封顶 `soft_rate_max`），行为不变。
- 依然是软冷却（`cool_kind=soft_rate`），熔断信号照常喂入；到期自动恢复/成功清零退避。

### 多图会话请求体超限怎么办？

请求体超过 `server.max_body_mb`（默认 8 MB）时网关直接返回：

```json
{"error":{"message":"请求体超过 8 MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试","type":"api_error","code":"request_body_too_large"}}
```

- 该错误在**网关侧**判出，**不会**打上游、**不会**罚账号、**不会**轮转；
- 收到 `413 request_body_too_large` 即表示是请求体本身超限（多图/超长上下文场景），
  调大 `server.max_body_mb` 即可（`WB2A_MAX_BODY_MB` 环境变量同样生效）；
- 历史版本的静默截断已废弃：此前 8M 截断会把半截 JSON 喂给上游触发
  `Unmarshal chat params failed ... unexpected EOF`，网关却把责任记在账号头上
  （罚号 + 轮转耗尽 503）——现在要么放行要么明确 413，不再"截断后误伤"。

## 📋 请求级日志

每个 `/v1/chat/completions` 请求结束时输出一行表格日志（stdout）：

```text
| #001 | 18:31:31 | deepseek-v4 | stream | 200 | uid=0851ce35 | TTFB=801ms | tok=60 | 23.5tok/s | total=2.6s |
```

| 字段 | 说明 |
|---|---|
| `#001` | 进程级请求序号 |
| `18:31:31` | 结束时刻 |
| `deepseek-v4` | 模型名（超 11 字符截断） |
| `stream` / `sync` | 请求模式 |
| `200` | 状态码 |
| `uid=0851ce35` | 账号 UID 前 8 位 |
| `TTFB` | 流式首帧耗时（非流式为 `-`） |
| `tok` / `tok/s` / `total` | 输出 token 数 / 速率 / 总时长 |

**敏感度**：日志不含任何 token 明文（详见[安全与合规](#-安全与合规)），无落盘日志文件。

## 🛡️ 安全与合规

### 1. 凭据管理（auths）

- **位置**：`./auths`（`auth_dir` 可配），文件名 `workbuddy-<uid>.json`
- **内容**：明文 `accessToken` / `refreshToken` + 账号元信息，结构见下：

```json
{
  "account": { "uid": "…", "enterpriseId": "…", "nickname": "…" },
  "auth": { "accessToken": "明文", "refreshToken": "明文", "expiresAt": 0, "domain": "" }
}
```

- **权限**：容器内以 `app` 用户（uid 10001）运行；token 刷新由 `SaveAtomic` 以 `0600` 原子写回（tmp + rename）；`login.sh` 首次落盘遵循登录 umask，建议手动 `chmod 600 auths/*.json`
- **备份**：备份 `auths/`（凭证）与 `data/state.json`（池状态：积分/冷却/计数）；配置 Upstash 后状态另镜像至 Redis
- **切勿提交 git**：`.gitignore` 已排除 `auths/`、`data/`、`backups/`、`config.json`、`*.key`、`*.pem`

### 1.5 敏感文件清单与 .gitignore 纪律

**永远不要提交到 git 的内容**（`.gitignore` 已覆盖，`git add` 前自查）：

| 路径 | 内容 | 风险 |
|---|---|---|
| `config.json` | `api_key`、Upstash `token`/`url` | 网关鉴权凭据 + 第三方密钥 |
| `auths/` | 各账号明文 `accessToken`/`refreshToken` | 账号直接被接管 |
| `data/` | `state.json` 池状态（积分/冷却/计数） | 运营信息 |
| `backups/` | 历史 auths/state 压缩包/快照 | 同 auths/ |
| `docs/`、`TASK-*.md`、`ACCEPT-*.md`、`REPORT*` 等 | 本地工作文档（可能含内网拓扑/主机名/临时凭据） | 信息泄露 |
| `*.key` / `*.pem` / `*.env` | 私钥、环境变量文件 | 凭据泄露 |

**纪律**：
- 新建敏感文件先确认落在 `.gitignore`（`git check-ignore <path>` 验证）。
- 提交前 `git status` 全绿后，再跑一遍 `git diff --cached --stat` 确认没有凭据文件混入。
- 仓库内只允许 `config.example.json` 形式的模板配置（占位符，零真值）。

### 2. 网络暴露与日志敏感度

- 默认监听 `:7863`，compose 暴露 `0.0.0.0:7863`，**无内置 TLS**；公网部署必须设置 `api_key`，建议前置反代/内网
- 请求日志字段：序号/模型/模式/状态码/**uid 前 8 位**/TTFB/token 数——**不含** `accessToken`/`refreshToken`/`api_key` 明文（不读取 `Authorization` 头）
- 日志写 **stdout/stderr**（容器内进入 `docker logs`），代码无任何落盘日志文件

### 3. 上游访问端点清单

| 端点 | 方法 | Host | 用途 |
|---|---|---|---|
| `/v2/chat/completions` | POST | `copilot.tencent.com` | 聊天补全（SSE） |
| `/console/enterprises/personal/models` | GET | 同上 | 动态模型列表 |
| `/v2/plugin/auth/token/refresh` | POST | 同上 | token 刷新 |
| `/v2/billing/meter/daily-checkin` | POST | `www.codebuddy.cn` | 每日签到 |
| `/v2/billing/meter/get-user-resource` | POST | 同上 | 余额查询 |
| `/v2/report` | POST | 同上 | 对话活跃上报（`chat_request_send` 事件数组，必须含 `userId`；点亮连登/解锁领养） |
| `/v2/plugin/auth/state?platform=CLI` | POST | `copilot.tencent.com` | OAuth 取授权 URL |
| `/v2/plugin/auth/token?state=` | GET | 同上 | OAuth 轮询取 token |
| `/v2/plugin/login/account?state=` | GET | 同上 | OAuth 取账号信息 |
| `/activity/growth/buddy/agreement` `first` `info` | POST/GET | 同上 | 猫猫旅行：同意协议 / 首次领养 / 查询 |
| `/activity/growth/buddy/travel/status` `depart` `claim` | GET/POST | 同上 | 猫猫旅行：状态 / 派出 / 领奖（独立排程 travel_hours） |

> 上述 `/v2/*` 端点是 CodeBuddy 官方 CLI/插件使用的接口，**未见公开 API 文档，属非公开/逆向接口**；本项目不主张任何上游接口的官方授权或稳定性承诺。出站统一携带 `CLI/2.63.2 CodeBuddy/2.63.2` UA；聊天请求带账号头（`X-User-Id` 等），**永不携带 `X-Refresh-Token`**。

### 4. 发布来源与合规边界

- **无预编译 release**：仓库无 Release / tag，产物 = 源码自构建
- 构建命令：`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server`（Dockerfile 多阶段：`golang:1.23-alpine` 构建 → `alpine:3.20` 运行）
- 登录/签到/积分工具：`./login.sh` / `./signin.sh` / `./credit.sh`（缺失时自动编译对应 `cmd/*`）
- **无产物校验和**：`go.sum` 仅约束 Go 模块依赖；Docker 镜像由本地 `docker compose build` 生成，未引用第三方镜像
- 上游 CodeBuddy 属腾讯系商业产品，本项目是其**非官方 OpenAI 兼容网关**；使用其账号做 API 网关涉及目标平台服务条款与账号风险，作者不对账号封禁、条款违约或使用结果负责

### 5. 授权使用边界

- 仅限**本人授权账号**、本机/私有环境测试
- 不得共享、转售、违规分发，或用于违反目标平台条款的用途
- 遵守 CodeBuddy 平台服务条款与所在地法律
- 妥善保管 `auths/`（明文凭证）与网关端口

## 🧰 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录 → 落盘 auth → 重启容器 |
| `./signin.sh [auths_dir]` | 批量签到（过期先刷新） |
| `./credit.sh` / `./credit.sh -json` | 积分日报（美化 / 原始 JSON） |
| `python3 scripts/probe_active.py` | 活跃上报手动诊断/补跑（probe=只读 / report=单号上报 / unlock=单号领猫 / ALL=全池；写操作默认 dry-run，需 `--yes`） |

## 🛠️ 开发

### 本地构建与测试

```bash
go build ./...
go vet ./...
go test ./... -count=20   # 多次运行验证无 flake
go test -race ./... -count=1
gofmt -l .
```

### 目录结构

```
cmd/
  server/    # 主服务（config + main + 路由装配）
  login/     # OAuth 登录工具
  credit/    # 积分查询工具
  signin/    # 批量签到工具
internal/
  auth/      # 凭证解析 + token 刷新 + 原子写回
  pool/      # 账号池（状态机/熔断/租约/加权/持久化）
  scheduler/ # 定时签到 + 保活 + 猫猫旅行巡检
  server/    # HTTP handler + 鉴权 + 请求日志
  session/   # 会话粘性路由
  upstream/  # 上游封装（chat/billing/auth/headers/sse/payload/sanitize/idle）
  redisstore/# Upstash 持久化 + Noop 降级
```

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 允许任意使用、复制、修改、合并、发布、分发、再授权及销售
- 再分发（源码或二进制形式，包括内嵌编译产物的整合项目）时，请保留原仓库的 MIT 版权声明与许可声明（如在 NOTICE 或 README 中注明原始出处 `https://github.com/Sliverkiss/workbuddy2api`，我们将不胜感激）
- 本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利；使用者仍需自行遵守上游服务条款（见上方免责声明）
