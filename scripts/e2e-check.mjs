// 端到端验收：通过真实 HTTP 打代理，然后核对落库的统计字段。
const BASE = 'http://127.0.0.1:18080'
let failures = 0

/** 脚本会临时改供应商配置，结束时必须还原，避免把环境留在奇怪状态。 */
let originalProvider = null

async function restoreProvider() {
  if (!originalProvider) return
  await fetch(`${BASE}/api/providers/${originalProvider.id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(originalProvider),
  })
}

function check(name, cond, detail) {
  if (cond) {
    console.log(`  PASS  ${name}`)
  } else {
    failures++
    console.log(`  FAIL  ${name}${detail ? ' -> ' + detail : ''}`)
  }
}

async function post(path, body, headers = {}) {
  const res = await fetch(BASE + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...headers },
    body: JSON.stringify(body),
  })
  const text = await res.text()
  return { status: res.status, text, headers: res.headers }
}

async function lastLog(n = 1) {
  const page = await (await fetch(`${BASE}/api/logs?page=1&pageSize=${n}&range=all`)).json()
  return page.items
}

async function detail(id) {
  return await (await fetch(`${BASE}/api/logs/${id}`)).json()
}

/** 逐块读取 SSE，用于测量首字节到达时间。 */
async function streamPost(path, body, headers = {}) {
  const res = await fetch(BASE + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...headers },
    body: JSON.stringify(body),
  })
  const chunks = []
  const reader = res.body.getReader()
  const dec = new TextDecoder()
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    chunks.push(dec.decode(value, { stream: true }))
  }
  return { status: res.status, body: chunks.join(''), contentType: res.headers.get('content-type') }
}

async function main() {
  originalProvider = await (await fetch(`${BASE}/api/providers/1`)).json()

  console.log('\n--- 1. OpenAI 非流式 ---')
  {
    const r = await post('/v1/chat/completions', {
      model: 'mock-model',
      messages: [{ role: 'user', content: '你好' }],
    })
    check('HTTP 200', r.status === 200, r.status)
    const body = JSON.parse(r.text)
    check('响应体原样透传', body.usage?.cached_tokens === undefined || body.usage?.prompt_tokens_details?.cached_tokens === 16,
      JSON.stringify(body.usage))

    const [log] = await lastLog()
    const d = await detail(log.id)
    check('成功标记', d.success === true, d.errorMsg)
    check('输入 token = 42', d.promptTokens === 42, d.promptTokens)
    check('缓存 token = 16', d.cachedTokens === 16, d.cachedTokens)
    check('输出 token = 37', d.completionTokens === 37, d.completionTokens)
    check('总计 = 79', d.totalTokens === 79, d.totalTokens)
    check('非流式 TTFT 为 0', d.ttftMs === 0, d.ttftMs)
    check('非流式不计算 TPS', d.tps === 0, d.tps)
    check('模型名', d.model === 'mock-model', d.model)
    check('客户端 IP 已记录', d.clientIp === '127.0.0.1', d.clientIp)
    check('token 非估算', d.tokensEstimated === false, d.tokensEstimated)
    check('上游地址已记录', d.upstreamUrl.includes('/v1/chat/completions'), d.upstreamUrl)
    check('请求头已脱敏', d.reqHeaders.includes('****') && !d.reqHeaders.includes('sk-mock-1234567890'), d.reqHeaders)
  }

  console.log('\n--- 2. OpenAI 流式（含 usage 注入） ---')
  {
    const r = await streamPost('/v1/chat/completions', {
      model: 'mock-model',
      stream: true,
      messages: [{ role: 'user', content: '你好' }],
    })
    check('HTTP 200', r.status === 200, r.status)
    check('Content-Type 为 SSE', r.contentType?.includes('text/event-stream'), r.contentType)
    check('响应含 [DONE]', r.body.includes('[DONE]'))
    check('流式内容完整', r.body.includes('模拟生成的回答'))
    // 上游只在收到 include_usage 时才返回 usage 块，这里能收到就说明注入生效了。
    check('usage 收尾块已回传', /"usage":\s*\{/.test(r.body), r.body.slice(-260))

    const [log] = await lastLog()
    const d = await detail(log.id)
    check('流式标记', d.stream === true)
    check('成功标记', d.success === true, d.errorMsg)
    check('请求被标记为已改写', d.requestModified === true, JSON.stringify(d.modifications))
    check('改写类型为 usage_inject', d.modifications.includes('usage_inject'), JSON.stringify(d.modifications))
    check('保存了改写前请求体', !!d.reqBodyOriginal && !d.reqBodyOriginal.includes('include_usage'), d.reqBodyOriginal)
    check('实际发出含 include_usage', d.reqBody.includes('include_usage'), d.reqBody)
    check('输入 token = 42', d.promptTokens === 42, d.promptTokens)
    check('输出 token = 37', d.completionTokens === 37, d.completionTokens)
    check('TTFT > 0', d.ttftMs > 0, d.ttftMs)
    check('TTFT 小于总耗时', d.ttftMs <= d.totalMs, `${d.ttftMs} vs ${d.totalMs}`)
    check('TPS > 0', d.tps > 0, d.tps)
    check('token 非估算', d.tokensEstimated === false, d.tokensEstimated)
    check('响应体已记录', d.respBody.includes('模拟生成'), d.respBody.slice(0, 80))
  }

  console.log('\n--- 3. Anthropic Messages ---')
  {
    // 先临时把格式改成 anthropic，并加一条供应商级提示词，验证注入与解析。
    const p = await (await fetch(`${BASE}/api/providers/1`)).json()
    p.apiFormat = 'anthropic'
    p.authHeader = 'x-api-key'
    p.authPrefix = ''
    p.customPath = '/v1/messages'
    p.promptMode = 'custom'
    p.prompt = { enabled: true, text: '始终用中文回答', strategy: 'append' }
    await fetch(`${BASE}/api/providers/1`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(p),
    })

    const r = await streamPost('/v1/messages', {
      model: 'mock-claude',
      stream: true,
      max_tokens: 100,
      system: '原始系统提示',
      messages: [{ role: 'user', content: '你好' }],
    })
    check('HTTP 200', r.status === 200, r.status)
    check('含 message_stop', r.body.includes('message_stop'))

    const [log] = await lastLog()
    const d = await detail(log.id)
    check('成功标记', d.success === true, d.errorMsg)
    check('输入总量 = 25+12+8 = 45', d.promptTokens === 45, d.promptTokens)
    check('缓存命中 = 12', d.cachedTokens === 12, d.cachedTokens)
    check('缓存写入 = 8', d.cacheWriteTokens === 8, d.cacheWriteTokens)
    check('输出 token = 30', d.completionTokens === 30, d.completionTokens)
    check('提示词注入已留痕', d.modifications.includes('prompt_append'), JSON.stringify(d.modifications))
    check('发出的请求含注入的提示词', d.reqBody.includes('始终用中文回答'), d.reqBody)
    check('原 system 保留并被追加', d.reqBody.includes('原始系统提示'), d.reqBody)
    check('未被注入 usage 参数', !d.reqBody.includes('include_usage'), d.reqBody)
    check('首 token 已记录', d.ttftMs > 0, d.ttftMs)
  }

  console.log('\n--- 4. Gemini generateContent ---')
  {
    const p = await (await fetch(`${BASE}/api/providers/1`)).json()
    p.apiFormat = 'gemini'
    p.customPath = ''
    p.authHeader = 'x-goog-api-key'
    p.authPrefix = ''
    p.baseUrl = 'http://127.0.0.1:9911'
    p.promptMode = 'inherit'
    await fetch(`${BASE}/api/providers/1`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...p, customPath: '' }),
    })

    const r = await post('/v1beta/models/mock-gemini:generateContent', {
      contents: [{ role: 'user', parts: [{ text: '你好' }] }],
    })
    check('HTTP 200', r.status === 200, r.status)

    const [log] = await lastLog()
    const d = await detail(log.id)
    check('成功标记', d.success === true, d.errorMsg)
    check('输入 token = 18', d.promptTokens === 18, d.promptTokens)
    check('输出 token = 22', d.completionTokens === 22, d.completionTokens)
    check('缓存 token = 6', d.cachedTokens === 6, d.cachedTokens)
    check('Gemini 输入不重复叠加缓存', d.totalTokens === 40, d.totalTokens)
  }

  console.log('\n--- 5. 上游错误（401） ---')
  {
    const r = await post('/v1/fail', { model: 'x' })
    check('状态码原样回传 401', r.status === 401, r.status)

    const [log] = await lastLog()
    const d = await detail(log.id)
    check('未记为成功', d.success === false)
    check('记录了 HTTP 状态', d.httpStatus === 401, d.httpStatus)
    check('记录了上游错误描述', d.errorMsg.includes('模拟的鉴权失败'), d.errorMsg)
  }

  console.log('\n--- 6. 按短名路由 /p/ ---')
  {
    const r = await post('/p/mock/v1/chat/completions', {
      model: 'mock-model',
      messages: [{ role: 'user', content: 'hi' }],
    })
    check('HTTP 200', r.status === 200, `${r.status} ${r.text.slice(0, 120)}`)

    const r2 = await post('/p/不存在的名字/v1/chat/completions', { model: 'x' })
    check('未知短名返回 502', r2.status === 502, r2.status)
    check('未知短名给出可读错误', r2.text.includes('找不到供应商'), r2.text)
  }

  console.log('\n--- 7. 统计聚合 ---')
  {
    const ov = await (await fetch(`${BASE}/api/stats/overview?range=all`)).json()
    check('总请求数 >= 6', ov.stats.totalRequests >= 6, ov.stats.totalRequests)
    check('成功率已计算', ov.stats.successRate > 0 && ov.stats.successRate < 100, ov.stats.successRate)
    check('平均首 token > 0', ov.stats.avgTtftMs > 0, ov.stats.avgTtftMs)
    check('平均 TPS > 0', ov.stats.avgTps > 0, ov.stats.avgTps)
    check('返回了当前生效供应商', !!ov.activeProvider, JSON.stringify(ov.activeProvider?.name))

    // 缓存命中率 = 命中缓存 / 输入总量，必须落在 0–100 之间且与原始量自洽。
    const expectRate = (ov.stats.cachedTokens / ov.stats.promptTokens) * 100
    check(
      '整体缓存命中率自洽',
      Math.abs(ov.stats.cacheHitRate - expectRate) < 0.01,
      `${ov.stats.cacheHitRate} vs ${expectRate}`,
    )
    check(
      '整体缓存命中率在 0–100 之间',
      ov.stats.cacheHitRate > 0 && ov.stats.cacheHitRate <= 100,
      ov.stats.cacheHitRate,
    )

    const ts = await (await fetch(`${BASE}/api/stats/timeseries?range=all&bucket=hour`)).json()
    check('趋势分桶有数据', ts.points.length > 0, JSON.stringify(ts.points))
    check(
      '趋势分桶带缓存命中率',
      ts.points.every((p) => typeof p.cacheHitRate === 'number' && p.cacheHitRate >= 0 && p.cacheHitRate <= 100),
      JSON.stringify(ts.points.map((p) => p.cacheHitRate)),
    )

    const bm = await (await fetch(`${BASE}/api/stats/by-model?range=all`)).json()
    check('模型排行有数据', bm.length > 0, JSON.stringify(bm.map((x) => x.model)))
    check(
      '模型排行带缓存命中率',
      bm.every((r) => r.cacheHitRate >= 0 && r.cacheHitRate <= 100),
      JSON.stringify(bm.map((r) => ({ m: r.model, rate: r.cacheHitRate }))),
    )

    const bp = await (await fetch(`${BASE}/api/stats/by-provider?range=all`)).json()
    check('供应商排行有数据', bp.length > 0 && bp[0].requests > 0, JSON.stringify(bp))
    check(
      '供应商排行带缓存命中率',
      bp.every((r) => r.cacheHitRate >= 0 && r.cacheHitRate <= 100),
      JSON.stringify(bp.map((r) => ({ p: r.providerName, rate: r.cacheHitRate }))),
    )
  }

  console.log('\n--- 8. 前端静态资源与代理入口共存 ---')
  {
    const index = await fetch(`${BASE}/`)
    const html = await index.text()
    check('首页返回 HTML', index.headers.get('content-type')?.includes('html'), index.headers.get('content-type'))
    check('首页是真实前端而非占位页', html.includes('/assets/'), html.slice(0, 200))

    const assetMatch = html.match(/src="([^"]+\.js)"/)
    if (assetMatch) {
      const asset = await fetch(BASE + assetMatch[1])
      check('JS 资源可加载', asset.status === 200, asset.status)
    } else {
      check('找到 JS 资源引用', false, html.slice(0, 400))
    }

    // 管理 API 的未知路径不能落到转发逻辑里。
    const bad = await fetch(`${BASE}/api/does-not-exist`)
    check('未知 /api 路径返回 404', bad.status === 404, bad.status)
  }

  console.log(`\n===== ${failures === 0 ? '全部通过' : failures + ' 项失败'} =====`)
  await restoreProvider()
  process.exit(failures === 0 ? 0 : 1)
}

main().catch(async (e) => {
  console.error('测试脚本异常:', e)
  await restoreProvider().catch(() => {})
  process.exit(1)
})
