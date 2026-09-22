// Package proxy 实现请求的透明转发与全程记账。
//
// 核心承诺：除了用户显式开启的请求体改写（usage 注入、提示词注入）之外，
// 请求与响应都按原字节透传，不做格式转换、不做字段裁剪。
package proxy

import (
	"context"
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
	store   *store.Store
	hub     *hub.Hub
	live    *LiveRegistry
	clients *clientPool
	keys    *keyPool
	logger  *slog.Logger

	idSeq atomic.Uint64
}

// NewServer 构造转发服务。
func NewServer(st *store.Store, h *hub.Hub, logger *slog.Logger) *Server {
	return &Server{
		store:   st,
		hub:     h,
		live:    NewLiveRegistry(),
		clients: newClientPool(),
		keys:    newKeyPool(),
		logger:  logger,
	}
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

	in.outBody, in.modifications = rewriteRequestBody(prov, settings, reqBody)

	s.forward(w, r, in)
}

// forwardInput 汇总一次转发的输入，避免 forward 参数过长。
type forwardInput struct {
	start         time.Time
	settings      model.Settings
	provider      model.Provider
	method        string
	clientPath    string
	clientIP      string
	upstreamPath  string
	reqBody       []byte // 客户端原始请求体
	outBody       []byte // 改写后实际发出的请求体
	modifications []string
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, in forwardInput) {
	prov := in.provider

	target, err := buildUpstreamURL(prov, in.upstreamPath, r.URL.RawQuery)
	if err != nil {
		s.failWith(w, in, http.StatusBadGateway, err.Error())
		return
	}

	key, _ := s.keys.Pick(prov)
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
		Model:        modelNameFromBody(in.outBody),
		Path:         in.clientPath,
		Stream:       requestWantsStream(in.outBody),
		ClientIP:     in.clientIP,
	})
	defer s.live.Remove(liveID)

	resp, err := client.Do(upReq)
	if err != nil {
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
		return
	}
	defer resp.Body.Close()

	// 按状态码决定是否让该密钥进入冷却。
	s.keys.Report(prov.ID, key.ID, resp.StatusCode, "")

	copyResponseHeaders(w.Header(), resp.Header, prov.StripUsageChunk)
	w.WriteHeader(resp.StatusCode)

	res := s.relay(w, r, resp, in, liveID)
	res.status = resp.StatusCode
	res.upstream = target
	res.proxyDesc = netcfg.ProxyDesc
	res.headers = headers
	s.record(in, res)
}

// relay 把上游响应原样写给客户端，同时旁路收集统计指标。
func (s *Server) relay(w http.ResponseWriter, r *http.Request, resp *http.Response, in forwardInput, liveID string) logResult {
	prov := in.provider
	var res logResult

	// 上游用了压缩时无法旁路解析用量，直接退化为估算。
	compressed := hasNonIdentityEncoding(resp.Header.Get("Content-Encoding"))
	an := newAnalyzer(prov.APIFormat, prov.CustomUsage, resp.Header.Get("Content-Type"))

	capture := newCapture(in.settings.StoreBodies, in.settings.MaxBodyBytes)

	buf := make([]byte, copyBufferSize)
	flusher, canFlush := w.(http.Flusher)

	var (
		ttft       time.Duration
		ttftSeen   bool
		events     int
		writeErr   error
		respModded bool
	)

	if an.Streaming() && !compressed {
		framer := newSSEFramer(func(raw []byte, event, data string) {
			if writeErr != nil {
				return
			}
			// 「剥离 usage 块」只影响回传给客户端的字节；统计该看的一个都不能少，
			// 否则开了这个开关用量就退化成估算值了。
			if prov.StripUsageChunk && an.IsUsageOnlyChunk(event, data) {
				respModded = true
			} else if _, err := w.Write(raw); err != nil {
				writeErr = err
				return
			} else {
				capture.add(raw)
			}
			an.OnEvent(event, data)
			if !ttftSeen && an.HasContent() {
				ttftSeen = true
				ttft = time.Since(in.start)
				s.live.MarkFirstToken(liveID)
			}
			// 每个内容事件都去抢一次锁不划算，隔几个事件更新一次实时值即可。
			if events++; events%8 == 0 {
				s.live.UpdateTokens(liveID, an.EstimateOutputTokens())
			}
			if canFlush {
				flusher.Flush()
			}
		})

		if canFlush {
			flusher.Flush() // 立刻把响应头推给客户端，让客户端能尽早开始计时
		}
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				framer.Feed(buf[:n])
				if writeErr != nil {
					break
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					res.errMsg = "读取上游响应失败: " + readErr.Error()
				}
				break
			}
		}
		framer.Flush()
		if canFlush {
			flusher.Flush()
		}
	} else {
		if canFlush {
			flusher.Flush()
		}
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, err := w.Write(buf[:n]); err != nil {
					writeErr = err
					break
				}
				capture.add(buf[:n])
				if !compressed {
					an.Feed(buf[:n])
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					res.errMsg = "读取上游响应失败: " + readErr.Error()
				}
				break
			}
		}
		if canFlush {
			flusher.Flush()
		}
	}

	an.Finish()

	res.ttftMs = ttft.Milliseconds()
	res.appliedTTFT = ttftSeen
	res.responseModified = respModded
	res.respBody = capture.string()

	usage := an.Result()
	if !usage.Found {
		// 上游没给用量：输入从头估算，输出按流式增量估算。
		usage.PromptTokens = estimatePromptTokens(prov.APIFormat, in.outBody)
		usage.CompletionTokens = an.EstimateOutputTokens()
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.Estimated = true
	}
	res.usage = usage
	res.model = an.Model()
	res.analyzerErr = an.ErrMsg()
	res.contentSeen = an.HasContent()
	res.streaming = an.Streaming()
	res.streamFinished = an.StreamFinished()

	if writeErr != nil {
		if isClientGone(r, writeErr) {
			res.cancelled = true
			res.errMsg = "客户端已断开连接"
		} else {
			res.errMsg = "写响应给客户端失败: " + writeErr.Error()
		}
		return res
	}
	if isClientGone(r, nil) {
		res.cancelled = true
		if res.errMsg == "" {
			res.errMsg = "客户端已断开连接"
		}
	}
	return res
}

// logResult 汇总一次转发结束时才知道的指标。
type logResult struct {
	status           int
	errMsg           string
	cancelled        bool
	upstream         string
	proxyDesc        string
	headers          http.Header
	usage            Usage
	model            string
	ttftMs           int64
	appliedTTFT      bool
	respBody         string
	responseModified bool
	analyzerErr      string
	contentSeen      bool
	streaming        bool
	streamFinished   bool
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
		Model:           modelNameFromBody(in.outBody),
		ModelResponse:   res.model,
		APIFormat:       string(in.provider.APIFormat),
		Stream:          requestWantsStream(in.outBody),
		ReasoningEffort: extractReasoningEffort(in.provider.APIFormat, in.outBody),
		ClientIP:        in.clientIP,

		RequestModified:  len(in.modifications) > 0,
		ResponseModified: res.responseModified,
		Modifications:    in.modifications,

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
	// 非流式请求没有「首 token」这个概念，保持 0。
	if res.appliedTTFT {
		entry.TTFTMs = res.ttftMs
	}
	// TPS 只在流式响应下计算。非流式响应拿不到逐 token 的到达时刻，
	// 用「输出量 ÷ 总耗时」得到的是延迟的倒数而不是生成速度，
	// 一个 4ms 返回的短响应会算出几千 TPS，把平均值完全带偏，反而失去参考价值。
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

	s.hub.Broadcast("log", entry)
}

// failEarly 处理还没有定位到供应商的失败，同样留下日志。
func (s *Server) failEarly(w http.ResponseWriter, in forwardInput, status int, msg string) {
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
	// 尤其危险，会把代理密钥直接泄露给上游。这里不挑头名，凡是值里带着访问密钥的
	// 一律删掉。
	if accessKey != "" {
		for name, values := range out {
			for _, v := range values {
				if strings.Contains(v, accessKey) {
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
func copyResponseHeaders(dst, src http.Header, stripUsage bool) {
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
		// 剥离 usage 块会改变响应体长度，这两个头必须丢弃。
		if stripUsage && (canonical == "Content-Length" || canonical == "Content-Encoding") {
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
	s := string(c.buf)
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
	return string(b[:limit]) + "\n…（内容超出上限，已截断）"
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

// authorized 在配置了访问密钥时校验请求。
func (s *Server) authorized(r *http.Request, st model.Settings) bool {
	if st.AccessKey == "" {
		return true
	}
	if r.Header.Get("X-AI-Proxy-Key") == st.AccessKey {
		return true
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if strings.TrimPrefix(auth, "Bearer ") == st.AccessKey {
			return true
		}
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

// requestWantsStream 读取请求里的 stream 字段。
func requestWantsStream(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	obj, err := decodeJSONObject(body)
	if err != nil {
		return false
	}
	if v, ok := obj["stream"].(bool); ok {
		return v
	}
	// Gemini 用 ?alt=sse 表达流式，请求体里没有 stream 字段，
	// 这里不做推断，交给响应 Content-Type 判断。
	return false
}

// computeTPS 计算生成速度，即每秒输出 token 数。
//
// 分母是「总耗时 - 首 token 等待」，反映模型真正的吐字速度，
// 而不是把排队与预填充的时间也算进去。ttftMs 为 0 时（拿不到首 token 时刻）
// 退化为用总耗时做分母。
func computeTPS(completionTokens int, ttftMs, totalMs int64) float64 {
	if completionTokens <= 0 || totalMs <= 0 {
		return 0
	}
	denom := totalMs - ttftMs
	if ttftMs <= 0 || denom <= 0 {
		denom = totalMs
	}
	return float64(completionTokens) / (float64(denom) / 1000)
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
	if errors.Is(err, context.Canceled) {
		return true
	}
	if r != nil && r.Context().Err() != nil {
		return true
	}
	return false
}
