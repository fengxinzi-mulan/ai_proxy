// TPS 可信上限的端到端回归：确认「上游整流下发」的响应不会被算出上万 t/s。
//
// 背景见 internal/proxy/server.go 的 maxPlausibleTPS：上游把整条 SSE 攒齐后一次性
// 下发时，代理量到的生成窗口（总耗时 − 首 token）会塌成几毫秒，58 个 token 就能
// 算出 19000 t/s。修复后这种情况记为不可测（TPS=0，界面显示 —）。
//
// 用法（先起 mock 上游与一个隔离实例，别拿正在用的那个）：
//
//	node scripts/mock-upstream.mjs 9911
//	./ai_proxy.exe -host 127.0.0.1 -port 8791 -data <临时目录>
//	node scripts/tps-guard-check.mjs http://127.0.0.1:8791
//
// 两个用例：
//  1. mock-model          逐块下发（40ms/块）→ TPS 应有值，且不超过上限
//  2. mock-buffered-model 整流下发（攒齐后一次性写）→ TPS 应为 0（不可测）
const BASE = process.argv[2] ?? 'http://127.0.0.1:8791'
const MOCK_BASE = process.env.MOCK_BASE ?? 'http://127.0.0.1:9911/v1'
const PROVIDER = 'tpscheck'
const MAX_PLAUSIBLE_TPS = 500

let failures = 0

function check(name, cond, detail) {
  if (cond) {
    console.log(`  PASS  ${name}`)
  } else {
    failures++
    console.log(`  FAIL  ${name}${detail !== undefined ? ' -> ' + JSON.stringify(detail) : ''}`)
  }
}

async function json(method, path, body) {
  const res = await fetch(BASE + path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  try {
    return { status: res.status, body: text ? JSON.parse(text) : null }
  } catch {
    return { status: res.status, body: text }
  }
}

/** 保证测试用的供应商存在，并指向 mock 上游。 */
async function ensureProvider() {
  const providers = await json('GET', '/api/providers')
  const list = Array.isArray(providers.body) ? providers.body : (providers.body?.items ?? [])
  const existing = list.find((p) => p.name === PROVIDER)
  const payload = {
    name: PROVIDER,
    displayName: 'TPS 回归用 mock',
    enabled: true,
    baseUrl: MOCK_BASE,
    apiFormat: 'openai',
    keys: [{ id: 'k1', key: 'sk-mock', enabled: true }],
  }
  if (existing) {
    const r = await json('PUT', `/api/providers/${existing.id}`, { ...existing, ...payload })
    if (r.status !== 200) throw new Error(`更新供应商失败: ${r.status} ${JSON.stringify(r.body)}`)
    return existing.id
  }
  const created = await json('POST', '/api/providers', payload)
  if (created.status !== 200 && created.status !== 201) {
    throw new Error(`创建供应商失败: ${created.status} ${JSON.stringify(created.body)}`)
  }
  return created.body.id
}

/** 发一次流式请求，返回客户端侧观测到的投递形态。 */
async function streamOnce(model) {
  const res = await fetch(`${BASE}/p/${PROVIDER}/v1/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ model, stream: true, messages: [{ role: 'user', content: '你好' }] }),
  })
  const chunks = []
  const reader = res.body.getReader()
  const dec = new TextDecoder()
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    chunks.push(dec.decode(value, { stream: true }))
  }
  const text = chunks.join('')
  return { status: res.status, text, chunks: chunks.length }
}

async function lastLog() {
  const page = await json('GET', '/api/logs?page=1&pageSize=1&range=all')
  const items = page.body?.items ?? []
  if (items.length === 0) throw new Error('没有落库日志')
  return items[0]
}

async function main() {
  const providerId = await ensureProvider()
  console.log(`供应商 ${PROVIDER} (id=${providerId}) → ${MOCK_BASE}`)

  console.log('\n--- 1. 逐块下发：TPS 应有值且不超过上限 ---')
  {
    const r = await streamOnce('mock-model')
    check('HTTP 200', r.status === 200, r.status)
    check('收到完整流', r.text.includes('[DONE]'), r.text.slice(0, 120))
    check('分多次到达（逐块下发）', r.chunks > 1, r.chunks)

    const log = await lastLog()
    console.log(`  日志：completionTokens=${log.completionTokens} ttftMs=${log.ttftMs} totalMs=${log.totalMs} tps=${log.tps}`)
    check('输出 token 已记录', log.completionTokens > 0, log.completionTokens)
    check('首 token 已记录', log.ttftMs > 0, log.ttftMs)
    check('TPS 有值', log.tps > 0, log.tps)
    check(`TPS 不超过可信上限 ${MAX_PLAUSIBLE_TPS}`, log.tps <= MAX_PLAUSIBLE_TPS, log.tps)
  }

  console.log('\n--- 2. 整流下发：TPS 应判为不可测（0） ---')
  {
    const r = await streamOnce('mock-buffered-model')
    check('HTTP 200', r.status === 200, r.status)
    check('收到完整流', r.text.includes('[DONE]'), r.text.slice(0, 120))

    const log = await lastLog()
    console.log(`  日志：completionTokens=${log.completionTokens} ttftMs=${log.ttftMs} totalMs=${log.totalMs} tps=${log.tps}`)
    check('输出 token 照常记录', log.completionTokens > 0, log.completionTokens)
    check('首 token 照常记录', log.ttftMs > 0, log.ttftMs)
    check('TPS = 0（不可测）', log.tps === 0, log.tps)
  }

  console.log(failures === 0 ? '\n全部通过' : `\n${failures} 项未通过`)
  process.exit(failures === 0 ? 0 : 1)
}

main().catch((err) => {
  console.error('脚本执行失败:', err.message)
  process.exit(1)
})
