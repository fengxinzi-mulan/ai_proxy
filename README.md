# AI Proxy

面向 AI 模型供应商的透明代理转发程序。把客户端的 `base_url` 指过来，请求原样转发到上游，
同时在本地记录每次调用的完整指标：输入 / 缓存 / 输出 token、首 token 耗时、总耗时、TPS、
HTTP 状态、推理强度等。

提供两种形态，共用同一套内核与界面：

- **桌面版**（`ai_proxy-desktop.exe`）：原生窗口，双击即用（Windows）。
- **服务端版**（`ai_proxy-server.exe`）：命令行启动、监听端口，适合服务器与脚本部署。

两种形态都是单文件，前端资源通过 `go:embed` 内嵌，无需额外部署。

## 特性

- **透明转发**：不改格式、不裁字段。请求与响应按原字节透传，SSE 流式逐事件转发。
- **全程记账**：解析 5 种协议格式的用量与性能指标，落 SQLite，带实时推送的管理界面。
- **自定义提示词**：全局默认 + 按供应商覆盖 + 按模型正则匹配，三种注入方式，改动全程留痕。
- **模型映射**：按供应商把请求的模型名精确映射成上游模型名，未命中的请求原样透传（见下文）。
- **Responses 会话续传**：对开启的供应商，把多轮 Responses 请求的全量历史截成增量，配合
  `store:true` + `previous_response_id` 降低多图会话的首 token 耗时；默认关闭，支持 dry-run 诊断（见下文）。
- **全局代理**：支持 HTTP / SOCKS5，可按供应商覆盖或直连，支持 no_proxy 例外表。
- **多供应商**：一个端口挂多个上游，前端切换「当前生效」，也可用路径前缀显式指定。
- **多密钥轮询**：一个供应商配多个密钥，自动轮询，失败自动进入冷却并避让。
- **套餐余量查询**：按供应商选一个内置模板，读回剩余额度与滚动窗口限额（见下文）。

## 界面预览

管理界面由程序自己提供，和代理入口共用同一个端口。桌面版直接开窗显示它；
服务端版启动后打开 `http://127.0.0.1:8080/` 即可。两种形态看到的是同一套界面。

下面这些截图取自一台本机演示实例（上游指向本地模拟上游），不涉及任何真实账号与密钥。

**仪表盘** —— 请求量、成功率、缓存命中率、token、首 token 耗时与 TPS 的汇总，
下面按时间桶展开请求与 token 趋势、性能曲线、供应商占比和模型排行。

![仪表盘](docs/screenshots/dashboard.png)

**暗色主题** —— 亮/暗两套都完整适配（含图表与状态色），也可以跟随系统自动切换。

![仪表盘暗色](docs/screenshots/dashboard-dark.png)

**供应商** —— 一个端口挂多个上游：API 格式、密钥数量、代理方式、自定义标签都摆在卡片上，
当前生效的那张带蓝色左竖条与细描边；配了套餐查询的供应商还会显示剩余额度与各窗口用量。

![供应商](docs/screenshots/providers.png)

**请求日志** —— 每次调用的完整指标，可按时间、供应商、模型、状态筛选，或按关键字搜索；
首 token、总耗时、TPS 与缓存命中率都按档位着色。

![请求日志](docs/screenshots/logs.png)

**日志详情** —— 用量与性能的精确值，以及本次请求的改写留痕；改写前的请求体也留在库中，
可以在「请求」标签页里和实际发出的那份对照。

![日志详情](docs/screenshots/log-detail.png)

**实时监控** —— 正在转发的请求每 0.7 秒推送一次，已生成 token、实时 TPS 与首 token 等待状态都在上面。

![实时监控](docs/screenshots/live.png)

**设置** —— 监听地址与访问密钥、客户端接入示例、默认超时、日志保留策略；
另有「全局代理（VPN）」「全局提示词」两个标签页。桌面版还会多一个「桌面应用」标签页
（关闭行为、数据/日志目录、版本号）。

![设置](docs/screenshots/settings.png)

## 快速开始

### 1. 构建

需要 Go 1.25+ 与 Node 18+。

```bash
# Linux / macOS / Git Bash
make release

# Windows 命令行（cmd 或 PowerShell）
build.bat
```

`make release` / `build.bat` 会先构建前端（`web/dist`），再把它嵌进 Go 二进制。
`build.bat` 现在产出两份：

| 产物 | 说明 |
|---|---|
| `dist\ai_proxy-desktop.exe` | 桌面版，Windows GUI 子系统，带图标与版本信息 |
| `dist\ai_proxy-server.exe` | 服务端版，控制台子系统，行为与旧版一致 |

`make dist` 等价于 Windows 上的 `build.bat` 双产物构建；`make release` 只产出服务端形态。

桌面版的构建需要多一步 Windows 资源：

```bash
go run scripts/gen-icon/main.go                  # 生成 build/windows/app.ico
go run scripts/gen-rc/main.go -version 0.2.0     # 生成 build/windows/app.rc（含版本号）
cd build/windows && windres --target=pe-x86-64 -i app.rc -o ../../internal/winres/rsrc_windows_amd64.syso -O coff -c 65001
```

`build.bat` 会自动完成这三步，并自动定位 `windres`（优先 `D:\TDM-GCC-64\bin`）。
`-c 65001` 不能省：`app.rc` 里的中文是 UTF-8，按默认 ANSI 读会变成 exe 属性里的乱码。

两个脚本都不依赖 PATH 上的 `go`：它们直接调用版本目录里的高版本
`D:\Program Files\g\versions\1.25.4\bin\go.exe`，并把 `GOROOT` 指到同一目录
（只在脚本内生效）。这是必要的 —— `g` 的 `...\g\go` 指针和机器级 `GOROOT`
可能停在旧版本（本机是 1.19），直接用 PATH 上的 `go` 会拿错标准库。
想换版本就改脚本里的 `GO_HOME`（Makefile 里是 `GO_DIR`）。

也可以分两步手动执行（同样要用上面那个 go，或自行 export 匹配的 `GOROOT`）：

```bash
cd web && npm install && npm run build && cd ..
"D:\Program Files\g\versions\1.25.4\bin\go.exe" build -o ai_proxy.exe .
```

> 程序正在运行时也可以重新构建（Windows 允许替换已映射的可执行文件），
> 但**旧进程跑的仍是旧代码，要重启才生效**。`build.bat` 会提醒这一点。
>
> `build.bat` 刻意只使用 ASCII 字符：cmd.exe 按控制台的 OEM 代码页（简体中文 Windows
> 上是 936/GBK）逐字节读取 `.bat`，不认 UTF-8，写中文注释会让脚本被解析成乱码命令。
> 同理，脚本里的外部命令用绝对路径调用 —— 从 Git Bash 启动时 `find` 会解析到 MSYS 的
> GNU find 而不是 Windows 的。

#### 自动发布

推 `v*` 标签会触发 `.github/workflows/release.yml`，在 `windows-latest` 上构建两个 exe
并自动创建 GitHub Release：

```bash
git tag v0.2.1
git push origin v0.2.1
```

也可以在 Actions 页面手动运行 `Release` 工作流，填版本号并勾选 `publish` 来发版；
不勾选时只构建并保留 Actions artifact。Release 附件包含
`ai_proxy-server.exe`、`ai_proxy-desktop.exe` 与 `SHA256SUMS.txt`。

### 2. 运行

**桌面版**（推荐）：

```bash
dist\ai_proxy-desktop.exe
```

双击即可，不需要命令行参数，也不需要手动打开浏览器。

**服务端版**：

```bash
./ai_proxy.exe
```

打开 <http://127.0.0.1:8080/> 进入管理界面。

命令行参数（两种形态都接受，桌面版通常不需要）：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-host` | 设置里的值（127.0.0.1） | 监听地址 |
| `-port` | 设置里的值（8080） | 监听端口，优先级高于界面设置 |
| `-data` | 服务端 `data`；桌面版见下 | 数据目录，存放 SQLite 数据库 |
| `-debug` | `false` | 输出调试日志 |

#### 桌面版的工作方式

窗口加载的是进程内已经跑起来的本地 HTTP 服务（`http://127.0.0.1:<端口>/`），
不是另外打包的一套前端。所以**浏览器访问仍然可用**：桌面版运行期间，同机任何程序
都可以打开同一个地址，也能把它当作代理入口给客户端用。

几个与命令行版不同、需要知道的行为：

| 行为 | 说明 |
|---|---|
| 数据目录 | 优先沿用 exe 同目录的 `data\`（若其中已有 `ai_proxy.db`），否则用 `%APPDATA%\ai_proxy`。用 `-data` 可强制指定 |
| 日志 | 同时写 stdout 与 `<数据目录>\logs\ai_proxy.log`，5 MB 轮转、保留 5 份。GUI 子系统没有控制台，文件日志是排障主入口 |
| 关闭窗口 | 默认弹三选一：退出 / 托盘运行 / 取消，可勾选记住选择。之后可在「设置 → 桌面应用」修改 |
| 托盘运行 | 窗口隐藏到系统托盘，**代理继续运行**，客户端连接不中断；双击托盘图标重新打开，右键菜单可退出 |
| 单实例 | 重复启动只会把已有窗口叫到前台，不会开第二个进程 |
| 端口占用 | 启动时若端口被占，会询问是否自动换到下一个可用端口；界面上的接入地址始终显示实际端口 |
| WebView2 | 需要系统安装 Microsoft Edge WebView2 Runtime。Windows 11 与较新的 Windows 10 通常已自带 |

日志里出现 `未检测到 WebView2 Runtime` 时，装一次
<https://developer.microsoft.com/microsoft-edge/webview2/> 的 Evergreen Runtime 即可。

#### 桌面版的边界

- **暂只支持 Windows**。代码结构已按平台隔离（`internal/desktop` + `desktop_windows.go`），
  但 Linux 桌面壳尚未实现。Linux 上继续使用服务端版。
- **不做系统托盘**。「最小化」是隐藏到任务栏，不是收进托盘区。
- 关闭行为、数据目录、版本号都在「设置 → 桌面应用」里可见可改。

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

## 套餐余量查询

在供应商的「用量」页选一个**查询模板**，就能读到该账号的套餐余量：剩余额度、
滚动窗口限额（5 小时 / 本周 / 本月）、计费周期与周期内花费。供应商卡片上会显示一条紧凑的额度条。

刷新是**服务端**做的：按供应商在「定时刷新」里设的间隔，后台线程查一次并把结果写进
`provider_usage` 表（每个供应商一行，覆盖写，不留历史）。界面打开时直接读库里那份快照，
不用等四个上游接口跑完；卡片和抽屉上的「刷新用量」只是让服务端立刻再查一次，
走的还是同一条「查询 + 落库」的路径。间隔设为「关闭」就不会有任何后台查询。

内置 `commandcode` 模板，读的是 Command Code 账号额度接口：

| 接口 | 内容 |
|---|---|
| `GET /alpha/whoami` | 账号名与组织 id |
| `GET /alpha/billing/credits` | 月度/充值/赠送额度、5 小时与每周窗口的已用量与上限 |
| `GET /alpha/billing/subscriptions` | 计划、状态、计费周期起止 |
| `GET /alpha/usage/summary` | 周期内花费、请求数、成功率、输入/输出 token |

另外还有两个模板：

**DeepSeek**（官方公开接口 `GET /user/balance`）读取总余额、充值余额、赠金余额；
金额与币种（CNY/USD）跟着上游返回走 —— 人民币余额不会被当成美元显示。

**OpenCode Go**（`GET https://opencode.ai/zen/go/v1/usage`）读取 Go 订阅的三个配额窗口
（5 小时 / 每周 / 每月）的用量百分比与重置时间。两点要知道：它只对**已订阅 OpenCode Go**
的账号有效（Zen 的按量付费余额目前没有公开接口，官方 issue 仍开着）；上游只给百分比、
不给金额，所以界面上只显示百分比，**不会编一个美元数**出来。
该接口固定在站点根，模板按「站点根 + 绝对路径」请求，所以 BaseURL 填
`https://opencode.ai/zen/v1` 或 `https://opencode.ai/zen/go/v1` 都能命中。

几点需要知道：

- 查询地址默认从供应商的 BaseURL 推导：额度接口在 `<站点根>/alpha/...`，而生成接口在
  `<站点根>/provider/v1`，所以模板会把 `/provider/v1`、`/provider`、`/v1` 这类协议前缀剥掉一层。
  推导结果不对时可以在「用量」页手填。
- 界面展示三个窗口：**5 小时**和**本周**是上游直接给的，**本月**是推导的 ——
  上游只把月度做成「剩余额度」而不是窗口，所以本月的上限 = 本周期已花 + 月度剩余额度。
  这类推导出来的数字在界面上带一个 ⓘ 标记，悬浮可以看到它的算法。
  周期花费读不到时就不显示本月，宁可少一格也不用半截的数据凑一个比值。
- 这组接口**未公开**，是 Command Code 自己的 CLI `/usage` 面板在读的同一批；上游随时可能改结构或下线。
  因此读不到的段落会明确标为「不可用」并给出原因，**不会显示成 0** —— 把「读不到」渲染成
  「额度为 0」是最容易误导人的失败方式。原始响应保留在「原始响应」折叠区里，便于对照排查。
- **逐请求金额不在其中。** 上游的响应体只有 token 数，没有任何成本字段；账号级接口也只给
  聚合值（周期总花费、平均每次请求成本）。想在每条日志上算金额，只能本地按 token 数乘单价，
  那是估算而非账单，本项目没做。

新增模板：在 `internal/proxy/usagequery.go` 的 `usageTemplateRegistry` 里加一条，
写一个 `fetch` 函数把响应映射到 `model.UsageSnapshot` 即可，前端下拉框会自动出现它。

想确认上游接口是否还能用（未公开接口最容易悄悄改结构），可以跑一次实查 —— 只发 GET，不消耗额度：

```bash
COMMAND_CODE_API_KEY=user_xxx go test ./internal/proxy -run Live -v
DEEPSEEK_API_KEY=sk-xxx       go test ./internal/proxy -run LiveDeepSeek -v
OPENCODE_API_KEY=sk-xxx       go test ./internal/proxy -run LiveOpenCode -v
CONTINUATION_LIVE=1 CONTINUATION_PROXY_LIVE=1 RESPONSES_LIVE_MODEL=grok-4.7 \
  go test ./internal/proxy -run LiveOpenCodeResponsesContinuation -v
```

## 模型映射

在供应商的「模型映射」页配一组「请求模型 → 目标模型」的对应关系，客户端请求里的模型名
与左侧**完全相等**时，实际发给上游的模型名会换成右侧。

- **只做精确匹配**，不做前缀或正则。模型名是上游用来选模型和计费的关键字段，模糊匹配一旦
  误命中，请求会被送到一个毫不相干的模型上，而且很难从日志里看出来。
- **没有命中的请求原样透传**，一个字节都不改。
- 编辑页的「拉取候选模型」给出候选 = 上游模型列表 + 该供应商历史请求用过的模型名；
  也可以直接手动输入。
- 命中映射的请求会在日志里同时记下**请求模型**（客户端要的）与**上游模型**（实际发出去的），
  改写标记为「模型映射」；日志列表的模型列显示成 `请求模型 → 上游模型`。
- Gemini 的模型名在 URL 路径上，映射改的是路径里的模型段；其余格式改的是请求体里的 `model`。

## Responses 会话续传

只对 API 格式为 `openai-responses`、路径以 `/responses` 结尾的请求生效。供应商编辑页的「高级」
里有三态开关：

- `关闭`（默认）：完全透传。
- `仅诊断`（dry-run）：只统计本地历史能否接上上一轮的链、以及预计能省下多少请求字节；
  不改请求、不设置 `store`。用真实会话跑一段，看日志里的「续传」列；`dry-hit` 比例高才值得开启。
- `开启改写`：命中后把全量 `input` 截成增量，补 `store:true` + `previous_response_id`；
  失败（400/404/409/422/503）自动回退全量，回退重发时若这一轮原本已经命中续传，会补回
  `store:true`，让这次全量响应也被保存；不支持类错误和 response id 失效的冷却时间
  可在供应商页配置（默认 3600 / 300 秒；填 0 表示不冷却，回退后下一轮立刻重建链）。

约束与后果：

- **上游会保存整段会话（含图片）**。这是 `previous_response_id` 的前提；只在你显式开启的供应商上生效。
  请求带 `x-cmd-zdr` / `x-zdr` / `x-zero-data-retention` 时自动跳过。
- **只有原生 Responses 且真正实现 `previous_response_id` 的模型有效**。实测通过：
  `grok-4.6`、`grok-4.7`、`gpt-5.6-luna`、`gpt-6-luna`；`deepseek-v4.1-flash` 会拒绝
  `store:true`（自动回退），`deepseek-v4-pro` 可能返回 200 但**静默丢失上下文**。程序不内置
  白名单，由你在「模型范围」里决定；留空表示全部模型。
  commandcode 网关上的 `deepseek/deepseek-v4.1-flash` 也实测通过（文本、图片、工具轮）；
  该网关的 `store` 会成片不可用（同一个会话连续多轮回显 `false`，换会话又正常），
  这类轮次只能发全量，日志里会标成「上游未保存」；偶发的续传 400 会自动回退，
  并按配置的冷却时间（默认 5 分钟）重新建链。
- 匹配规则：请求历史部分（`input` 段）必须逐项一致（去 `id/status`、合并 text 类型、解析工具参数），
  但**上一轮响应的 output 部分允许客户端丢项**——ZCode 回放历史时不带 `reasoning`、偶尔还会丢掉
  没执行的 `function_call`，这些缺项不影响续传（上游已经保存过）。历史、模型、账号、
  工具定义或采样参数变化仍会退回全量，客户端每轮重新编码图片也会导致不命中。
- 标准 Responses 接口在续传时仍按整条链计费，省的是上行请求体积和首 token 等待，不是 token 账单。
- 日志会记录 `continuation_mode`、`continuation_reason`、`saved_bytes`。日志列表的「标记」列用颜色
  直接区分结果：绿色「续传成功」= 这一轮真的只发了增量；红色「续传失败已回退」/ 黄色「续传不可用」
  = 尝试过但上游没接受；灰色「本轮建链（全量）」= 发了全量用于建链，本身不省流量。
  详情页「续传」区给出同样的结论和一句话原因。

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

前端用 Vite + Vue 3 + TypeScript + Naive UI + ECharts，图标用 lucide-vue-next。
开发时 Vite 把 `/api` 代理到 `127.0.0.1:8080`，构建产物由 `go:embed` 打进二进制。

服务端把「能对应到嵌入文件的路径」返回静态资源，其余路径全部当作转发目标 ——
所以管理界面与代理入口共用一个端口，且任何上游路径（`/v1/...`、`/v1beta/...`）都无需预先注册。

界面样式遵循两条约定：

- 颜色、间距、字号只从 `web/src/styles/tokens.css` 取，组件里不写死 hex。
  ECharts 认不了 CSS 变量，序列色另有一份 `web/src/theme/palette.ts`，改色时两处一起改。
- 卡片统一用 `AppCard`，页面标题统一用 `PageHeader`；不要再各写一份 `box-shadow`。

### 桌面版的开发

```bash
# 只构建桌面形态（需要先 npm run build 过前端）
go build -tags "production desktop" -ldflags "-H windowsgui" -o ai_proxy-desktop.exe .

# 调试时保留控制台，方便看日志
go build -tags "production desktop" -o ai_proxy-desktop-debug.exe .
```

桌面入口是 `main_desktop.go`（`//go:build desktop && windows`），命令行入口是 `main.go`
（`//go:build !desktop`），两者共用 `internal/service` 的启动逻辑。

新增桌面能力时注意：`internal/desktop` 里的接口只由桌面入口注册，
服务端形态根本没有这些路由，所以前端一律先看 `/api/meta` 的 `desktop` 字段再决定是否展示。

## 目录结构

```
main.go                 服务端入口（build tag: !desktop）
main_desktop.go         桌面入口（build tag: desktop && windows）
desktop_platform_windows.go  单实例互斥量、窗口激活、WebView2 检测、原生弹窗
desktop_platform_other.go    非 Windows 的桩实现
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
  service/              服务生命周期：先 Listen 再建路由、可 Shutdown、后台任务可等待
    datadir.go            数据目录解析（桌面版优先沿用 exe 同级 data）
  desktop/              桌面壳：配置持久化、桌面专属 API、打开目录
  logging/              文件日志与轮转（桌面版没有控制台，靠它排障）
  version/              构建版本号（-ldflags 注入）
  winres/               Windows 图标与版本信息（.syso，仅桌面入口引用）
web/                    前端源码（构建产物 web/dist 被 embed）
web/src/styles/tokens.css   设计 token：颜色/间距/字号/圆角
web/src/theme/          图表调色板与 Naive UI 主题覆盖
scripts/mock-upstream.mjs  本地模拟上游
scripts/e2e-check.mjs      端到端验收脚本
scripts/gen-icon/          生成应用图标（多尺寸 ICO）
scripts/gen-rc/            生成 Windows 资源脚本（含版本号）
```

## 已知边界

- 只做转发与记账，**不在协议格式之间转换**。上游是什么格式，客户端就得用什么格式。
- 桌面版暂只支持 Windows；Linux 继续用服务端版。
- 主动去掉转发请求里的 `Accept-Encoding`：压缩后的响应无法旁路解析用量，而用量统计是本程序的
  核心功能。上游因此返回未压缩内容，客户端也会收到未压缩内容。
- 请求体需要完整读入内存才能改写与记账，硬上限 128 MB。
- 报文默认各截断到 64 KB 后落库（可在设置里调整）；关掉报文存储后统计指标仍然完整。
- 数据库用 `modernc.org/sqlite` 纯 Go 驱动，无 CGO 依赖，但也因此连接数限制为 1，
  极高并发写入场景下可能成为瓶颈。
