// 本地模拟上游，用于在不消耗真实额度的情况下验证代理的转发与统计。
//
// 用法：
//   node scripts/mock-upstream.mjs [端口]
//
// 然后在本程序的管理界面里新增一个供应商，BaseURL 填 http://127.0.0.1:9911/v1，
// 密钥随便填一个，API 格式选 OpenAI Chat Completions。
//
// 它会按请求体里的 stream 决定返回流式还是一整个 JSON，
// 并区分 /v1/chat/completions、/v1/messages、/v1beta/models/*:generateContent 三条路径，
// 覆盖三种格式的用量解析。

import { createServer } from 'node:http'

const PORT = Number(process.argv[2] ?? 9911)

/** 生成一段可读的中文/英文混合文本，便于观察 token 估算与 TPS。 */
function makeText(seed) {
  const base = '这是一段模拟生成的回答，用于验证代理的 token 统计与流式转发是否正常工作。'
  return `${base}（第 ${seed} 段）This is a mock response segment for load testing.`
}

function chunk(text, size = 12) {
  const out = []
  for (let i = 0; i < text.length; i += size) out.push(text.slice(i, i + size))
  return out
}

function readBody(req) {
  return new Promise((resolve) => {
    let raw = ''
    req.on('data', (c) => (raw += c))
    req.on('end', () => {
      try {
        resolve(raw ? JSON.parse(raw) : {})
      } catch {
        resolve({})
      }
    })
  })
}

function sse(res, event, data) {
  if (event) res.write(`event: ${event}\n`)
  res.write(`data: ${JSON.stringify(data)}\n\n`)
}

/** OpenAI Chat Completions。 */
async function openaiChat(req, res, body) {
  const model = body.model ?? 'mock-model'
  // 模型名带 slow 时切成慢速长响应，用来观察「实时监控」里的进行中请求。
  const slow = String(model).includes('slow')
  const text = slow ? makeText(1).repeat(12) : makeText(1)
  const parts = chunk(text, slow ? 16 : 12)
  const perChunkMs = slow ? 220 : 40
  const promptTokens = 42
  const completionTokens = slow ? parts.length * 8 : 37

  if (!body.stream) {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(
      JSON.stringify({
        id: 'chatcmpl-mock',
        object: 'chat.completion',
        model,
        choices: [
          { index: 0, message: { role: 'assistant', content: text }, finish_reason: 'stop' },
        ],
        usage: {
          prompt_tokens: promptTokens,
          completion_tokens: completionTokens,
          total_tokens: promptTokens + completionTokens,
          prompt_tokens_details: { cached_tokens: 16 },
          completion_tokens_details: { reasoning_tokens: 0 },
        },
      }),
    )
    return
  }

  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  })

  // 首个包只有 role，按约定不计入首 token。
  sse(res, null, { id: 'chatcmpl-mock', model, choices: [{ index: 0, delta: { role: 'assistant' } }] })

  // 模拟首 token 等待。
  await new Promise((r) => setTimeout(r, 300))

  for (const p of parts) {
    sse(res, null, { id: 'chatcmpl-mock', model, choices: [{ index: 0, delta: { content: p } }] })
    await new Promise((r) => setTimeout(r, perChunkMs))
  }
  sse(res, null, { id: 'chatcmpl-mock', model, choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] })

  // 只有客户端开了 include_usage 才会带这个收尾块，与真实 OpenAI 行为一致。
  if (body.stream_options?.include_usage) {
    sse(res, null, {
      id: 'chatcmpl-mock',
      model,
      choices: [],
      usage: {
        prompt_tokens: promptTokens,
        completion_tokens: completionTokens,
        total_tokens: promptTokens + completionTokens,
        prompt_tokens_details: { cached_tokens: 16 },
      },
    })
  }
  res.write('data: [DONE]\n\n')
  res.end()
}

/** Anthropic Messages。 */
async function anthropicMessages(req, res, body) {
  const model = body.model ?? 'mock-claude'
  const text = makeText(2)

  if (!body.stream) {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(
      JSON.stringify({
        id: 'msg_mock',
        type: 'message',
        model,
        content: [{ type: 'text', text }],
        stop_reason: 'end_turn',
        usage: {
          input_tokens: 25,
          output_tokens: 30,
          cache_read_input_tokens: 12,
          cache_creation_input_tokens: 8,
        },
      }),
    )
    return
  }

  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' })
  sse(res, 'message_start', {
    type: 'message_start',
    message: {
      id: 'msg_mock',
      model,
      usage: { input_tokens: 25, output_tokens: 1, cache_read_input_tokens: 12, cache_creation_input_tokens: 8 },
    },
  })
  await new Promise((r) => setTimeout(r, 200))
  for (const p of chunk(text)) {
    sse(res, 'content_block_delta', {
      type: 'content_block_delta',
      index: 0,
      delta: { type: 'text_delta', text: p },
    })
    await new Promise((r) => setTimeout(r, 40))
  }
  sse(res, 'message_delta', {
    type: 'message_delta',
    delta: { stop_reason: 'end_turn' },
    usage: { output_tokens: 30 },
  })
  sse(res, 'message_stop', { type: 'message_stop' })
  res.end()
}

/** Gemini generateContent / streamGenerateContent。 */
async function geminiGenerate(req, res, url, body) {
  const model = decodeURIComponent(url.pathname.split('/models/')[1]?.split(':')[0] ?? 'mock-gemini')
  const text = makeText(3)
  const usageMetadata = {
    promptTokenCount: 18,
    candidatesTokenCount: 22,
    totalTokenCount: 40,
    cachedContentTokenCount: 6,
  }

  if (url.searchParams.get('alt') !== 'sse') {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(
      JSON.stringify({
        candidates: [{ content: { role: 'model', parts: [{ text }] }, finishReason: 'STOP' }],
        usageMetadata,
        modelVersion: model,
      }),
    )
    return
  }

  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' })
  for (const p of chunk(text)) {
    sse(res, null, {
      candidates: [{ content: { role: 'model', parts: [{ text: p }] } }],
      usageMetadata,
      modelVersion: model,
    })
    await new Promise((r) => setTimeout(r, 40))
  }
  sse(res, null, {
    candidates: [{ content: { role: 'model', parts: [] }, finishReason: 'STOP' }],
    usageMetadata,
    modelVersion: model,
  })
  res.end()
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`)
  const body = await readBody(req)

  // 模型列表：让「测试连接」按钮能直接成功。
  if (req.method === 'GET' && url.pathname.endsWith('/models')) {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(
      JSON.stringify({
        data: [
          { id: 'mock-model', object: 'model' },
          { id: 'mock-reasoner', object: 'model' },
          { id: 'mock-claude', object: 'model' },
          { id: 'mock-gemini', object: 'model' },
        ],
      }),
    )
    return
  }

  console.log(`${req.method} ${url.pathname}${body.stream ? ' (stream)' : ''}`)

  // 模拟一次失败，用来验证错误路径也会被记账。
  if (url.pathname.includes('/fail')) {
    res.writeHead(401, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({ error: { message: '模拟的鉴权失败', type: 'invalid_api_key' } }))
    return
  }

  if (url.pathname.endsWith('/chat/completions')) return openaiChat(req, res, body)
  if (url.pathname.endsWith('/messages')) return anthropicMessages(req, res, body)
  if (url.pathname.includes(':streamGenerateContent') || url.pathname.includes(':generateContent')) {
    return geminiGenerate(req, res, url, body)
  }

  res.writeHead(404, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify({ error: { message: `mock 上游未实现该路径: ${url.pathname}` } }))
})

server.listen(PORT, '127.0.0.1', () => {
  console.log(`模拟上游已启动: http://127.0.0.1:${PORT}`)
  console.log('可用路径:')
  console.log('  POST /v1/chat/completions        （stream 字段决定流式与否）')
  console.log('  POST /v1/messages                （Anthropic）')
  console.log('  POST /v1beta/models/X:generateContent  （Gemini，加 ?alt=sse 走流式）')
  console.log('  GET  /v1/models                   （模型列表）')
  console.log('  POST /v1/fail                     （模拟 401）')
})
