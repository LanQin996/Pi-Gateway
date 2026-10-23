# Pi Gateway

面向 Pi / Responses API 客户端的自托管 ChatGPT 订阅代理网关。提供账号与 API Key 管理、HTTP / WebSocket
转发、统一规则引擎、用量统计，以及请求和响应的传输差异分析。

后端使用 Go，管理界面使用 Vue 3 + TypeScript；生产前端嵌入 Go 可执行文件，运行时不需要单独部署 Node.js 服务。

> 本项目提供 Responses API 和 Chat Completions 兼容入口，并非所有 OpenAI API 的通用实现。当前部署模型是**单个活动网关实例**；启用 PostgreSQL 或
> Redis 不等于支持多实例无状态负载均衡。

## 功能概览

- **账号管理**：ChatGPT 登录、凭据导入与刷新、账号分组、代理配置、模型限制和调度策略；支持批量启用、禁用、恢复、加入分组以及仅含邮箱与凭证的
  JSON 导入导出。兼容 sub2api 与 CLIProxyAPI 的 Codex 凭证，标准格式和迁移规则见 [ACCOUNTS_FORMAT.md](ACCOUNTS_FORMAT.md)
  。管理员登录会话在有效期内跨网关重启保留。
- **客户端 API Key**：按 Key 管理访问范围、并发、限额及预算；客户端使用网关签发的 Key，而不是管理员密码或账号令牌。
- **协议转发**：HTTP SSE、非流式 JSON、下游 WebSocket；上游支持 SSE、WebSocket、缓存连接与自动回退。
- **模型与配额**：账号模型目录自动同步、模型路由与禁用配置；可关联独立 Codex 凭据查询配额和补充模型目录。
- **统一规则**：递归条件、多动作、优先级、启停、三阶段处理、图形 / JSON 双模式编辑，以及无上游副作用的试运行。
- **传输分析**：查看原始客户端输入、实际发往上游的内容、响应帧及差异；按规则和动作追踪修改来源。
- **持久化**：默认 SQLite，可选 PostgreSQL；Redis 用于有界、可丢弃的会话与缓存元数据。
- **管理界面**：中英文切换、明暗主题、使用统计、传输记录和在线设置。

启用模型自动同步时，网关在启动 5 秒后首次扫描启用账号，之后在每轮结束 30 秒后再次扫描，
发现新增账号或新关联的 Codex 凭据；已有模型目录按 `models.refresh_interval_seconds` 间隔重新同步。
账号较多时单轮同步可能耗时；后台同步失败保留上次成功目录和手动补充的模型。

传输分析的 JSON diff 保留原始请求字段顺序，并仅在比较时对齐另一侧对象字段；数组和消息顺序始终保留。
大 body、长行和 SSE 文本不会被前端裁切，原始值视图保留双方各自的字段顺序。
记录默认容量为 64 MiB，规则追踪最多使用其中一半；`capture.max_bytes_per_record` 可调大。
已有配置中的显式容量不会自动升级，超过记录总预算仍会标记裁切，旧记录已丢失的内容无法恢复。

识别到 `pi (...)` User-Agent 时，网关保留白名单内的 Pi SDK 平台、版本、语言及超时请求头；
其他客户端继续使用配置的 Pi 请求头，鉴权始终使用上游账号凭据。
默认规则集包含可编辑、可禁用的“排除图像生成”规则，通过 `drop_tools.types` 排除内置
`image_generation`（包括 namespace 中的工具）及对应工具选择；同名自定义 function 保留。
旧安装升级后会一次性补入缺失的该规则并默认启用；已有固定 ID 的规则保留编辑和禁用状态。
迁移完成后手动删除该规则，重启也不会再次补入。
在管理界面“规则”中启用该规则即可；自行添加时，使用请求阶段、始终匹配，动作选择 `drop_tools`，
参数为 `{"types":["image_generation"]}`。此动作处理 `tools` 及其 namespace，不扫描历史 `input`。

普通生成请求默认透传客户端未被官方明确禁止的字段，包括 `instructions`、完整 `text` 对象
（如 `verbosity` / `format`）、`parallel_tool_calls` 和未知扩展字段；保留嵌套值及显式 `false` / `null`。
依据 [Sign in with ChatGPT 预览限制](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)，
网关固定 `store:false`、`stream:true`，将字符串 `input` 转为消息数组，移除 `background`、`conversation`、
`max_output_tokens`、`max_tool_calls`、`metadata`、`moderation`、`multi_agent`、`prompt`、`prompt_cache_retention`、
`safety_identifier`、`temperature`、`top_logprobs`、`top_p`、`truncation` 和 `user`；规则无法重新引入这些字段。
未列为禁止只表示网关允许透传，具体模型及上游仍可能拒绝该参数。`previous_response_id` 继续由网关校验
同一认证连接的 WebSocket 续接，不能作为 HTTP 持久会话使用；顶层 `type` 由 WebSocket 传输层生成。
管理界面的复制按钮同时支持 Clipboard API 和局域网 HTTP 的浏览器复制回退。

Codex 在普通 Responses 请求末尾发送 `compaction_trigger` 时，网关将该操作视为压缩请求：
移除触发项，发送完整历史，并将压缩结果适配回 JSON、SSE 或 WebSocket Responses 响应。
同时支持 `/v1/responses/compact`、`/backend-api/codex/responses/compact`、
`/codex/responses/compact` 和 `/responses/compact`，这些入口直接返回压缩结果 JSON。
压缩请求保留规则处理后的模型、输入和 instructions，使用独立的 compact 字段配置，
不发送普通生成的 stream、store、tools、tool_choice、reasoning 和 include。
上游压缩结果的整个 `output` 窗口及 `encrypted_content` 原样保留；下一轮使用返回的
窗口加新消息作为完整 input，不使用网关包装响应的 ID 作为 `previous_response_id`。
压缩操作目前要求完整历史，带 `previous_response_id` 的请求会明确返回 400。
在线设置 → 模型与推理中提供“模拟压缩”和独立的“压缩模型”，默认开启模拟压缩、
默认模型为 `gpt-6-luna`；旧数据库未保存这些字段时也使用该默认值。模型可从本地缓存目录
选择或手动输入，保存后立即生效并持久化。三种模式：

- **开启（on，默认）**：直接使用所选普通模型生成续接摘要，不调用原生 compact 端点。
- **自动（auto）**：先调用原生 `/responses/compact`；端点返回 404 / 405 / 501，或 403 且
  明确提示 `This ChatPass credential is not authorized for the requested operation.` 时，
  使用同一账号和所选压缩模型生成续接摘要。模型被该账号或分组禁用时明确返回错误。
- **关闭（off）**：只调用原生端点，不进行模型摘要回退；仍可读取之前生成的网关摘要。

同一区域可编辑“压缩提示词”（管理 API 字段 `compaction_prompt`），保存后立即生效并持久化。
默认原样使用 [OpenAI Codex 的 compact/prompt.md](https://github.com/openai/codex/blob/a6baf8867cb4c9726213c0884a5c8b11f0cfd8bf/codex-rs/prompts/templates/compact/prompt.md)，
留空或点击“恢复 Codex 默认提示词”后保存即可恢复；自定义文本保留换行，最多 64 KiB。
提示词仅用于开启模式和自动模式的模型摘要回退，不改变原生 compact 请求。
与 Codex 的普通模型压缩分支一致，网关保留规则处理后的 `instructions`，
将压缩提示词追加为历史末尾的用户消息，通过普通 `/responses` 流式生成摘要，且不提供工具。

源码核对基于 Codex commit `a6baf8867cb4c9726213c0884a5c8b11f0cfd8bf`：
[prompts/src/compact.rs](https://github.com/openai/codex/blob/a6baf8867cb4c9726213c0884a5c8b11f0cfd8bf/codex-rs/prompts/src/compact.rs)
将该 Markdown 编译为 `SUMMARIZATION_PROMPT`；
[core/src/compact.rs](https://github.com/openai/codex/blob/a6baf8867cb4c9726213c0884a5c8b11f0cfd8bf/codex-rs/core/src/compact.rs)
在自动压缩时使用 `compact_prompt` 覆盖或上述默认值，并调用普通模型 `stream`；
[tasks/compact.rs](https://github.com/openai/codex/blob/a6baf8867cb4c9726213c0884a5c8b11f0cfd8bf/codex-rs/core/src/tasks/compact.rs)
在手动压缩的 `RemoteCompactionSupport::Unsupported` 分支使用相同提示词。
这指的是模型摘要压缩；该版本的 `TokenBudget` 分支直接开启新上下文窗口，不执行模型摘要，
因此不能把所有不走远程 compact 的路径都视为使用此提示词。

开启模式在请求规则和账号选择前应用压缩模型，规则可继续改写或拒绝请求；模型禁用策略
始终生效。自动模式的原生请求保留客户端模型，仅在回退时改用所选压缩模型。
原生压缩需要当前上游端点和账号凭据支持；允许输入压缩摘要不等于授予该端点权限。
摘要调用不提供工具，不向客户端透出摘要生成过程，按实际压缩模型记录用量和费用。
自动模式遇到原生端点的 401、429、其他权限错误及服务故障时不会转为摘要；
429 仍遵守已配置的换号重试策略，摘要调用的重试继续使用摘要模型。
回退结果保留 `response.compaction` / `compaction` 的客户端结构，使用带
`pi_compact_v1:` 前缀的网关 AES-GCM 加密内容。下一轮在网关中校验并还原为历史摘要消息，
也支持再次压缩；原生压缩项仍原样透传。摘要绑定客户端 API Key ID，使用数据库中持久化的
内部密钥，因此可跨网关重启和账号轮换，但需要继续通过同一网关数据库和同一客户端 Key 使用，
不能直接提交给官方端点。备份数据库时须保留该内部密钥；网关不额外持久化摘要正文。
文字摘要可能遗漏细节，不能复现原生压缩的内部推理状态；模型摘要失败或为空时返回错误。

账号菜单提供“手动恢复”，将错误状态重置为正常，清除最近错误、连续失败和冷却状态，
允许重新尝试现有凭据。恢复不会启用已停用账号，也不会修改凭据、配额、模型限制或历史统计；
实际凭据过期或上游限制仍需刷新凭据、重新登录或等待额度恢复。操作记录进入管理审计。

## 流速与官方消耗

请求明细、统计趋势和汇总使用同一流速口径，参考
[codex2api](https://github.com/james-6-23/codex2api/blob/main/frontend/src/pages/Usage.tsx)：
输出 Token ÷ 输出阶段秒数。首字按首个非生命周期、非终态响应事件记录，包含
思考及输出结构帧，并与总耗时使用同一起点。首字缺失或无效、输出区间不足 20ms 时
改用总耗时；失败、取消、未完成和无输出请求不计流速。输出计数保留上游原值，包含
上游计入输出的思考 Token。旧记录没有保存更早的结构事件时间，无法补算新的首字。

使用记录页和账号额度详情提供“官方结算消耗”，读取 Codex 凭证对应的
`wham/analytics/daily-workspace-usage-counts`，以官方 UTC 日粒度展示今日、近 7/30/84 天
和自定义期间的 Credits、等价美元、输入/缓存/输出 Token，以及结算和同步状态。
这份数据包含未经本网关的客户端消耗，与本地按模型价格估算的请求费用分别显示。
美元参考 codex2api 按 25 credits/$ 换算，实际付款取决于套餐或协议，不表示发票金额。

启用账号首次同步回补近 84 天，此后每小时回补近 7 天，修正延迟结算并保留历史快照；
手动同步提供 30 秒防重复请求。同一官方工作区的同一用户重复绑定不会重复统计，
不同用户分别保存。上游返回缺失字段时显示未知，同步失败保留之前的成功数据。
当天数据可能延迟，实际可回溯深度和接口可用性由上游决定。

## 快速开始

### Docker Compose

在仓库根目录执行：

```bash
cp .env.example .env
```

编辑 `.env`，至少替换管理员密码，不要保留示例中的 `change-me`：

```dotenv
PI_GATEWAY_PORT=8317
PI_GATEWAY_ADMIN_PASSWORD=replace-with-a-long-random-password
TZ=Asia/Shanghai
```

构建并启动：

```bash
docker compose up -d --build
docker compose logs -f pi-gateway
```

管理界面默认访问地址：

```text
http://127.0.0.1:8317
```

默认用户名为 `admin`。管理员密码留空时，程序会生成密码并打印到服务端日志；部署时建议显式设置。

首次登录后：

1. 在“账号管理”中使用 ChatGPT 登录，或导入支持的 ChatGPT 凭据。
2. 按需配置账号代理、分组及允许使用的模型。
3. 在“API 密钥”中创建供客户端使用的网关 Key。
4. 将客户端的 Base URL 指向网关，并使用实际可用的模型 ID。

**凭据边界**：生成请求使用主 ChatGPT 凭据。可选的 Codex 关联凭据用于配额查询和模型目录同步，不会取代主账号凭据。手动导入目前需要可校验来源的
ChatGPT refresh token；仅有 access token 时使用登录流程。

**网络边界**：原生程序默认监听回环地址；Compose 中网关监听 `0.0.0.0`，默认端口映射可能对外开放。部署到服务器时应配置防火墙和
HTTPS 反向代理，不要直接暴露无 TLS 的管理入口。

### PostgreSQL / Redis 组合

仓库提供四种 Compose 组合：

```bash
# SQLite，不使用 Redis
docker compose up -d --build

# PostgreSQL
docker compose -f compose.yaml -f compose.postgres.yaml up -d --build

# SQLite + Redis
docker compose -f compose.yaml -f compose.redis.yaml up -d --build

# PostgreSQL + Redis
docker compose -f compose.yaml -f compose.postgres.yaml -f compose.redis.yaml up -d --build
```

启用 PostgreSQL 前，须在 `.env` 中设置 `POSTGRES_PASSWORD`。当前 Compose 将密码插入连接 URI，应使用 URI 安全字符。

这些组合使用命名卷保存数据，数据库和 Redis 端口默认不映射到宿主机。停止服务时：

```bash
docker compose down
```

使用组合文件启动的服务，停止、查看日志和升级时也应带上相同的 `-f` 参数。`down` 默认保留命名卷；**不要随意添加 `--volumes`
，它会删除对应数据卷**。

完整定义见 [compose.yaml](compose.yaml)、[compose.postgres.yaml](compose.postgres.yaml)、[compose.redis.yaml](compose.redis.yaml)
和 [.env.example](.env.example)。

### 从源码运行

工具链以 [go.mod](go.mod)、[Dockerfile](Dockerfile) 为准：当前使用 Go 1.26.0，容器和 CI 使用 Node.js 24 构建前端。

在仓库根目录执行：

```bash
cp config.example.yaml config.yaml
npm --prefix web ci
npm --prefix web run build
go run ./cmd/pi-gateway --config config.yaml
```

也可以先编译可执行文件：

```bash
go build -o pi-gateway ./cmd/pi-gateway
./pi-gateway --config config.yaml
```

Windows 下可将输出名改为 `pi-gateway.exe`。**先构建前端，再构建或启动 Go 程序**，否则无法保证嵌入最新管理界面。

## 客户端接入

通用 Responses 客户端的配置：

```text
Base URL: http://127.0.0.1:8317/v1
API Key:  在管理界面创建的网关 Key
```

先查询当前 Key 可以访问的模型：

```bash
export PI_GATEWAY_KEY='your-gateway-api-key'

curl http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer $PI_GATEWAY_KEY"
```

以下示例中的 `your-available-model` 需替换为实际可用模型 ID：

```bash
curl -N http://127.0.0.1:8317/v1/responses \
  -H "Authorization: Bearer $PI_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-available-model","input":"你好","stream":true}'
```

将 `stream` 改为 `false` 可获得聚合 JSON 响应。

主要端点：

| 端点                   | 用途                                                                 |
|----------------------|--------------------------------------------------------------------|
| `POST /v1/responses` | HTTP Responses 请求，支持 SSE 和聚合 JSON                                  |
| `POST /v1/chat/completions` | Chat Completions 兼容请求，支持 SSE 和非流式 JSON                           |
| `GET /v1/responses`  | WebSocket Upgrade，支持 `response.create` 和活动响应的 `response.interrupt` |
| `GET /v1/models`     | 当前 Key 可访问的模型目录                                                    |
| `GET /healthz`       | 健康检查                                                               |

网关还提供 `/backend-api/codex/responses`、`/codex/responses` 的 HTTP / WebSocket 兼容别名，以及 HTTP `POST /responses`
。根据客户端实际追加的路径选择 Base URL，避免重复拼接 `/v1` 或 `/responses`。这些别名**不会改变上游凭据类型或生成端点**。

Codex 多代理兼容默认开启。`upstream.codex_history_recovery` 默认启用旧加密历史自动恢复：
优先使用同一 API Key、显式会话标识、工具调用 ID 和接收方对应的短期明文缓存；
缓存仅驻留当前进程，最多 2048 条 / 16 MiB，单条最多 64 KiB，4 小时有效，重启即清空。
没有显式会话标识时不跨请求缓存。无缓存的已配对历史调用保留原有结果，仅将密文消息标记为
“历史内容不可用”；加密代理消息保留可见文本并增加相同提示，不会伪装成已解密。
未完成且无法恢复的加密调用、格式错误以及新生成的加密调用仍拒绝；不会重放历史操作，
也不会修改 reasoning / compaction 密文。恢复次数写入捕获记录的协议追踪，不新增正文日志。
可设置 `PI_GATEWAY_UPSTREAM_CODEX_HISTORY_RECOVERY=false` 回退到原来的严格拒绝行为。

仅支持 Chat Completions 的客户端也可使用相同 Base URL 和网关 Key：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $PI_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-available-model","messages":[{"role":"user","content":"你好"}]}'
```

此入口默认返回非流式 `chat.completion`；设置 `stream:true` 时实时返回
`chat.completion.chunk`，以 `data: [DONE]` 结束。`stream_options.include_usage:true`
会在结束前增加 `choices:[]` 的用量块（上游提供用量时）。

适配支持 system/developer/user/assistant 消息、用户图片 URL 或 data URL、自定义 function 工具及其
调用历史和结果、`tool_choice`、`response_format` 和 `reasoning_effort`。内部将 `messages` 转换为
Responses `input`，再把文本、拒绝信息、工具调用及 Token 用量转回 Chat Completions 格式；
鉴权、账号调度、规则、429 重试和用量记录复用原有流程，传输记录保留原始客户端请求。
规则处理的请求及响应事件仍为 Responses 格式，`client_body` 保留客户端的 Chat 请求。

这是网关兼容层，上游仍调用 SiWC `/responses`，不会向上游请求 `/chat/completions`，
也不增加账号权限。仅支持单候选（`n=1`）；音频、旧版 `functions/function_call`、
`stop`、`seed`、`logprobs`、惩罚项等未适配参数明确返回 400。
`max_tokens` / `max_completion_tokens` 映射到 `max_output_tokens` 后仍由现有 SiWC 限制移除；
`temperature`、`top_p` 等同样沿用原有过滤，不保证生效。结构化输出和图片能力仍取决于模型。

上游协议由账号设置优先决定，其次使用 Key 或全局配置；下游使用 WebSocket 不代表上游也必须使用 WebSocket。`passthrough`
模式才会按客户端协议选择上游。

Codex 的 `response.interrupt` 控制帧会即时发送到活动响应占用的原上游 WebSocket，
要求 `response_id` 匹配且 `mode` 为 `discard_partial_items`；重复中断只发送一次。
上游确认 `response.incomplete` 且原因为 `interrupted` 后可继续使用原连接续接。
实际上游为 HTTP/SSE 时，中断控制帧返回 400 且不影响当前生成；关闭客户端连接可取消请求。
下游 SSE 原样转发上游中断终态，不追加流截断错误。

## 配置

完整配置和参数说明见 [config.example.yaml](config.example.yaml)。配置文件缺失时使用内置默认值；受支持的环境变量覆盖对应
YAML 配置，`--host`、`--port` 可覆盖监听设置。

主要配置项：

| 配置项                           | 对应环境变量                                              | 说明                              |
|-------------------------------|-----------------------------------------------------|---------------------------------|
| `server.host` / `server.port` | `PI_GATEWAY_HOST` / `PI_GATEWAY_PORT`               | 监听地址与端口                         |
| `admin.password`              | `PI_GATEWAY_ADMIN_PASSWORD`                         | 管理员密码，不是客户端 API Key             |
| `data.driver`                 | `PI_GATEWAY_DATABASE_DRIVER`                        | `sqlite` 或 `postgres`           |
| `data.database`               | `PI_GATEWAY_DATABASE`                               | SQLite 文件路径                     |
| `data.dsn`                    | `PI_GATEWAY_DATABASE_DSN`                           | 数据库连接字符串；SQLite 非空 DSN 优先于文件路径  |
| `redis.enabled` / `redis.url` | `PI_GATEWAY_REDIS_ENABLED` / `PI_GATEWAY_REDIS_URL` | 是否启用 Redis 及其连接地址               |
| `upstream.base_url`           | `PI_GATEWAY_UPSTREAM_BASE_URL`                      | 生成请求的上游 Base URL                |
| `upstream.transport`          | `PI_GATEWAY_UPSTREAM_TRANSPORT`                     | 默认上游传输模式                        |
| `models.codex_base_url`       | `PI_GATEWAY_CODEX_BASE_URL`                         | 可选 Codex 模型目录端点                 |
| `models.codex_client_version` | `PI_GATEWAY_CODEX_CLIENT_VERSION`                   | Codex 目录请求版本；`auto` 自动解析，也可固定版本 |
| `models.refresh_interval_seconds` | `PI_GATEWAY_MODELS_REFRESH_INTERVAL_SECONDS` | 模型目录自动同步间隔，默认 3600 秒；0 禁用，启用时至少 30 秒 |
| `logging.level`               | `PI_GATEWAY_LOG_LEVEL`                              | 日志级别                            |

`upstream.transport` 支持 `sse`、`websocket`、`websocket-cached`、`auto` 和 `passthrough`。模型、路由策略、捕获等运行时选项也可在管理界面调整。

### Compose 配置注意事项

- `.env` 用于 Compose 变量替换，并不自动把所有变量传入容器，也不会自动导出到宿主机的测试进程。
- 如需传入 Codex 目录等额外环境变量，应显式添加到 `pi-gateway.environment`。
- 默认镜像使用构建时复制的配置；仓库根目录新建的 `config.yaml` **不会自动挂载到容器**。
- 如需自定义 YAML，可在网关服务的 `volumes` 中增加以下挂载，同时保留原数据卷：

```yaml
- ./config.yaml:/app/config.yaml:ro
```

### 存储与会话边界

- SQLite 默认数据库位于 `data/pi-gateway.db`；容器内位于 `/app/data/pi-gateway.db`。
- Redis 保存有界元数据，不保存对话正文、请求 / 响应载荷或账号凭据。
- 活跃 WebSocket 连接和续接基线仍属于本地进程；Redis 无法恢复已断开的上游连接。
- 进程重启或续接状态丢失后，应携带完整上下文开始新链路，而不是假设旧 `previous_response_id` 一直有效。
- 切换数据库驱动不会自动搬迁已有业务数据；升级或迁移前先备份数据库和配置。

## 统一规则引擎

进入管理界面的“规则”页面（`/rules`）。旧 `/middlewares` 页面会重定向到此处。

每条规则包含“**当条件成立 → 按顺序执行动作**”，支持同类型的多条独立规则和同一规则内的多种动作。规则名称可重复，规则与动作分别具有稳定
ID。

### 执行阶段

| 阶段               | 执行位置                               |
|------------------|------------------------------------|
| `request`        | 请求规范化后、账号选择前，可修改模型和请求内容或拒绝请求       |
| `response_event` | 原始上游事件观测后、交付客户端前，可改写事件副本或丢弃允许丢弃的事件 |
| `response_body`  | 仅 `stream:false`，最终响应 JSON 写出前     |

修改流式 delta **不会自动修改终态快照**。需要调整非流式最终 JSON 时，应配置 `response_body` 规则。

### 条件与动作

当前注册 **21 种条件**：

- 分组：`always`、`all`、`any`、`not`。
- 比较与存在性：`eq`、`ne`、`exists`、`not_exists`。
- 内容与集合：`contains`、`not_contains`、`starts_with`、`ends_with`、`in`、`not_in`。
- 正则、数值与类型：`regex`、`not_regex`、`gt`、`gte`、`lt`、`lte`、`type`。

当前注册 **15 类动作**：

| 动作                              | 用途               |
|---------------------------------|------------------|
| `rewrite_model`                 | 修改模型，并同步路由使用的模型  |
| `drop_environment_context`      | 清理环境上下文          |
| `drop_input_items`              | 按类型或正则删除输入项      |
| `drop_tools`                    | 删除指定工具或全部工具      |
| `passthrough_fields`            | 从原始客户端 JSON 补入字段 |
| `set_reasoning`                 | 设置推理参数           |
| `json_set` / `json_remove`      | 设置或删除 JSON 值     |
| `json_merge`                    | 浅层或深层合并对象        |
| `json_transfer`                 | 复制或移动 JSON 值     |
| `array_insert` / `array_filter` | 插入或过滤数组元素        |
| `text_replace`                  | 字面量或正则文本替换       |
| `reject_request`                | 拒绝请求             |
| `drop_event`                    | 丢弃允许丢弃的非终态响应事件   |

不同动作有阶段限制，以页面内帮助和服务端 schema 为准。条件、动作、参数、递归对象 / 数组及上下文引用均有图形控件，不需要依赖任意脚本执行。

### 默认规则

**首次安装默认已包含并启用环境上下文清理。** 默认规则配置如下：

| 规则名称                       | 启用 | 优先级 |
|----------------------------|----|-----|
| `drop_environment_context` | 是  | 10  |
| `drop_fields`              | 是  | 20  |
| `drop_input_items`         | 否  | 30  |
| `drop_tools`               | 否  | 40  |
| `rewrite_model`            | 否  | 50  |
| `set_reasoning`            | 否  | 60  |
| `passthrough_fields`       | 否  | 70  |
| `block_prompt`             | 否  | 80  |

这些默认规则位于 `request` 阶段。`drop_environment_context` 默认处理 `environments.environment_context`，并开启
`also_strip_from_instructions`。旧名称保留为迁移来源，例如 `drop_fields` 对应新规则中的 `json_remove` 动作，而不是新的动作类型。

已有安装迁移时保留原来的启停和顺序，不会强制恢复首次安装的默认状态。规则全部删除后也不会在重启时自动重新生成。

### JSON 示例：模型匹配后执行两个动作

以下内容可以粘贴到“JSON 代码”模式。请将模型名称替换为实际可用的模型 ID；模型重写不会绕过账号或 Key 的访问限制。

```json
{
  "schema_version": 1,
  "name": "模型映射与推理参数",
  "description": "匹配客户端别名，再改写模型和推理强度",
  "enabled": true,
  "priority": 100,
  "phase": "request",
  "when": {
    "op": "eq",
    "source": "context",
    "path": "/model",
    "value": "client-model-alias"
  },
  "actions": [
    {
      "id": "rewrite-model",
      "type": "rewrite_model",
      "params": {
        "model": "your-available-model"
      }
    },
    {
      "id": "set-reasoning",
      "type": "set_reasoning",
      "params": {
        "effort": "low"
      }
    }
  ],
  "stop_after_match": false,
  "on_error": "abort"
}
```

可在图形模式继续编辑，再使用“服务端校验”和“试运行”。试运行只在内存执行，不访问上游、不选取账号、不产生用量或业务传输记录。

### 语义与边界

- **顺序**：每个阶段内，优先级数值越小越先执行；相同优先级采用持久化顺序和 ID 确定顺序。动作按列表顺序执行。
- **停止与回滚**：`stop_after_match` 在规则成功执行后停止当前阶段；`on_error: abort` 中止，`skip_rule` 原子回滚本条规则的全部动作再继续。
- **版本一致性**：请求开始时固定规则集版本，在途响应流不会混用后续发布的新规则。
- **数据来源**：`current` 是当前载荷，`client` 是只读原始客户端 JSON，`context` 是请求事实；`item`
  仅在数组过滤谓词内可用。请求阶段不能读取尚未选定的账号或上游响应事实。
- **路径**：使用严格 JSON Pointer，空字符串表示根节点，`~0` 表示 `~`，`~1` 表示 `/`。缺失值与显式 `null` 不同。
- **值表达式**：`{"$ref":{"source":"context","path":"/model"}}` 表示运行时引用；含表达式保留键的字面量可用
  `{"$literal": ...}` 包装。
- **正则**：使用 Go 正则语义，支持 `case_insensitive`、`dot_all`、`multiline`；不执行 JavaScript，也不以浏览器正则结果代替服务端校验。
- **严格校验**：未知字段、非法版本、无效路径 / 正则及不安全整数字面量会被拒绝，不静默丢失参数。
- **资源限制**：不设置规则总数或同类实例数量的产品上限，但单条规则复杂度、载荷和追踪记录有显式预算；管理分页不限制实际执行的规则数量。
- **保护边界**：不能改写鉴权凭据、协议必需身份、工具调用关联或上游计量；不能丢弃终态或错误事件。网关仍会执行独立的 Pi
  形状修复，并将其标记为网关处理而非规则修改。

### 管理 API 与迁移

规则 API 位于 `/api/rules`，使用管理员会话鉴权，与客户端网关 Key 不同：

- 列表 / 创建：`GET/POST /api/rules`。
- 查询 / 更新 / 删除：`GET/PUT/DELETE /api/rules/{id}`。
- 复制、调序、批量启停：`/{id}/duplicate`、`/reorder`、`/batch`。
- 能力定义、校验、试运行：`GET /schema`、`POST /validate`、`POST /simulate`。

现有规则写入需要预期修订号；冲突返回 `409`，不会覆盖其他人的修改。页面保留各规则的未保存草稿，并提供冲突处理。

旧中间件配置采用一次性、事务式迁移，保留旧配置用于诊断。无法保持语义时，迁移明确失败并保留旧执行链；规则页可进入兼容修复入口。迁移规则已变成旧接口无法表达的多动作结构时，旧
`/api/middlewares` 更新会拒绝覆盖。

升级前请备份。新规则不能无损降级为旧中间件；回滚应同时恢复升级前的程序、数据库备份和对应配置。

## 传输记录与安全

“传输记录”可查看客户端原始请求、网关实际发送内容、上游响应、实际客户端输出，以及规则逐步修改的前后差异。

- 规则来源记录当时的规则 ID、名称、修订号、优先级、动作和阶段，后续改名或删除不会通过当前配置猜测历史来源。
- 未命中、无实际变化、回滚、失败、阻断和真实修改分别显示。
- 响应帧优先通过显式事件关联 ID 配对；历史记录或不完整记录会提示缺失，不能可靠归因时不会猜测。
- 规则在选账号前阻断的请求可记录为“未分配账号”，不伪造上游流量或用量记录。
- `capture.enabled`、`capture.persist`、每账号条数和每条字节预算控制捕获；追踪截断不影响规则执行。

**注意：传输捕获可能包含提示词、业务数据和模型输出。** 已知敏感头和追踪字段的处理不能替代业务级脱敏。请限制管理员权限、谨慎配置保留范围，并保护数据库、配置、日志及备份。

## 开发与测试

### 本地开发

先安装依赖并构建一次前端，然后在两个终端分别运行：

```bash
# 终端一：Go 后端，默认 8317
go run ./cmd/pi-gateway --config config.yaml
```

```bash
# 终端二：Vite，默认 5273
npm --prefix web run dev
```

Vite 会将 `/api`、`/v1`、`/healthz` 代理到本机 `8317`；修改后端端口时同步调整 [web/vite.config.ts](web/vite.config.ts)。

### 自动化检查

日常 push（`master` / `main`）和 PR 的 CI 并行执行前端类型、契约检查与构建，以及无 race 的 Go 测试和静态检查，目标约 3 分钟；不安装浏览器，不启动数据库容器，也不重复构建 Docker 镜像。

`master` / `main` 的 push 通过 CI 后，会自动触发 **Publish container image**，构建刚通过检查的提交并推送到 `ghcr.io/lanqin996/pi-gateway`；默认分支更新 `latest` / `edge` 标签。镜像发布单独计时，不再重复执行一遍 CI。

完整的浏览器、`-race`、真实 PostgreSQL / Redis 与 Compose 回归保留在 Actions 的 **Full regression**，按需手动运行。版本标签发布会自动运行完整回归；手动发布也可勾选 `full_regression`。
镜像仅构建 `linux/amd64`，在原生 x86 runner 上执行，不使用 QEMU；适用于 `uname -m` 显示 `x86_64` 的 Intel / AMD 服务器。

从仓库根目录执行：

```bash
go test ./...
go vet ./...
go test -race ./...

npm --prefix web run typecheck
npm --prefix web run check:i18n
npm --prefix web run check:capture
npm --prefix web run check:rules
npm --prefix web run build
```

`-race` 需要平台支持及相应 C 工具链。`check:rules` 会调用 Go 测试导出真实后端 Catalog，因此执行前端规则门禁的环境也需要
Go。

规则门禁检查 schema、字段、枚举、双语帮助、真实 Vue 控件交互、图形 / 代码往返及 API 契约；传输门禁检查差异、事件配对和规则归因。协议回归包含
SSE / WebSocket 组合、缓存连接、自动回退、阻断、回滚及在途规则版本固定。

### 真实 PostgreSQL / Redis 集成测试

使用**独立、可丢弃的测试服务**，不要指向生产数据库。示例变量中的连接信息须按本机测试环境替换：

```bash
export PI_GATEWAY_TEST_POSTGRES_DSN='postgres://test_user:test_password@127.0.0.1:5432/pi_gateway_test?sslmode=disable'
export PI_GATEWAY_TEST_REDIS_URL='redis://127.0.0.1:6379/15'
go test -race -count=1 ./...
```

未设置变量时，依赖真实服务的测试会跳过；**跳过不代表实库验证通过**。默认 Compose 不开放这些服务的宿主机端口，也不会把 `.env`
自动导出给测试进程，需要单独准备连接条件。

[完整回归工作流](.github/workflows/full-ci.yml) 提供临时 PostgreSQL / Redis 服务，并要求指定集成测试实际运行而不能跳过。

## 项目结构

| 目录                                                        | 内容                               |
|-----------------------------------------------------------|----------------------------------|
| [cmd/pi-gateway](cmd/pi-gateway/main.go)                  | 程序入口、启动配置与迁移                     |
| [internal/api](internal/api/server.go)                    | 客户端 API、HTTP / WebSocket 转发与规则接入 |
| [internal/admin](internal/admin/server.go)                | 管理 API 与鉴权                       |
| [internal/rules](internal/rules/model.go)                 | 规则契约、校验、条件、动作和执行引擎               |
| [internal/rulesruntime](internal/rulesruntime/runtime.go) | 规则存储、迁移与编译快照之间的衔接                |
| [internal/store](internal/store/db.go)                    | SQLite / PostgreSQL 持久化          |
| [internal/capture](internal/capture/capture.go)           | 传输捕获、脱敏与规则追踪                     |
| [internal/webui](internal/webui/webui.go)                 | 嵌入式管理界面服务                        |
| [web](web/package.json)                                   | Vue 管理界面及前端检查脚本                  |

## 常见问题

**为什么没有可用账号或模型？**  
先添加并启用主 ChatGPT 账号，再检查代理、分组、Key 范围和模型限制。单独关联 Codex 凭据不能作为主账号提供生成能力。

**环境上下文还需要手动添加清理规则吗？**  
首次安装不需要：默认 `drop_environment_context` 已启用。升级安装请检查迁移后的状态，原先停用的规则不会被强制启用。

**为什么响应文本改了，最终 JSON 还没改？**  
`response_event` 和 `response_body` 是不同阶段；delta、终态快照和非流式输出需要按实际路径分别处理。

**修改规则后，正在进行的请求为什么没变化？**  
这是版本快照机制：新请求使用新版本，在途请求继续使用开始时固定的版本。

**为什么改了前端源码，二进制界面还是旧的？**  
先重新构建前端，再重新编译或启动 Go 程序。前端产物在编译时嵌入，已运行的可执行文件不会自动替换它。

**接上 Redis 后可以直接开多个网关实例吗？**  
不可以据此假设已支持多活。当前连接池、续接基线和部分准入状态仍由本地进程持有，应按单个活动实例部署。
