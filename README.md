# AI Proxy

面向 AI 模型供应商的透明代理转发程序。把客户端的 `base_url` 指过来，请求原样转发到上游，
同时在本地记录每次调用的完整指标：输入 / 缓存 / 输出 token、首 token 耗时、总耗时、TPS、
HTTP 状态、推理强度等。编译产物是单个可执行文件，管理界面已内嵌其中。

## 特性

- **透明转发**：不改格式、不裁字段。请求与响应按原字节透传，SSE 流式逐事件转发。
- **全程记账**：解析 5 种协议格式的用量与性能指标，落 SQLite，带实时推送的管理界面。
- **自定义提示词**：全局默认 + 按供应商覆盖 + 按模型正则匹配，三种注入方式，改动全程留痕。
- **全局代理**：支持 HTTP / SOCKS5，可按供应商覆盖或直连，支持 no_proxy 例外表。
- **多供应商**：一个端口挂多个上游，前端切换「当前生效」，也可用路径前缀显式指定。
- **多密钥轮询**：一个供应商配多个密钥，自动轮询，失败自动进入冷却并避让。

## 快速开始

### 1. 构建

需要 Go 1.25+ 与 Node 18+。

```bash
# Linux / macOS / Git Bash
make release

# Windows 命令行（cmd 或 PowerShell）
build.bat
```

`make release` / `build.bat` 会先构建前端（`web/dist`），再把它嵌进 Go 二进制，
产出 `ai_proxy.exe`（约 18 MB）。

也可以分两步手动执行：

```bash
cd web && npm install && npm run build && cd ..
go build -o ai_proxy.exe .
```

> 程序正在运行时也可以重新构建（Windows 允许替换已映射的可执行文件），
> 但**旧进程跑的仍是旧代码，要重启才生效**。`build.bat` 会提醒这一点。
>
> `build.bat` 刻意只使用 ASCII 字符：cmd.exe 按控制台的 OEM 代码页（简体中文 Windows
> 上是 936/GBK）逐字节读取 `.bat`，不认 UTF-8，写中文注释会让脚本被解析成乱码命令。
> 同理，脚本里的外部命令用绝对路径调用 —— 从 Git Bash 启动时 `find` 会解析到 MSYS 的
> GNU find 而不是 Windows 的。

### 2. 运行

```bash
./ai_proxy.exe
```

打开 <http://127.0.0.1:8080/> 进入管理界面。

命令行参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-host` | 设置里的值（127.0.0.1） | 监听地址 |
| `-port` | 设置里的值（8080） | 监听端口，优先级高于界面设置 |
| `-data` | `data` | 数据目录，存放 SQLite 数据库 |
| `-debug` | `false` | 输出调试日志 |

### 3. 配置供应商

在「供应商」页新增一个上游，至少填两项：

- **BaseURL**：例如 `https://api.openai.com/v1`。填不填 `/v1` 都可以，重复的路径段会自动去重。
- **API 格式**：决定去哪儿读用量、提示词注入到哪个字段。程序不会做格式转换。

密钥可以不填 —— 那时客户端自带的认证信息会原样转发给上游，适合「客户端持钥、代理只记账」的用法。

### 4. 接入客户端

```
固定入口（走当前生效供应商）  http://127.0.0.1:8080
指定供应商                    http://127.0.0.1:8080/p/<供应商短名>
```

一个端口同时支持所有路径，所以不同客户端可以指向同一个地址：

```bash
# OpenAI SDK
export OPENAI_BASE_URL=http://127.0.0.1:8080/v1
export OPENAI_API_KEY=sk-anything        # 密钥由代理侧注入

# Claude Code
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_API_KEY=sk-anything
```

同机多个客户端想各走各的供应商时，用短名路由：

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8080/p/openai-主力/v1
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080/p/claude-备用
```

### 5. 设置了访问密钥之后

监听 `0.0.0.0` 时建议在「设置 → 服务」里配置访问密钥，否则同网段的任何人都能用这个代理、
看全部请求日志。

**浏览器访问**：直接打开地址即可。管理界面的静态资源不要求密钥，页面会弹出一个输入框，
把密钥填进去就行 —— 存在浏览器本地，之后不用再输。

**脚本 / 客户端访问**：代理入口与管理接口都要求密钥，三种送法任选：

```bash
# 请求头（推荐）
curl -H "X-AI-Proxy-Key: <密钥>" http://127.0.0.1:8080/api/settings

# Authorization（把代理密钥当成 API key 用，客户端接入最省事）
export OPENAI_API_KEY=<密钥>

# 查询参数（仅用于 EventSource 这类无法自定义请求头的场景）
http://127.0.0.1:8080/api/events?key=<密钥>
```

客户端用 `Authorization` 送过来的代理密钥**不会被转发到上游**：那是给本代理看的凭据。
供应商自己没有配置密钥（「客户端持钥」模式）时，这一点尤其重要，否则会把代理密钥泄露给上游。

## 不消耗真实额度地试一遍

仓库带了一个 OpenAI / Anthropic / Gemini 三格式的模拟上游：

```bash
node scripts/mock-upstream.mjs 9911
```

然后在管理界面新增供应商：BaseURL 填 `http://127.0.0.1:9911/v1`，API 格式选
「OpenAI Chat Completions」，密钥随便填。模型名用 `mock-model` 会得到一次普通流式响应，
用 `mock-slow` 会得到一次约 17 秒的慢速响应 —— 打开「实时监控」页就能看到 token 与 TPS 实时跳动。

## 统计口径

| 指标 | 定义 |
|---|---|
| 输入 token | 输入总量，**含**缓存命中与缓存写入的部分（各家原始语义不同，这里已统一） |
| 缓存 token | 输入中命中缓存的部分 |
| 缓存命中率 | `缓存 token ÷ 输入 token`。因为输入总量已含缓存部分，比值天然落在 0–100% |
| 缓存写入 token | 写入缓存的输入量（目前只有 Anthropic 报这个值） |
| 输出 token | 模型生成的 token 数，含思维链 |
| 首 token | 从发出请求到收到第一个生成 token。对推理模型来说，第一个推理 token 也算 |
| 总耗时 | 从收到客户端请求到响应体读完 |
| TPS | `输出 token ÷ (总耗时 − 首 token)`，即模型吐字速度。**仅流式响应计算** |
| 成功 | HTTP 2xx **且** 无错误信息。流式响应中途被切断记为失败 |
| 估算值 | 上游没返回用量时的本地估算，日志里会明确标注 |

缓存命中率在总览指标卡、趋势图、模型排行、日志列表与日志详情里都会给出。
它直接反映提示词缓存是否在起作用 —— 命中率高意味着同样内容的重复请求更便宜、首 token 更快。

非流式响应不计算 TPS：没有逐 token 的到达时刻，用输出量除以总耗时得到的是延迟的倒数，
一个 4 ms 返回的短响应能算出几千 TPS，会把平均值完全带偏。

## 请求体改写

只有两件事会改动请求，各自独立开关，且都会在日志里留痕（保存改写前的原文供对比）：

1. **注入 `stream_options.include_usage`**（默认开启）
   OpenAI Chat Completions 格式的流式请求，上游默认不返回 token 用量。注入这个参数后
   末尾会多一个带 usage 的块，统计才是精确值。不开启则退化为本地估算。
   极少数严格客户端会被这个空 `choices` 的收尾块影响，可以在供应商的「高级」页开启
   「剥离 usage 块」——统计不受影响，只是不把它转发给客户端。

2. **注入提示词**
   全局默认 + 供应商覆盖 + 按模型正则规则，三种注入方式分别适配 OpenAI 的 `messages`、
   Anthropic 的顶层 `system`、Gemini 的 `systemInstruction`。
   任何一步解析失败都会放弃注入、原样转发，绝不把请求改坏。

## 代理配置

「设置 → 全局代理（VPN）」配置全局代理，对所有「代理模式 = 继承全局」的供应商生效。
单个供应商可在自己的「网络代理」页覆盖或选择直连。HTTP 代理走标准的 CONNECT 隧道，
SOCKS5 通过 `golang.org/x/net/proxy` 接管拨号。`no_proxy` 列表按域名后缀匹配，含子域。

连通性测试在供应商编辑页的「网络代理」标签下，会优先拉取上游 `/models`；
上游没有这个接口时退化为一次 `max_tokens=1` 的最小请求。测试请求可以带上尚未保存的配置，
省去「先存再测」的来回。

## 开发

```bash
# 后端测试
go test ./...

# 前端热更新（另开一个终端跑 go run . -data data）
cd web && npm run dev

# 类型检查
cd web && npm run typecheck

# 端到端验收：需要先启动模拟上游与本程序
node scripts/mock-upstream.mjs 9911 &
./ai_proxy.exe -data ./data -port 18080 &
node scripts/e2e-check.mjs
```

前端用 Vite + Vue 3 + TypeScript + Naive UI + ECharts。开发时 Vite 把 `/api` 代理到
`127.0.0.1:8080`，构建产物由 `go:embed` 打进二进制。

服务端把「能对应到嵌入文件的路径」返回静态资源，其余路径全部当作转发目标 ——
所以管理界面与代理入口共用一个端口，且任何上游路径（`/v1/...`、`/v1beta/...`）都无需预先注册。

## 目录结构

```
main.go                 入口、静态资源与转发的分发、日志清理后台任务
console_windows.go      把 Windows 控制台切到 UTF-8，避免中文日志乱码
internal/
  model/                全局数据结构
  store/                SQLite 持久化：供应商、设置、请求日志、聚合统计
  proxy/                转发核心
    server.go             主流程：路由、URL 拼接、头部处理、响应回写、记账
    analyzer.go           SSE 组帧 + 旁路解析（用量、模型、错误、首 token）
    usage.go              五种格式的用量提取
    rewrite.go            请求体改写：usage 注入 + 提示词注入
    upstream.go           上游连接：代理、超时、TLS、多密钥轮询与冷却
    probe.go              连通性测试与模型列表
    live.go               进行中请求注册表
  api/                  管理接口与 SSE 推送
  hub/                  进程内事件广播
web/                    前端源码（构建产物 web/dist 被 embed）
scripts/mock-upstream.mjs  本地模拟上游
scripts/e2e-check.mjs      端到端验收脚本
```

## 已知边界

- 只做转发与记账，**不在协议格式之间转换**。上游是什么格式，客户端就得用什么格式。
- 主动去掉转发请求里的 `Accept-Encoding`：压缩后的响应无法旁路解析用量，而用量统计是本程序的
  核心功能。上游因此返回未压缩内容，客户端也会收到未压缩内容。
- 请求体需要完整读入内存才能改写与记账，硬上限 128 MB。
- 报文默认各截断到 64 KB 后落库（可在设置里调整）；关掉报文存储后统计指标仍然完整。
- 数据库用 `modernc.org/sqlite` 纯 Go 驱动，无 CGO 依赖，但也因此连接数限制为 1，
  极高并发写入场景下可能成为瓶颈。
