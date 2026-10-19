# Cline Go Proxy

Cline API 的反向代理服务，支持多账号轮询、OpenAI 和 Anthropic Messages API 双协议、API Key 鉴权，内置中文管理后台。

## 功能

- **双协议兼容**：同时支持 `/v1/chat/completions`（OpenAI）和 `/v1/messages`（Anthropic Messages API）
- **多账号轮询**：自动在多个 Cline 账号间切换负载（支持 `round_robin` / `fill` / `random` 策略）
- **中文管理后台**：浏览器访问 `/admin/` 即可管理账号、API Key、模型配置、请求头、轮询策略
- **管理后台鉴权**：公网部署可设置 `ADMIN_TOKEN`（或 `-admin-token` / 配置项 `adminToken`），访问后台需登录令牌
- **每账号出口代理**：可为每个账号配置独立 `http` / `https` / `socks5` 代理（可空=宿主机直连），OAuth 注册 / Token 刷新 / 推理请求均走该代理，降低多账号同 IP 关联
- **API Key 鉴权**：保护代理端点，支持生成/删除多个 API Key
- **System Prompt 覆盖**：项目目录下放 `override.md` 则自动替换系统提示词，不存在则使用客户端自带
- **账号导入**：支持 OAuth 浏览器登录、手动 Token 输入、批量文件导入
- **持久化存储**：账号和 Key 保存在 `.cline-accounts.json`
- **账号冷却与自动恢复**：命中 429 `INFERENCE_CAP_ERROR` 时自动解析 "Try again in 17h 59m" 并标记冷却，冷却到期自动恢复，账号列表显示预计恢复时间
- **账号测试**：后台账号管理提供「⚡测试」按钮，对单个账号发起真实探测请求，验证是否可用；测试成功会清除冷却/过期状态（相当于升级版重置），失败会按上游返回的等待时长标记冷却
- **本地调用统计**：账号列表明确显示「本地今日/累计调用」，统计代理转发成功（上游 HTTP 200）的实际调用；跨日自动重置今日调用；「↻重置」按钮仅重置本地今日调用。该数字不是 Cline 官方免费额度，官方每日额度不公开精确次数。
- **多平台 CI/CD**：GitHub Actions 自动构建，推送代码到 main 即触发，生成 Windows (amd64/arm64)、Linux (amd64/arm64)、macOS (amd64/arm64) 共 6 个平台二进制，版本从 `v0.0.1` 开始，之后每次自动递增补丁版本

## 快速开始

### 直接运行

```bash
# 编译并启动（默认监听所有网卡，局域网可访问）
go build -o cline-proxy.exe .
./cline-proxy.exe

# 局域网访问地址：http://<本机局域网IP>:3457/admin/
# 仅允许本机访问时：
./cline-proxy.exe -host 127.0.0.1

# 指定端口
./cline-proxy.exe -port 3457

# 公网 / 不可信网络：启用管理后台令牌（三选一，优先级：CLI > 环境变量 > 配置文件）
./cline-proxy.exe -admin-token "your-strong-secret"
# 或
# set ADMIN_TOKEN=your-strong-secret          # Windows CMD
# $env:ADMIN_TOKEN="your-strong-secret"     # PowerShell
# export ADMIN_TOKEN=your-strong-secret     # Linux/macOS
# 或在 .cline-accounts.json 增加 "adminToken": "your-strong-secret"

# 构建 + 启动 + 打开浏览器
go run . -start
```

启动后本机访问 http://127.0.0.1:3457/admin/；局域网设备访问 http://<本机局域网IP>:3457/admin/。

监听所有网卡会开放管理后台给同网设备。**公网部署必须设置 `ADMIN_TOKEN`**，否则任何人都能进入 `/admin/` 管理账号与密钥。未设置令牌时保持本地开发便利（后台不鉴权）。

### 管理后台鉴权

| 注入方式 | 说明 |
|----------|------|
| 环境变量 `ADMIN_TOKEN` 或 `CLINE_ADMIN_TOKEN` | 推荐 Docker / 公网 |
| 启动参数 `-admin-token` | 优先级最高 |
| 配置文件 `data/.cline-accounts.json` 字段 `adminToken` | 持久化，GET 接口不会回显明文 |

启用后：

1. 打开 `/admin/` 会显示登录页，输入同一令牌即可
2. 浏览器会保存会话（`sessionStorage` / Cookie `cline_admin_token`）
3. 管理 API 也可使用请求头：`Authorization: Bearer <token>` 或 `X-Admin-Token: <token>`

> 注意：这是**管理后台**令牌，与「设置 → API Keys」里给客户端用的代理 API Key 不同。  
> **作用范围**：仅 `/admin/` 与 `/admin/api/*`。`/v1/chat/completions`、`/v1/messages`、`/v1/models`、`/health` 等客户端/Cline 调用**不校验** `ADMIN_TOKEN`，仍只走原有 API Key 逻辑。

### Docker 部署

持久化目录为项目下的 **`./data/`**（整目录挂载到容器 `/app/data`），内含：

| 文件 | 说明 |
|------|------|
| `data/.cline-accounts.json` | 账号池 + **API Keys** + 可选 `adminToken` |
| `data/override.md` | 可选 system prompt 覆盖 |
| `data/cline-proxy.log` | 运行日志 |

> **不要**再把单个 `.cline-accounts.json` 文件 bind-mount 进容器。宿主机文件不存在时，Docker 会把挂载点建成**目录**，导致账号/Key 无法保存。

```bash
# 1) 准备数据目录（首次）
mkdir -p data

# 若以前误把 .cline-accounts.json 建成了目录，先清掉再启动：
# rm -rf .cline-accounts.json          # Linux/macOS
# Remove-Item -Recurse -Force .cline-accounts.json   # PowerShell

# 若根目录还有旧的账号文件，迁到 data/：
# cp .cline-accounts.json data/.cline-accounts.json
# cp override.md data/override.md   # 可选

# 2) 公网务必传入 ADMIN_TOKEN
export ADMIN_TOKEN='your-strong-secret'   # Windows PowerShell: $env:ADMIN_TOKEN='...'
docker compose up -d --build

# 查看日志
docker compose logs -f

# 停止（保留 ./data）
docker compose down
```

本地直接运行时，默认也会使用（或创建）工作目录下的 `data/`；也可用环境变量覆盖：

```bash
export DATA_DIR=/path/to/data   # 或 CLINE_DATA_DIR
./cline-proxy
```

## 使用指南

### 1. 添加 Cline 账号

在管理后台 **账号管理** → **导入账号**，选择以下任一方式：

- **OAuth 浏览器登录**：可先填「出口代理」，再点开始；服务端 device 申请 / 轮询 / register 走该代理。复制返回的链接到**远程指纹浏览器**（或同出口环境）登录即可，本机进程会自动完成入库。
- **手动输入 Token**：输入 refreshToken，可选邮箱与 `proxyURL`
- **批量文件导入**：JSON 数组，每项可带 `proxyURL`

```json
[
  {"refreshToken":"xxx","email":"a@x.com","proxyURL":"socks5://user:pass@1.2.3.4:1080"},
  {"refreshToken":"yyy","email":"b@x.com","proxyURL":"http://127.0.0.1:7890"}
]
```

账号列表操作列说明：

- **⚡ 测试**：对账号发起一次真实探测请求（走该账号代理）。成功则将账号置为活跃；失败则按上游等待时长标记冷却。
- **🌐 代理**：编辑该账号出口代理；留空表示宿主机直连。
- **↻ 重置**：仅重置该账号的「今日调用」，不影响累计调用、状态和 Token。
- **✕ 删除**：从账号池移除该账号。

#### 每账号出口代理说明

| 场景 | 是否走账号代理 |
|------|----------------|
| OAuth device / 轮询 / Cline register | 是（导入时填写的 proxy） |
| Token refresh、chat、账号测试 | 是 |
| 浏览器打开登录页 | **否**（由你打开链接的环境决定，建议远程指纹浏览器） |
| 全局模型列表同步 | 否（宿主机） |

支持格式：`socks5://[user:pass@]host:port`、`socks5h://...`、`http://...`、`https://...`。代理 URL 与 refreshToken 一并保存在 `.cline-accounts.json`（文件权限 0600）。

### 2. 配置客户端

应用（如 Claude Code、Cline）配置为使用此代理：

**OpenAI 格式（/v1/chat/completions）：**
```
Base URL: http://<本机局域网IP>:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    deepseek/deepseek-v4-flash
```

**Anthropic 格式（/v1/messages）：**
```
Base URL: http://<本机局域网IP>:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    deepseek/deepseek-v4-flash
```

### 3. API Key 管理

在后台 **设置** → **API Keys** 中生成和管理。如果未配置任何 Key，代理允许无鉴权访问。

### 4. System Prompt 覆盖

在项目目录下创建 `override.md`，内容将替换所有客户端请求的系统提示词。删除该文件则使用客户端自带的提示词。

### 5. 请求头配置

后台 **设置** → **请求头** 可编辑转发给上游的自定义请求头（如 `x-client-type: cline-cli`）。

## 可用模型（实测）

### 消耗账户额度

| 模型 ID | 状态 | 说明 |
|---------|:----:|------|
| `deepseek/deepseek-v4-pro` | ✅ 可用 · 消耗额度 | DeepSeek V4 Pro |
| `openai/gpt-4.1-nano` | ✅ 可用 · 消耗额度 | GPT-4.1 Nano |
| `qwen/qwen3-235b-a22b` | ✅ 可用 · 消耗额度 | Qwen3 235B |
| `meta-llama/llama-4-maverick` | ✅ 可用 · 消耗额度 | Llama 4 Maverick |
| `google/gemini-2.5-flash` | ⚠️ 响应为空 · 消耗额度 | API 返回 200 但内容为空 |
| `google/gemini-2.5-pro` | ⚠️ 响应为空 · 消耗额度 | API 返回 200 但内容为空 |

### 官方免费模型（限时促销）

> **重要**：官方免费模型是 Cline 的**限时促销**（官方文档明确"Free model usage is not supported through the Cline API"），
> 促销名单滚动变化、有每日限额，调用资格以网关对账号状态的判定为准。代理现在以官方
> `/ai/cline/recommended-models` 的 `free` 字段为**唯一事实来源做全量对账**：
> 同步成功后，不在 feed 中的模型自动下架（`status=removed`，从 `/v1/models` 与可选列表消失）；
> 实际调用收到 404/"promotion ended" 时即时下架，收到要求订阅/扣费类错误时把 `cost` 改标 `quota`（不再承诺免费）。
> seed 条目仅作离线冷启动兜底，同样参与对账。

| 模型 ID | 状态 | 说明 |
|---------|:----:|------|
| （以官方 feed 实时对账为准） | - | 后台「可用模型」面板显示当前对账结果与原因（note） |

> **注意**：日志里的 `max_tokens` 是**单次生成输出上限**，不是上下文窗口。  
> DeepSeek V4 Flash 上下文约 1M；若客户端未传 `max_tokens`，代理默认按模型 `maxOutputTokens`（约 131K）转发，不会把上下文“砍成 128K”。  
> 促销结束后模型可能继续出现在官方目录里（仍是付费模型），代理的下架逻辑可避免客户端选到这类必然 404/扣费的条目。

### 需要订阅

| 模型 ID | 状态 | 说明 |
|---------|:----:|------|
| `cline-pass/glm-5.2` | ❌ 403 · 需要订阅 | 需要 `cline-pass` 订阅 |
| `cline-pass/deepseek-v4-flash` | ❌ 403 · 需要订阅 | 需要 `cline-pass` 订阅 |
| `cline-pass/qwen3.7-max` | ❌ 403 · 需要订阅 | 需要 `cline-pass` 订阅 |

可在后台 **设置** → **默认模型** 中修改默认模型。

## CI/CD

Release 版本号以 `v` 开头，从 `v0.0.1` 开始按语义版本递增：首次发布为 `v0.0.1`，后续推送自动发布 `v0.0.2`、`v0.0.3` 等版本。

## 项目结构

```
├── main.go               入口与 CLI 参数处理
├── proxy.go              HTTP 服务、API 路由与协议转换
├── models.go             官方免费模型同步
├── admin.go              管理后台 REST API
├── admin_auth.go         管理后台访问令牌鉴权
├── admin_html.go         管理后台页面
├── auth.go               WorkOS OAuth 登录与 Token 刷新
├── pool.go               账号池管理与持久化
├── types.go              数据结构定义
├── capture.go            OAuth 信息捕获工具
├── http.go               HTTP 客户端与工具函数
├── Dockerfile            Docker 构建配置
├── docker-compose.yml    Docker Compose 配置
├── go.mod                Go 模块定义
├── data/                 持久化目录（Docker 挂载 ./data → /app/data）
│   ├── .cline-accounts.json  账号池 + API Keys
│   ├── override.md           可选系统提示词覆盖
│   └── cline-proxy.log       运行日志
└── README.md             项目说明
```


---

感谢 [LINUX DO](https://linux.do) 社区
