// Package proxy 实现请求的透明转发与全程记账。
//
// 核心承诺：除了用户显式开启的请求体改写（usage 注入、提示词注入、严格上游
// 字段裁剪）之外，请求按原字节转发；响应只在两种情况下动字节——用户开启的
// 「剥离 usage 块」，以及把非标准的 chat/completions content 数组拍平成字符串
// （不改内容的语义，只为让按 OpenAI 规范校验的客户端能正常解析）。
package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// maxRequestBodyBytes 是单个请求体的读取上限。
// 请求体需要完整读入内存才能改写与记账，所以必须有硬上限兜底。
const maxRequestBodyBytes = 128 << 20

// copyBufferSize 是转发时的读块大小。
const copyBufferSize = 32 << 10

// Server 是转发入口，同时对外提供运行时可观测状态。
type Server struct {
	store        *store.Store
	hub          *hub.Hub
	live         *LiveRegistry
	clients      *clientPool
	keys         *keyPool
	continuation *continuationManager
	// mistral 缓存 Mistral 网页后台的登录会话（见 mistralauth.go）。
	// 放在 Server 上而不是每次查询新建：登录有速率限制，定时刷新时不能每次重登。
	mistral *mistralSession
	logger  *slog.Logger

	idSeq atomic.Uint64
}

// NewServer 构造转发服务。
func NewServer(st *store.Store, h *hub.Hub, logger *slog.Logger) *Server {
	return &Server{
		store:        st,
		hub:          h,
		live:         NewLiveRegistry(),
		clients:      newClientPool(),
		keys:         newKeyPool(),
		continuation: newContinuationManager(),
		mistral:      newMistralSession(),
		logger:       logger,
	}
}

// ResetContinuation 清空续传链与冷却状态；供应商配置变化后调用，
// 让用户改完模型/开关后不必等冷却到期或重启进程。
func (s *Server) ResetContinuation() {
	if s == nil || s.continuation == nil {
		return
	}
	s.continuation.reset()
}

// Live 暴露进行中请求注册表，供管理接口读取。
func (s *Server) Live() *LiveRegistry { return s.live }

// InvalidateClients 在全局设置（尤其是代理）变化后清空连接缓存。
func (s *Server) InvalidateClients() { s.clients.reset() }

// KeyHealth 返回指定供应商的密钥健康快照。
func (s *Server) KeyHealth(prov model.Provider) map[string]KeyHealth {
	return s.keys.Health(prov)
}

// ServeHTTP 是转发主流程。
//
// 它挂在 catch-all 路由上：/api/ 前缀由管理接口处理，其余全部当作转发目标，
// 这样任何客户端把 base_url 指过来都能工作，无需为每种 API 路径单独注册。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	in := forwardInput{
		start:      time.Now(),
		method:     r.Method,
		clientPath: r.URL.Path,
		clientIP:   clientIP(r),
	}

	settings, err := s.store.GetSettings()
	if err != nil {
		s.logger.Error("读取全局设置失败", "err", err)
		s.failEarly(w, in, http.StatusInternalServerError, "读取配置失败: "+err.Error())
		return
	}
	in.settings = settings

	if !s.authorized(r, settings) {
		s.failEarly(w, in, http.StatusUnauthorized, "代理访问密钥无效")
		return
	}

	prov, upstreamPath, err := s.resolveProvider(r)
	if err != nil {
		// 没有任何可用供应商时给出可操作的提示，而不是干巴巴的 502。
		s.failEarly(w, in, http.StatusBadGateway, err.Error())
		return
	}
	in.provider = prov
	in.upstreamPath = upstreamPath

	reqBody, err := readRequestBody(r)
	if err != nil {
		s.failEarly(w, in, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	in.reqBody = reqBody

	// 模型名与流式意图都在改写之前解析：写入日志、实时监控和提示词规则匹配都要用，
	// 而 Gemini 这两项都不在请求体里（模型在路径上，流式靠 :streamGenerateContent / alt=sse）。
	in.model = modelNameForRequest(prov.APIFormat, in.clientPath, reqBody)
	in.wantStream = requestWantsStream(prov.APIFormat, in.clientPath, r.URL.RawQuery, reqBody)

	// 模型映射：把客户端请求的模型名换成目标模型名。非 Gemini 的格式模型名在请求体里，
	// 由 rewriteRequestBody 改写；Gemini 的模型名在 URL 路径上，必须在拼上游地址之前
	// 就把路径里的那一段换掉，所以在这里单独处理。
	if mapped, ok := prov.MapModel(in.model); ok {
		in.modelMapped = mapped
		if prov.APIFormat == model.FormatGemini {
			in.upstreamPath = replaceGeminiModelInPath(in.upstreamPath, in.model, mapped)
		}
	}

	// 会话标识与端点路径先于改写算好：prompt_cache_key 与实际协议判定都要用
	// 「真正发往上游的那条路径」（配了 CustomPath 时客户端路径被完全接管）。
	endpointPath := upstreamEndpointPath(prov, in.upstreamPath)
	// 响应与请求改写都按「这次请求实际用的协议」走，而不是供应商配置的静态协议：
	// DSH 会同时打 /v1/chat/completions 和 /v1/responses，供应商却只能配一种。
	in.format = effectiveFormat(prov.APIFormat, endpointPath)

	rewritten := rewriteRequest(prov, settings, reqBody, in.model, rewriteOptions{
		cacheKeySeed: sessionCacheSeed(r),
		clientPath:   endpointPath,
		format:       in.format,
	})
	in.outBody = rewritten.Body
	in.modifications = rewritten.Modifications
	in.imageCompressed = rewritten.ImageCompressed
	in.imageSavedBytes = rewritten.ImageSavedBytes
	in.imagesDropped = rewritten.ImagesDropped

	s.forward(w, r, in)
}

// forwardInput 汇总一次转发的输入，避免 forward 参数过长。
type forwardInput struct {
	start        time.Time
	settings     model.Settings
	provider     model.Provider
	method       string
	clientPath   string
	clientIP     string
	upstreamPath string
	model        string // 请求模型：Gemini 在 URL 路径里，其余格式在请求体里
	// modelMapped 是模型映射命中后实际发给上游的模型名；未命中为空。
	modelMapped   string
	wantStream    bool   // 客户端是否要求流式响应
	reqBody       []byte // 客户端原始请求体
	outBody       []byte // 改写后实际发出的请求体
	modifications []string
	continuation  *continuationAttempt
	// imageCompressed / imageSavedBytes 是本次请求里内联图片压缩的结果，
	// 由 rewriteRequest 算出，落进日志供「这个功能到底省了多少」核对。
	imageCompressed int
	imageSavedBytes int64
	// imagesDropped 是超出供应商图片数量上限、被占位文本替换掉的张数。
	imagesDropped int
	// format 是「这次请求实际用的协议」，由请求路径推导（见 effectiveFormat）。
	// 响应解析、归一化、统计都看它，而不是供应商配置的 apiFormat。
	format model.APIFormat
}

// apiFormat 返回本次请求实际使用的协议。
//
// ServeHTTP 里已经按路径算好 in.format；这里保留一个回落，是为了 relay 等
// 直接构造 forwardInput 的调用方（测试、失败路径）不显式设置时仍按供应商配置走。
func (in forwardInput) apiFormat() model.APIFormat {
	if in.format != "" {
		return in.format
	}
	return in.provider.APIFormat
}

// effectiveFormat 决定这次请求按哪种协议解析响应。
//
// 不能只看供应商配置的 apiFormat：客户端可能把 /v1/chat/completions 打到配置成
// openai-responses 的供应商上（DSH 就同时用两个端点）。响应解析、归一化、用量统计
// 都必须跟着「实际这条请求的形状」走，否则 chat 的分块数组没人拍平，客户端会
// 直接报 text-delta text must be a string。
func effectiveFormat(fallback model.APIFormat, clientPath string) model.APIFormat {
	path := strings.TrimSuffix(clientPath, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return model.FormatOpenAI
	case strings.HasSuffix(path, "/responses"):
		return model.FormatResponses
	}
	return fallback
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, in forwardInput) {
	prov := in.provider

	target, err := buildUpstreamURL(prov, in.upstreamPath, r.URL.RawQuery)
	if err != nil {
		s.failWith(w, in, http.StatusBadGateway, err.Error())
		return
	}
	// 客户端可能用 ?key=<代理访问密钥> 通过本代理鉴权（Gemini SDK 的旧写法）。
	// 那是给本代理看的，绝不能随 URL 发给上游，更不能落进请求日志。
	target = stripProxyKeyFromURL(target, in.settings.AccessKey)

	key, _ := s.keys.Pick(prov)
	// 续传改写必须在选定 key 之后：链按 provider+model+key+session 隔离，
	// 不同账号的 response id 不能互相引用。
	in.continuation = s.continuation.prepare(&in, r, key)
	netcfg := resolveNet(prov, in.settings)
	client, err := s.clients.client(netcfg)
	if err != nil {
		s.failWith(w, in, http.StatusBadGateway, "初始化上游连接失败: "+err.Error())
		return
	}

	ctx := r.Context()
	if netcfg.TotalTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, netcfg.TotalTime)
		defer cancel()
	}

	headers := buildUpstreamHeaders(r, prov, key, in.settings.AccessKey)
	upReq, err := buildRequest(ctx, r.Method, target, in.outBody, headers)
	if err != nil {
		s.failWith(w, in, http.StatusBadGateway, err.Error())
		return
	}

	liveID := fmt.Sprintf("%d-%d", prov.ID, s.idSeq.Add(1))
	s.live.Add(model.LiveRequest{
		ID:           liveID,
		ProviderName: prov.DisplayName,
		Model:        in.model,
		Path:         in.clientPath,
		Stream:       in.wantStream,
		ClientIP:     in.clientIP,
	})
	defer s.live.Remove(liveID)

	resp, err := client.Do(upReq)
	if err != nil {
		s.failUpstream(w, r, in, prov, key, target, netcfg, headers, err)
		return
	}
	resp, err = s.retryContinuation(resp, &in, client, target, headers, ctx)
	if err != nil {
		s.failUpstream(w, r, in, prov, key, target, netcfg, headers, err)
		return
	}
	defer resp.Body.Close()

	// 按状态码决定是否让该密钥进入冷却。
	s.keys.Report(prov.ID, key.ID, resp.StatusCode, "")

	// 未压缩且响应体可能被改写（OpenAI chat 归一化或剥离 usage 块）时，长度会随之变化，
	// 必须在写响应头之前把上游的 Content-Length 摘掉，让 Go 走分块传输，
	// 否则客户端会按旧长度截断或一直等剩余字节。
	// 上游已压缩时 relay 会原样转发压缩字节，绝不能删 Content-Encoding。
	respCompressed := hasNonIdentityEncoding(resp.Header.Get("Content-Encoding"))
	mayMutateResp := !respCompressed && (prov.StripUsageChunk || in.apiFormat() == model.FormatOpenAI)
	if mayMutateResp {
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
	}

	copyResponseHeaders(w.Header(), resp.Header, mayMutateResp)
	w.WriteHeader(resp.StatusCode)

	res := s.relay(w, r, resp, in, liveID)
	res.status = resp.StatusCode
	res.upstream = target
	res.proxyDesc = netcfg.ProxyDesc
	res.headers = headers
	s.continuation.commit(in.continuation, res)
	s.record(in, res)
}

// failUpstream 统一处理「还没写出响应头就失败」的情况。
func (s *Server) failUpstream(w http.ResponseWriter, r *http.Request, in forwardInput,
	prov model.Provider, key model.APIKey, target string, netcfg resolvedNet,
	headers http.Header, err error) {
	if in.continuation != nil {
		// 没发出去就没有节省，别留下误导性的数字。
		in.continuation.savedBytes = 0
		if in.continuation.mode == "continued" || in.continuation.mode == "seed" {
			in.continuation.reason = "network-error"
		}
		s.continuation.fail(in.continuation)
	}
	cancelled := isClientGone(r, err)
	status := http.StatusBadGateway
	msg := "请求上游失败: " + err.Error()
	if cancelled {
		status = statusClientClosedRequest
		msg = "客户端已断开连接"
	} else if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		msg = "请求上游超时: " + err.Error()
	}
	s.keys.Report(prov.ID, key.ID, 0, err.Error())
	s.record(in, logResult{
		status:    status,
		errMsg:    msg,
		cancelled: cancelled,
		upstream:  target,
		proxyDesc: netcfg.ProxyDesc,
		headers:   headers,
	})
	writeProxyError(w, status, msg)
}

// relay 把上游响应原样写给客户端，同时旁路收集统计指标。
func (s *Server) relay(w http.ResponseWriter, r *http.Request, resp *http.Response, in forwardInput, liveID string) logResult {
	prov := in.provider
	var res logResult

	// 上游用了压缩时无法旁路解析用量，直接退化为估算。
	compressed := hasNonIdentityEncoding(resp.Header.Get("Content-Encoding"))
	an := newAnalyzer(in.apiFormat(), prov.CustomUsage, resp.Header.Get("Content-Type"))

	capture := newCapture(in.settings.StoreBodies, in.settings.MaxBodyBytes)

	buf := make([]byte, copyBufferSize)
	controller := http.NewResponseController(w)

	var (
		ttft              time.Duration
		ttftSeen          bool
		events            int
		writeErr          error
		respModded        bool
		terminalDelivered bool
	)
	flush := func() {
		if writeErr != nil {
			return
		}
		if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeErr = err
		}
	}
	readFailure := func(err error) {
		if !errors.Is(err, io.EOF) {
			res.errMsg = "读取上游响应失败: " + err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				res.errMsg = "读取上游响应超时: " + err.Error()
			}
		}
	}

	if an.Streaming() && !compressed {
		framer := newSSEFramer(func(raw []byte, event, data string) {
			if writeErr != nil || terminalDelivered {
				return
			}
			if err := r.Context().Err(); err != nil {
				writeErr = err
				return
			}
			// 「剥离 usage 块」只影响回传给客户端的字节；统计该看的一个都不能少，
			// 否则开了这个开关用量就退化成估算值了。
			analyzerData := data
			if prov.StripUsageChunk && an.IsUsageOnlyChunk(event, data) {
				respModded = true
			} else {
				out := raw
				// chat/completions 的 content 偶尔是分块数组，客户端按规范只认
				// 字符串，这里按事件归一化后再转发；分析器同样看归一化后的内容。
				if in.apiFormat() == model.FormatOpenAI {
					if replaced, newData, ok := normalizeChatSSEEvent(event, data); ok {
						out = replaced
						analyzerData = newData
						respModded = true
					}
				}
				n, err := w.Write(out)
				if err == nil && n != len(out) {
					err = io.ErrShortWrite
				}
				if err != nil {
					writeErr = err
					return
				}
				capture.add(out)
			}
			an.OnEvent(event, analyzerData)
			if !ttftSeen && an.HasContent() {
				ttftSeen = true
				ttft = time.Since(in.start)
				s.live.MarkFirstToken(liveID)
			}
			// 每个内容事件都去抢一次锁不划算，隔几个事件更新一次实时值即可。
			if events++; events%8 == 0 {
				s.live.UpdateTokens(liveID, an.EstimateOutputTokens())
			}
			flush()
			// 交付了终止事件就不再读上游：Chat 的 [DONE]、Anthropic 的
			// message_stop、Responses 的 response.completed/failed/incomplete。
			// 少了这一步，客户端收完就关连接，我们还在读，会拿到一个
			// context canceled，把一条正常完成的请求记成「已取消」。
			if writeErr == nil {
				if an.TerminalEvent(event, analyzerData) ||
					(in.apiFormat() == model.FormatResponses && an.StreamFinished()) {
					terminalDelivered = true
				}
			}
		})

		flush() // 立刻把响应头推给客户端，让客户端能尽早开始计时
		for writeErr == nil {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				framer.Feed(buf[:n])
				if writeErr != nil || terminalDelivered {
					break
				}
			}
			if readErr != nil {
				readFailure(readErr)
				break
			}
		}
		if !terminalDelivered {
			framer.Flush()
			flush()
		}
		if terminalDelivered {
			// Responses 的终止事件后不再等 EOF，避免客户端正常收尾取消覆盖结果。
			resp.Body.Close()
		}
	} else {
		flush()
		// 非流式 chat/completions 同样可能把 content 写成数组。整体解析才能改，
		// 所以先读进内存（有上限），改完再交回原来的转发循环；超限就放弃改写、
		// 原样透传，避免为了一次兼容把内存吃穿。
		var pendingReadErr error
		if in.apiFormat() == model.FormatOpenAI && !compressed {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNormalizeBodyBytes+1))
			pendingReadErr = readErr
			if int64(len(body)) <= maxNormalizeBodyBytes {
				resp.Body.Close()
				if out, changed := normalizeChatResponseBody(body); changed {
					body = out
					respModded = true
				}
				resp.Body = io.NopCloser(bytes.NewReader(body))
			} else {
				resp.Body = prefixedReadCloser{
					Reader: io.MultiReader(bytes.NewReader(body), resp.Body),
					Closer: resp.Body,
				}
			}
		}
		for writeErr == nil {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				written, err := w.Write(buf[:n])
				if err == nil && written != n {
					err = io.ErrShortWrite
				}
				if err != nil {
					writeErr = err
					break
				}
				capture.add(buf[:n])
				if !compressed {
					an.Feed(buf[:n])
				}
			}
			if readErr != nil {
				readFailure(readErr)
				break
			}
		}
		if pendingReadErr != nil {
			readFailure(pendingReadErr)
		}
		flush()
	}

	an.Finish()

	res.ttftMs = ttft.Milliseconds()
	res.appliedTTFT = ttftSeen
	res.responseModified = respModded
	res.respBody = capture.string()

	usage := an.Result()
	if !usage.Found {
		// 上游没给用量：输入从头估算，输出按流式增量估算。
		usage.PromptTokens = estimatePromptTokens(in.apiFormat(), in.outBody)
		usage.CompletionTokens = an.EstimateOutputTokens()
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.Estimated = true
	}
	res.usage = usage
	res.model = an.Model()
	res.responseID = an.ResponseID()
	res.responseStore = an.ResponseStore()
	res.responseOutput = an.ResponseOutput()
	res.analyzerErr = an.ErrMsg()
	res.contentSeen = an.HasContent()
	res.streaming = an.Streaming()
	res.streamFinished = an.StreamFinished()
	if in.apiFormat() == model.FormatResponses {
		res.responseStatus = an.FinishReason()
		res.terminalDelivered = terminalDelivered
		if res.streaming && !compressed && !res.streamFinished && res.analyzerErr == "" {
			res.analyzerErr = "流式响应未正常结束（上游提前关闭连接）"
		}
	}

	if writeErr != nil {
		if isClientGone(r, writeErr) {
			res.cancelled = true
			res.errMsg = "客户端已断开连接"
		} else {
			res.errMsg = "写响应给客户端失败: " + writeErr.Error()
		}
		return res
	}
	if !terminalDelivered && isClientGone(r, nil) {
		res.cancelled = true
		// 客户端一断，正在读的上游流必然返回 context canceled。那是结果、不是原因，
		// 留着它日志里就全是「读取上游响应失败: context canceled」这种误导性文案。
		res.errMsg = "客户端已断开连接"
	}
	return res
}

// logResult 汇总一次转发结束时才知道的指标。
type logResult struct {
	status            int
	errMsg            string
	cancelled         bool
	upstream          string
	proxyDesc         string
	headers           http.Header
	usage             Usage
	model             string
	ttftMs            int64
	appliedTTFT       bool
	respBody          string
	responseModified  bool
	analyzerErr       string
	contentSeen       bool
	streaming         bool
	streamFinished    bool
	responseStatus    string
	terminalDelivered bool
	responseID        string
	responseStore     *bool
	responseOutput    []json.RawMessage
}

// record 把结果落库并广播给管理界面。
func (s *Server) record(in forwardInput, res logResult) {
	end := time.Now()
	total := end.Sub(in.start)

	entry := model.RequestLog{
		TSStart:         in.start,
		TSEnd:           end,
		ProviderID:      in.provider.ID,
		ProviderName:    in.provider.DisplayName,
		UpstreamURL:     res.upstream,
		ProxyUsed:       res.proxyDesc,
		Method:          in.method,
		Path:            in.clientPath,
		Model:           in.model,
		ModelMapped:     in.modelMapped,
		ModelResponse:   res.model,
		APIFormat:       string(in.apiFormat()),
		Stream:          in.wantStream,
		ReasoningEffort: extractReasoningEffort(in.apiFormat(), in.outBody),
		ClientIP:        in.clientIP,

		RequestModified:  len(in.modifications) > 0,
		ResponseModified: res.responseModified,
		Modifications:    in.modifications,

		ImageCompressed: in.imageCompressed,
		ImageSavedBytes: in.imageSavedBytes,
		ImagesDropped:   in.imagesDropped,

		HTTPStatus: res.status,
		Cancelled:  res.cancelled,
		ErrorMsg:   res.errMsg,

		PromptTokens:     res.usage.PromptTokens,
		CachedTokens:     res.usage.CachedTokens,
		CacheWriteTokens: res.usage.CacheWriteTokens,
		CompletionTokens: res.usage.CompletionTokens,
		ReasoningTokens:  res.usage.ReasoningTokens,
		TotalTokens:      res.usage.TotalTokens,
		TokensEstimated:  res.usage.Estimated,

		TotalMs: total.Milliseconds(),
	}
	if entry.Modifications == nil {
		entry.Modifications = []string{}
	}
	if in.continuation != nil {
		entry.ContinuationMode = in.continuation.mode
		entry.ContinuationReason = in.continuation.reason
		entry.SavedBytes = in.continuation.savedBytes
	}
	// 非流式请求没有「首 token」这个概念，保持 0。
	if res.appliedTTFT {
		entry.TTFTMs = res.ttftMs
	}
	// TPS 只在流式响应下计算。非流式响应拿不到逐 token 的到达时刻，
	// 用「输出量 ÷ 总耗时」得到的是延迟的倒数而不是生成速度，
	// 一个 4ms 返回的短响应会算出几千 TPS，把平均值完全带偏，反而失去参考价值。
	// 流式响应也可能量不出速度（上游整流下发），那种情况由 computeTPS 判为 0。
	if res.streaming {
		entry.TPS = computeTPS(res.usage.CompletionTokens, entry.TTFTMs, entry.TotalMs)
	}

	// 失败原因可能只体现在响应体或流的中断上，这里挑最具体的一条。
	if entry.ErrorMsg == "" {
		entry.ErrorMsg = res.analyzerErr
	}
	if entry.ErrorMsg == "" && res.streaming && res.contentSeen && !res.streamFinished {
		entry.ErrorMsg = "流式响应未正常结束（上游提前关闭连接）"
	}
	// 兜底：上游明确报错（4xx/5xx）但响应体不是我们认识的错误结构（例如网关返回
	// HTML 错误页）时，至少让日志说明是哪个状态码，而不是一条红色但没有原因的记录。
	if entry.ErrorMsg == "" && entry.HTTPStatus >= 400 {
		entry.ErrorMsg = fmt.Sprintf("上游返回 HTTP %d（响应体未给出错误描述）", entry.HTTPStatus)
	}
	entry.Success = isSuccess(res, entry.ErrorMsg)

	if in.settings.StoreBodies {
		entry.ReqHeaders = sanitizeHeaders(res.headers)
		entry.ReqBody = truncate(in.outBody, in.settings.MaxBodyBytes)
		if len(in.modifications) > 0 {
			entry.ReqBodyOriginal = truncate(in.reqBody, in.settings.MaxBodyBytes)
		}
		entry.RespBody = res.respBody
	}

	id, err := s.store.InsertLog(&entry)
	if err != nil {
		s.logger.Error("写入请求日志失败", "err", err)
	}
	entry.ID = id
	if in.apiFormat() == model.FormatResponses {
		s.logger.Debug("Responses 转发结束", "request_id", entry.ID, "response_status", res.responseStatus,
			"terminal_delivered", res.terminalDelivered, "http_status", res.status,
			"success", entry.Success, "cancelled", entry.Cancelled, "error", entry.ErrorMsg)
	}

	s.hub.Broadcast("log", entry)
}

// failEarly 处理还没有定位到供应商的失败，同样留下日志。
func (s *Server) failEarly(w http.ResponseWriter, in forwardInput, status int, msg string) {
	if in.continuation != nil {
		s.continuation.fail(in.continuation)
	}
	s.logger.Warn("转发被拒绝", "status", status, "msg", msg, "path", in.clientPath)
	s.record(in, logResult{status: status, errMsg: msg})
	writeProxyError(w, status, msg)
}

// failWith 处理已定位供应商但未发出上游请求的失败。
func (s *Server) failWith(w http.ResponseWriter, in forwardInput, status int, msg string) {
	s.failEarly(w, in, status, msg)
}

// writeProxyError 输出与本程序风格一致的 JSON 错误。
func writeProxyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-AI-Proxy-Error", "1")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%s,"type":"ai_proxy_error","code":%d}}`,
		strconv.Quote(msg), status)
}

// ---------- 路由解析 ----------

// resolveProvider 决定本次请求交给哪个供应商，以及发往上游的路径。
//
//   - /p/<短名>/...  显式指定，方便同机多客户端各走各的；
//   - 其余路径        一律交给「当前生效」的供应商，客户端 base_url 无需改动。
func (s *Server) resolveProvider(r *http.Request) (model.Provider, string, error) {
	path := r.URL.EscapedPath()

	if rest, ok := strings.CutPrefix(path, "/p/"); ok {
		name, tail, _ := strings.Cut(rest, "/")
		if name == "" {
			return model.Provider{}, "", errors.New("路径 /p/ 后面缺少供应商短名")
		}
		prov, err := s.store.GetProviderByName(name)
		if err != nil {
			return model.Provider{}, "", fmt.Errorf("找不到供应商 %q", name)
		}
		if !prov.Enabled {
			return model.Provider{}, "", fmt.Errorf("供应商 %q 已禁用", prov.DisplayName)
		}
		return prov, "/" + tail, nil
	}

	prov, err := s.store.GetActiveProvider()
	if err != nil {
		return model.Provider{}, "", errors.New("还没有配置任何可用的供应商，请先在管理界面添加并启用")
	}
	if !prov.Enabled {
		return model.Provider{}, "", fmt.Errorf("当前生效的供应商 %q 已禁用", prov.DisplayName)
	}
	return prov, path, nil
}

// upstreamEndpointPath 返回判定「上游端点形状」时该看的路径。
//
// 配了 CustomPath 的供应商，客户端路径被完全接管 —— 发出去的是 CustomPath，
// 客户端打的是什么路径都无关紧要。这时必须看 CustomPath，否则
// prompt_cache_key 的端点判定会按一个根本没被使用的路径下结论。
func upstreamEndpointPath(prov model.Provider, upstreamPath string) string {
	if p := strings.TrimSpace(prov.CustomPath); p != "" {
		return p
	}
	return upstreamPath
}

// buildUpstreamURL 把 BaseURL 与客户端请求路径拼成上游地址。
//
// 难点在于去重。客户端发的是 /v1/chat/completions，而 BaseURL 可能写成
// 三种形态：https://api.openai.com、https://api.openai.com/v1、
// 或者带自有前缀的 https://gw.example.com/api/openai/v1。后两种都会和请求
// 路径的头部重复一段。
//
// 解法是在「路径段」粒度上找最长重叠：取 BaseURL 路径的一段后缀，与请求
// 路径的一段前缀逐段比对，取最长的相等前缀，把重复的那几段去掉再拼。
// 对上面三种形态分别得到 /v1/chat/completions、/v1/chat/completions、
// /api/openai/v1/chat/completions，v1、v1beta 等版本段都能覆盖。
func buildUpstreamURL(prov model.Provider, escapedPath, rawQuery string) (string, error) {
	base, err := url.Parse(prov.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("供应商 %q 的 BaseURL 无效: %q", prov.DisplayName, prov.BaseURL)
	}
	basePath := strings.TrimSuffix(base.Path, "/")

	var tail string
	if prov.CustomPath != "" {
		// 自定义路径完全接管：忽略客户端路径，但仍保留 BaseURL 自带的前缀。
		tail = prov.CustomPath
		if !strings.HasPrefix(tail, "/") {
			tail = "/" + tail
		}
	} else {
		tail = suffixAfterOverlap(basePath, escapedPath)
	}

	target := base.Scheme + "://" + base.Host + basePath + tail
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	if _, err := url.Parse(target); err != nil {
		return "", fmt.Errorf("拼接上游地址失败: %w", err)
	}
	return target, nil
}

// suffixAfterOverlap 返回需要追加到 BaseURL 路径后面的那一段。
//
// 在「段」粒度上找最长重叠：若 BaseURL 路径的最后 k 段恰好等于请求路径的
// 前 k 段，就认为这段是重复的，从请求路径里去掉。k 取最大的那个。
// 举几个实际会遇到的例子（左边是 BaseURL 路径，右边是客户端请求路径）：
//
//	""                  + /v1/chat/completions -> /v1/chat/completions
//	"/v1"               + /v1/chat/completions -> /chat/completions
//	"/api/openai/v1"    + /v1/chat/completions -> /chat/completions
//	"/v1beta"           + /v1beta/models/x     -> /models/x
//	"/v1"               + /v1                  -> （空，直接用 BaseURL）
func suffixAfterOverlap(basePath, reqPath string) string {
	reqSegs := splitPath(reqPath)
	if basePath == "" {
		if len(reqSegs) == 0 {
			return "/"
		}
		return "/" + strings.Join(reqSegs, "/")
	}

	baseSegs := splitPath(basePath)
	limit := len(baseSegs)
	if len(reqSegs) < limit {
		limit = len(reqSegs)
	}
	overlap := 0
	for k := limit; k > 0; k-- {
		if equalSegments(baseSegs[len(baseSegs)-k:], reqSegs[:k]) {
			overlap = k
			break
		}
	}

	rest := reqSegs[overlap:]
	if len(rest) == 0 {
		// 请求路径正好就是 BaseURL 的路径（例如裸请求 /v1），不需要追加。
		return ""
	}
	return "/" + strings.Join(rest, "/")
}

// splitPath 把路径切成非空段，保留原始转义形式。
func splitPath(p string) []string {
	out := []string{}
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func equalSegments(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------- 请求头处理 ----------

// hopByHopHeaders 是不应被代理转发的逐跳头（RFC 7230 §6.1）。
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Connection":    true,
}

// buildUpstreamHeaders 生成本次发往上游的请求头。
//
// 四件事：剥掉逐跳头、抹掉本代理自己的访问密钥、按供应商配置写入认证头、补上自定义头。
// 另外主动去掉 Accept-Encoding —— 压缩后的响应无法旁路解析用量，
// 而用量统计是本程序的核心功能。这一项对上游是良性的标准请求头调整。
func buildUpstreamHeaders(r *http.Request, prov model.Provider, key model.APIKey, accessKey string) http.Header {
	out := make(http.Header, len(r.Header)+len(prov.ExtraHeaders)+2)

	// 请求自带的 Connection 头里列出的字段同样要剔除。
	extraHop := map[string]bool{}
	for _, v := range r.Header.Values("Connection") {
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				extraHop[http.CanonicalHeaderKey(item)] = true
			}
		}
	}

	for name, values := range r.Header {
		canonical := http.CanonicalHeaderKey(name)
		if hopByHopHeaders[canonical] || extraHop[canonical] || canonical == "Host" {
			continue
		}
		if canonical == "Accept-Encoding" {
			continue
		}
		out[canonical] = append([]string(nil), values...)
	}

	// 客户端为了通过本代理的鉴权，常把代理访问密钥填进 API key 字段。
	// 那是给本代理看的，绝不能转发给上游 —— 供应商没配密钥（「客户端持钥」模式）时
	// 尤其危险，会把代理密钥直接泄露给上游。
	// 始终剔除 X-AI-Proxy-Key；其余头仅在值精确等于访问密钥或 "Bearer <访问密钥>" 时删除，
	// 避免较短的访问密钥误伤 User-Agent 等普通请求头。
	out.Del("X-AI-Proxy-Key")
	if accessKey != "" {
		for name, values := range out {
			for _, v := range values {
				trimmed := strings.TrimSpace(v)
				if constantTimeEqual(trimmed, accessKey) ||
					isBearerOf(trimmed, accessKey) {
					out.Del(name)
					break
				}
			}
		}
	}

	// 只有配置了密钥才覆盖认证头；否则放行客户端的认证信息，
	// 这样「客户端自带 key、代理只做记账」的用法也能成立。
	if key.Key != "" {
		header, prefix := prov.AuthHeaderFor()
		out.Set(header, prefix+key.Key)
	}
	for name, value := range prov.ExtraHeaders {
		if name == "" {
			continue
		}
		out.Set(name, value)
	}
	return out
}

// copyResponseHeaders 把上游响应头写给客户端。
// dropContentLength 为 true 表示后续会改写响应体（且响应未压缩），必须丢弃 Content-Length。
func copyResponseHeaders(dst, src http.Header, dropContentLength bool) {
	extraHop := map[string]bool{}
	for _, v := range src.Values("Connection") {
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				extraHop[http.CanonicalHeaderKey(item)] = true
			}
		}
	}
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if hopByHopHeaders[canonical] || extraHop[canonical] {
			continue
		}
		if dropContentLength && canonical == "Content-Length" {
			continue
		}
		dst[canonical] = append([]string(nil), values...)
	}
}

// hasNonIdentityEncoding 判断响应是否被压缩（压缩后无法旁路解析用量）。
func hasNonIdentityEncoding(encoding string) bool {
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	return encoding != "" && encoding != "identity"
}

// ---------- 辅助 ----------

// capture 按设置截断地保存报文，用于落库。
type capture struct {
	enabled bool
	limit   int
	buf     []byte
	over    bool
}

func newCapture(enabled bool, limit int) *capture {
	if limit <= 0 {
		limit = 64 * 1024
	}
	return &capture{enabled: enabled, limit: limit}
}

func (c *capture) add(p []byte) {
	if !c.enabled || c.over {
		return
	}
	remain := c.limit - len(c.buf)
	if remain <= 0 {
		c.over = true
		return
	}
	if len(p) > remain {
		c.buf = append(c.buf, p[:remain]...)
		c.over = true
		return
	}
	c.buf = append(c.buf, p...)
}

func (c *capture) string() string {
	if len(c.buf) == 0 {
		return ""
	}
	buf := c.buf
	if c.over {
		buf = trimInvalidUTF8Tail(buf)
	}
	s := string(buf)
	if c.over {
		s += "\n…（内容超出上限，已截断）"
	}
	return s
}

func truncate(b []byte, limit int) string {
	if len(b) == 0 {
		return ""
	}
	if limit <= 0 || len(b) <= limit {
		return string(b)
	}
	return string(trimInvalidUTF8Tail(b[:limit])) + "\n…（内容超出上限，已截断）"
}

// trimInvalidUTF8Tail 去掉截断末尾可能残留的不完整 UTF-8 字节序列。
func trimInvalidUTF8Tail(b []byte) []byte {
	n := len(b)
	// UTF-8 字符最多 4 字节，最多往回退 3 字节就能对齐到合法 rune 边界。
	for i := 1; i <= 3 && i <= n; i++ {
		if utf8.RuneStart(b[n-i]) {
			if utf8.Valid(b[n-i:]) {
				return b
			}
			return b[:n-i]
		}
	}
	return b
}

// sanitizeHeaders 把请求头渲染成可读文本，并抹掉凭据。
func sanitizeHeaders(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	var sb strings.Builder
	for name, values := range h {
		for _, v := range values {
			sb.WriteString(name)
			sb.WriteString(": ")
			sb.WriteString(maskSecret(name, v))
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// maskSecret 对认证类头部做脱敏，只保留可辨识的前后几位。
func maskSecret(name, value string) string {
	lower := strings.ToLower(name)
	sensitive := strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "cookie")
	if !sensitive || value == "" {
		return value
	}
	// 保留 "Bearer " 这类前缀，方便确认用的是哪种认证方式。
	prefix := ""
	if i := strings.IndexByte(value, ' '); i > 0 && i < 12 {
		prefix = value[:i+1]
		value = value[i+1:]
	}
	if len(value) <= 8 {
		return prefix + "****"
	}
	return prefix + value[:4] + "****" + value[len(value)-4:]
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func constantTimeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// isBearerOf 判断 value 是否是 "Bearer <token>"（前缀大小写不敏感）。
//
// 认证方案名按 RFC 7235 是大小写不敏感的，客户端写 "bearer xxx" 也得认；
// 常量时间比较只用于 token 本身。
func isBearerOf(value, token string) bool {
	const prefix = "Bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	return constantTimeEqual(strings.TrimSpace(value[len(prefix):]), token)
}

// stripProxyKeyFromURL 删掉查询串里值等于代理访问密钥的参数。
//
// Gemini 系的客户端习惯把密钥放在 ?key=…（也有 api_key / token 的写法）。
// 客户端把「代理访问密钥」填进去时，不删就会连 URL 一起转发给上游，
// 并原样写进请求日志。只删值完全相等的参数，上游自己需要的 key 不受影响。
func stripProxyKeyFromURL(raw, accessKey string) string {
	if accessKey == "" || raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for name, values := range q {
		kept := make([]string, 0, len(values))
		removed := false
		for _, v := range values {
			if constantTimeEqual(v, accessKey) {
				removed = true
				continue
			}
			kept = append(kept, v)
		}
		if !removed {
			continue
		}
		changed = true
		if len(kept) == 0 {
			q.Del(name)
		} else {
			q[name] = kept
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// authorized 在配置了访问密钥时校验请求。
// 兼容 OpenAI（Authorization: Bearer）、Anthropic（x-api-key）、Gemini（x-goog-api-key）
// 与自定义头（X-AI-Proxy-Key）。
func (s *Server) authorized(r *http.Request, st model.Settings) bool {
	if st.AccessKey == "" {
		return true
	}
	for _, h := range []string{"X-AI-Proxy-Key", "x-api-key", "x-goog-api-key"} {
		if constantTimeEqual(r.Header.Get(h), st.AccessKey) {
			return true
		}
	}
	if auth := r.Header.Get("Authorization"); isBearerOf(auth, st.AccessKey) {
		return true
	}
	return false
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limited := io.LimitReader(r.Body, maxRequestBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("读取请求体失败: %w", err)
	}
	if len(body) > maxRequestBodyBytes {
		return nil, fmt.Errorf("请求体超过 %d MB 上限", maxRequestBodyBytes>>20)
	}
	return body, nil
}

// modelNameForRequest 读出本次请求的模型名。
//
// 除 Gemini 外的格式都把模型名放在请求体里，直接读体即可；Gemini 的模型名在 URL 路径上
// （/v1beta/models/<model>:generateContent），请求体里没有这个字段，只读请求体会让这些请求
// 在模型排行、实时监控和「按模型匹配的提示词规则」里一起落空。
func modelNameForRequest(format model.APIFormat, path string, body []byte) string {
	if format == model.FormatGemini {
		if m := geminiModelFromPath(path); m != "" {
			return m
		}
	}
	return modelNameFromBody(body)
}

// geminiModelFromPath 从 /models/<模型名>:<方法> 里取出模型名，取不到返回空串。
//
// 从最后一个 /models/ 开始找：Vertex AI 形态的路径是
// /v1/projects/<p>/locations/<l>/publishers/google/models/<model>:generateContent，
// 取最后一个才拿得到模型名。
func geminiModelFromPath(path string) string {
	const marker = "/models/"
	idx := strings.LastIndex(path, marker)
	if idx < 0 {
		return ""
	}
	rest := path[idx+len(marker):]
	end := strings.IndexByte(rest, ':')
	if end <= 0 {
		return ""
	}
	// 模型名后面紧跟 :方法名；中间出现斜杠说明这一段不是模型名。
	model := rest[:end]
	if strings.Contains(model, "/") {
		return ""
	}
	return model
}

// replaceGeminiModelInPath 把路径里 /models/<from> 的模型段换成 to。
//
// 定位规则与 geminiModelFromPath 完全一致（最后一个 /models/，取到冒号为止），
// 只在确实解析出与 from 相同的模型名时才替换，否则原样返回 —— 路径结构一旦不符合
// 预期，宁可让模型映射对这条请求失效，也不要乱改路径。
func replaceGeminiModelInPath(path, from, to string) string {
	const marker = "/models/"
	idx := strings.LastIndex(path, marker)
	if idx < 0 {
		return path
	}
	start := idx + len(marker)
	rest := path[start:]
	end := strings.IndexByte(rest, ':')
	if end <= 0 {
		return path
	}
	model := rest[:end]
	if model != from || strings.Contains(model, "/") {
		return path
	}
	return path[:start] + to + rest[end:]
}

// requestWantsStream 判断客户端是否要求流式响应。
//
// Gemini 不认请求体里的 stream 字段：流式写在路径的 :streamGenerateContent 上，
// 或者写成 ?alt=sse。这里只判断「客户端的意图」，响应到底是不是流式由响应头决定
// （TPS 的计算走的是那条判断）。
func requestWantsStream(format model.APIFormat, path, rawQuery string, body []byte) bool {
	if format == model.FormatGemini && geminiWantsStream(path, rawQuery) {
		return true
	}
	if len(body) == 0 {
		return false
	}
	obj, err := decodeJSONObject(body)
	if err != nil {
		return false
	}
	v, _ := obj["stream"].(bool)
	return v
}

// geminiWantsStream 识别 Gemini 的两种流式写法：路径上的 :streamGenerateContent，以及 alt=sse。
func geminiWantsStream(path, rawQuery string) bool {
	if strings.Contains(path, ":streamGenerateContent") {
		return true
	}
	if rawQuery == "" {
		return false
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	return strings.EqualFold(q.Get("alt"), "sse")
}

// maxPlausibleTPS 是单条流式响应可以采信的输出速度上限（token/秒）。
//
// 超过它的数不算「很快」，而是根本没量到吐字速度：上游把整流攒齐后一次性下发时，
// 「总耗时 - 首 token」会塌成几毫秒，58 个 token 就能算出 19000 t/s。
// 2026-10-13 实测 opencode zen 对「reasoning_tokens=0 的小 tool_calls 响应」就是这样
// （响应体里 35~66 个 SSE 事件全部挤在首内容事件之后的 3~78ms 内到达，
// 同一时段的正常流式行窗口是 448~1312ms），commandcode 等其它上游也偶发。
//
// 但这个上限只在「生成窗口塌陷」时才生效，判据见 minPlausibleMsPerToken：
// 光看速度绝对值会把真实的高速流一起误杀。
const maxPlausibleTPS = 500

// minPlausibleMsPerToken 是「每个输出 token 至少占用的生成时间」（毫秒）。
//
// 整流下发的本质不是「算出来的速度大」，而是「传输时间与 token 数脱钩」：
// 上游在服务端把整条流攒完，再一次性灌进 socket，于是生成窗口只反映一次
// 网络传输的耗时（几毫秒到一百多毫秒），与 token 数无关。真实流式则相反，
// 每个 token 都要过一次模型前向，窗口随 token 数线性增长。
//
// 2026-11-15 用全库 851 条流式行校准（data/ai_proxy.db）：
//   - 每 token 耗时 ≥1.0ms 的有 789 条，占绝对多数，对应 1~500 t/s 的常规吐字；
//   - 0.5~1.0ms 的 31 条是高速上游（≈1000~2000 t/s）的正常表现；
//   - <0.5ms 的 16 条几乎全是整流下发，其中 10 条超过 5000 t/s（最高 23000 t/s，
//     即 46 个 token 落在 2ms 里），这些值物理上不可能是逐 token 生成的结果。
//
// 取 0.5ms/token（等价于 2000 t/s）作为分界：它把上述 16 条整流下发行全部拦下，
// 同时给 Groq、Cerebras 这类单流上千 tok/s 的真实高速推理留出余量。
// 将来接入更快的内核时需要重新校准这个值。
const minPlausibleMsPerToken = 0.5

// minTPSMeasureTokens 是「值得计算吐字速度」的最小输出量。
//
// token 数太少时，生成窗口主要由网络往返与事件间隔决定，而不是模型前向：
// 库里 8 个 token 的响应窗口从 2ms 到 581ms 连续分布、毫无断层（对应 4000~13.8 t/s），
// 同一批响应里 reasoning_tokens 全是 8，即只吐了思维链就结束。这种短响应的
// 「速度」在两个方向上都不具参考价值，按不可测处理，与「非流式不算 TPS」同口径。
//
// 取 20：低于它的行只占全库 4%（35/851），且几乎都是纯 reasoning 的短收尾；
// 保留它们只会让平均值被网络抖动带偏。
const minTPSMeasureTokens = 20

// computeTPS 计算生成速度，即每秒输出 token 数。
//
// 分母是「总耗时 - 首 token 等待」，反映模型真正的吐字速度，
// 而不是把排队与预填充的时间也算进去。
//
// 四种算不出速度的情况一律返回 0，与「非流式不计算 TPS」一样按不可测处理：
// 界面显示为 —，聚合统计也因为只取 tps > 0 而不会把它算进平均值。
//   - 拿不到首 token 时刻（ttftMs <= 0）：流式但没识别出内容，退回用总耗时做分母，
//     得到的是吞吐而非吐字速度；这种值也要守住可信上限。
//   - 首 token 与响应结束落在同一毫秒：整条流是一次性到的，量不出速度。
//   - 输出量少于 minTPSMeasureTokens：窗口由网络抖动主导，速度不具参考价值。
//   - 每 token 耗时低于 minPlausibleMsPerToken 且速度超过 maxPlausibleTPS：
//     整流下发的特征，量不出速度。
//
// 最后一条必须两个判据同时成立。只看速度会误杀真实高速流：2026-11-15
// 库里有 21980 个 token 在 32136ms 内持续到达（684 t/s）、18845 个 token 用了
// 35809ms（526 t/s），这些是实打实的持续吐字，不是整流下发，此前被 500 的
// 单一阈值全部判成 0，导致最近 200 条里 166 条 TPS 显示「—」。
func computeTPS(completionTokens int, ttftMs, totalMs int64) float64 {
	if completionTokens <= 0 || totalMs <= 0 {
		return 0
	}
	// 输出太少时窗口由网络往返与事件间隔主导，算出来的不是吐字速度。
	if completionTokens < minTPSMeasureTokens {
		return 0
	}

	var tps float64
	var windowMs int64
	if ttftMs <= 0 {
		windowMs = totalMs
		tps = float64(completionTokens) / (float64(totalMs) / 1000)
	} else {
		windowMs = totalMs - ttftMs
		if windowMs <= 0 {
			return 0
		}
		tps = float64(completionTokens) / (float64(windowMs) / 1000)
	}
	// 两个判据同时成立才判为整流下发：速度快，且每个 token 占用的时间短到
	// 不可能是逐 token 生成的结果。窗口够长时的高速度是真实吞吐，予以保留。
	if tps > maxPlausibleTPS && float64(windowMs)/float64(completionTokens) < minPlausibleMsPerToken {
		return 0
	}
	return tps
}

// isSuccess 判定一次请求是否算成功。
//
// 标准比「HTTP 2xx」更严：还必须没有错误信息 —— 这包含上游返回 200 但响应体里
// 报错、以及流式响应中途被切断两种情况。否则一个断在半路的 200 会被记成成功，
// 成功率这个指标就失去意义了。
func isSuccess(res logResult, errMsg string) bool {
	if res.cancelled || res.status < 200 || res.status >= 300 {
		return false
	}
	return errMsg == ""
}

// isClientGone 判断错误是否来自客户端断开。
func isClientGone(r *http.Request, err error) bool {
	if r != nil {
		return errors.Is(r.Context().Err(), context.Canceled)
	}
	return errors.Is(err, context.Canceled)
}
