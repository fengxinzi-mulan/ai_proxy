package proxy

// Responses 续传的真实上游实验，默认跳过，只在显式开启时运行。
//
// 目的：验证「store:true + previous_response_id + 只发增量」能否在 10+ 张图片的
// 会话里显著降低第二轮首 token 时间，并且模型确实能回忆第一轮的图片内容。
//
// 判定门槛（写进日志，不直接失败）：第二轮中位 TTFT 降 ≥20% 且 ≥1s，
// 回忆全部正确，无新增 4xx/5xx。达不到就不移植。
//
//	CONTINUATION_LIVE=1           OPENCODE_API_KEY=oc_sk_xxx \
//	CONTINUATION_LIVE_IMAGES="E:\Pictures\Saved Pictures" \
//	  go test ./internal/proxy -run TestLiveOpenCodeResponsesContinuation -v
//
// 未设 OPENCODE_API_KEY 时只读打开 data/ai_proxy.db 取 opencode 供应商的密钥与
// BaseURL；不写生产库、不改运行中的配置。图片默认在内存里生成 12 张纯色图，
// 需要真实截图时用 CONTINUATION_LIVE_IMAGES 指定目录或逗号分隔的文件列表。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"

	_ "modernc.org/sqlite"
)

const continuationImageCount = 12

// continuationImage 是一张待上传的图片及其可验证的预期颜色。
// 用真实截图时 expected 为空，回忆校验退化为「全量轮与续传轮答案一致」。
type continuationImage struct {
	url      string
	expected string
}

type continuationUsage struct {
	input     int
	output    int
	cached    int
	reasoning int
	found     bool
}

type continuationResult struct {
	status    int
	body      string
	id        string
	store     *bool
	respState string
	output    []json.RawMessage
	text      string
	ttft      time.Duration
	firstText time.Duration
	total     time.Duration
	sentBytes int
	events    int
	usage     continuationUsage
	errMsg    string
}

type continuationRun struct {
	rep        int
	mode       string
	status     int
	ttft       time.Duration
	total      time.Duration
	sentBytes  int
	usage      continuationUsage
	recallOK   bool
	recallNote string
	answer     string
}

func TestLiveOpenCodeResponsesContinuation(t *testing.T) {
	if os.Getenv("CONTINUATION_LIVE") != "1" {
		t.Skip("未设置 CONTINUATION_LIVE=1，跳过真实上游续传实验")
	}

	key, baseURL, settings := liveOpenCodeResponsesTarget(t)
	modelName := os.Getenv("RESPONSES_LIVE_MODEL")
	if modelName == "" {
		modelName = "deepseek-v4.1-flash"
	}

	prov := model.Provider{
		Name:      "live-opencode-responses",
		Enabled:   true,
		BaseURL:   baseURL,
		APIFormat: model.FormatResponses,
		Keys:      []model.APIKey{{ID: "live", Key: key, Enabled: true}},
	}
	prov.ApplyDefaults()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	netcfg := resolveNet(prov, settings)
	client, err := NewServer(st, hub.New(), quietLogger()).clients.client(netcfg)
	if err != nil {
		t.Fatalf("初始化 client 失败: %v", err)
	}
	target, err := buildUpstreamURL(prov, "/v1/responses", "")
	if err != nil {
		t.Fatalf("拼接上游地址失败: %v", err)
	}
	t.Logf("上游=%s 代理=%v(%s) 模型=%s 图片=%d", target, netcfg.ProxyInUse, netcfg.ProxyDesc, modelName, continuationImageCount)

	realImages, imageSource := continuationImages(t)
	t.Logf("图片来源=%s", imageSource)

	var runs []continuationRun
	var turn1Results []continuationResult
	gateBlocked := ""

	const reps = 3
	for rep := 1; rep <= reps; rep++ {
		session := fmt.Sprintf("continuation-live-%d-%d", time.Now().UnixNano(), rep)
		headers := continuationHeaders(prov, session, true)
		imgs := generatedImagesForRep(t, rep)
		if realImages != nil {
			imgs = imagesForRep(realImages, rep)
		}

		body1, input1 := continuationTurn1Body(modelName, session, imgs)
		first := sendContinuationRequest(t, client, target, headers, body1)
		if first.status == http.StatusOK {
			turn1Results = append(turn1Results, first)
		}
		if first.status != http.StatusOK {
			gateBlocked = fmt.Sprintf("首轮返回 %d: %s", first.status, continuationTrim(first.body, 800))
			t.Logf("rep %d 首轮失败，停止: %s", rep, gateBlocked)
			break
		}
		if first.id == "" {
			gateBlocked = "首轮没有返回 response.id"
			t.Logf("rep %d %s", rep, gateBlocked)
			break
		}
		storeDesc := "nil"
		if first.store != nil {
			storeDesc = fmt.Sprintf("%v", *first.store)
		}
		t.Logf("rep %d 首轮：ttft=%dms total=%dms bytes=%d id=%s store=%s events=%d usage input=%d cached=%d output=%d",
			rep, first.ttft.Milliseconds(), first.total.Milliseconds(), first.sentBytes, first.id, storeDesc,
			first.events, first.usage.input, first.usage.cached, first.usage.output)

		output := first.output
		if len(output) == 0 {
			output = []json.RawMessage{mustJSON(map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": first.text}},
			})}
		}

		question := fmt.Sprintf("第 7 张图（按图片顺序从 1 开始数）的主色是什么？只回答颜色名称，不要解释。")
		fullBody := continuationFullTurn2Body(modelName, session, input1, output, question, false)
		contBody := continuationContinuationTurn2Body(modelName, session, question, first.id)

		modes := []string{"full", "cont"}
		if rep%2 == 0 {
			modes = []string{"cont", "full"}
		}

		for _, mode := range modes {
			var (
				res      continuationResult
				expected = imgs[6].expected
			)
			switch mode {
			case "full":
				res = sendContinuationRequest(t, client, target, headers, fullBody)
				// 部分渠道在 store:false 下不接受 reasoning 历史项，去掉后重试一次。
				if res.status == http.StatusBadRequest && hasReasoningItem(output) {
					retry := continuationFullTurn2Body(modelName, session, input1, stripReasoningItems(output), question, false)
					t.Logf("rep %d full 首次 400（含 reasoning 项），去掉 reasoning 后重试: %s", rep, continuationTrim(res.body, 300))
					res = sendContinuationRequest(t, client, target, headers, retry)
				}
			case "cont":
				res = sendContinuationRequest(t, client, target, headers, contBody)
			}

			recallOK, note := continuationRecall(mode, res, expected)
			if mode == "full" {
				// 全量轮的答案作为续传轮的参照；生成图用已知颜色直接校验。
			}
			runs = append(runs, continuationRun{
				rep: rep, mode: mode, status: res.status, ttft: res.ttft, total: res.total,
				sentBytes: res.sentBytes, usage: res.usage, recallOK: recallOK,
				recallNote: note, answer: continuationTrim(res.text, 120),
			})
			t.Logf("rep %d %s：status=%d ttft=%dms total=%dms bytes=%d input=%d cached=%d recall=%v answer=%q",
				rep, mode, res.status, res.ttft.Milliseconds(), res.total.Milliseconds(),
				res.sentBytes, res.usage.input, res.usage.cached, recallOK, continuationTrim(res.text, 120))
			if res.status != http.StatusOK {
				t.Logf("rep %d %s 失败: %s", rep, mode, continuationTrim(res.body, 800))
			}
			if mode == "cont" && res.status != http.StatusOK && gateBlocked == "" {
				gateBlocked = fmt.Sprintf("续传轮返回 %d: %s", res.status, continuationTrim(res.body, 500))
			}
		}
	}

	if len(runs) > 0 {
		continuationReport(t, runs, turn1Results, gateBlocked)
	}

	// 错误形态探测：决定实现时的回退匹配和 session 头处理。
	if len(turn1Results) > 0 {
		continuationProbes(t, prov, client, target, modelName, continuationHeaders(prov, "continuation-probe-session", true))
	} else {
		t.Logf("没有拿到首轮 id，跳过错误形态探测")
	}
}

// ---------- 上游目标 ----------

func liveOpenCodeResponsesTarget(t *testing.T) (key, baseURL string, settings model.Settings) {
	t.Helper()

	baseURL = os.Getenv("CONTINUATION_LIVE_BASE_URL")
	path := os.Getenv("AI_PROXY_DB")
	if path == "" {
		path = filepath.Join("..", "..", "data", "ai_proxy.db")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("解析库路径失败: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(abs, `\`, "/")+"?mode=ro")
	if err != nil {
		t.Fatalf("打开只读数据库失败: %v", err)
	}
	defer db.Close()

	var raw string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'global'`).Scan(&raw); err == nil {
		if json.Unmarshal([]byte(raw), &settings) == nil {
			settings.ApplyDefaults()
		}
	}

	if key = os.Getenv("OPENCODE_API_KEY"); key != "" {
		if baseURL == "" {
			baseURL = "https://opencode.ai/inference/go/v1"
		}
		return key, baseURL, settings
	}

	var dbBase, keysJSON string
	err = db.QueryRow(`SELECT base_url, keys_json FROM providers
		WHERE display_name LIKE '%opencode%' AND enabled = 1
		ORDER BY active DESC, sort_order, id LIMIT 1`).Scan(&dbBase, &keysJSON)
	if err == nil {
		if baseURL == "" {
			baseURL = dbBase
		}
		var keys []model.APIKey
		if json.Unmarshal([]byte(keysJSON), &keys) == nil {
			for _, k := range keys {
				if k.Enabled && strings.TrimSpace(k.Key) != "" {
					key = k.Key
					break
				}
			}
		}
	}
	if key == "" {
		t.Skip("没有 OPENCODE_API_KEY，数据库里也没有可用的 opencode 供应商密钥")
	}
	if baseURL == "" {
		baseURL = "https://opencode.ai/inference/go/v1"
	}
	return key, baseURL, settings
}

func continuationHeaders(prov model.Provider, session string, withSession bool) http.Header {
	h := probeHeaders(prov)
	h.Set("Accept", "text/event-stream")
	h.Set("Content-Type", "application/json")
	if withSession {
		h.Set("x-opencode-session", session)
		h.Set("X-Session-Id", session)
	}
	return h
}

// ---------- 图片 ----------

func continuationImages(t *testing.T) ([]continuationImage, string) {
	t.Helper()

	if spec := strings.TrimSpace(os.Getenv("CONTINUATION_LIVE_IMAGES")); spec != "" {
		if imgs, ok := loadRealImages(t, spec, continuationImageCount); ok {
			return imgs, "CONTINUATION_LIVE_IMAGES"
		}
		t.Logf("CONTINUATION_LIVE_IMAGES 没有读到可用图片，回退到生成图")
	}
	return nil, "生成图"
}

func imagesForRep(imgs []continuationImage, rep int) []continuationImage {
	if len(imgs) < continuationImageCount {
		out := make([]continuationImage, continuationImageCount)
		for i := range out {
			out[i] = imgs[i%len(imgs)]
		}
		return out
	}
	out := make([]continuationImage, continuationImageCount)
	for i := 0; i < continuationImageCount; i++ {
		out[i] = imgs[(rep*continuationImageCount+i)%len(imgs)]
	}
	return out
}

func loadRealImages(t *testing.T, spec string, n int) ([]continuationImage, bool) {
	t.Helper()

	var paths []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		info, err := os.Stat(part)
		if err != nil {
			continue
		}
		if info.IsDir() {
			entries, err := os.ReadDir(part)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !isImageFile(e.Name()) {
					continue
				}
				paths = append(paths, filepath.Join(part, e.Name()))
			}
			continue
		}
		if isImageFile(part) {
			paths = append(paths, part)
		}
	}
	if len(paths) == 0 {
		return nil, false
	}
	sort.Strings(paths)

	out := make([]continuationImage, 0, n)
	for i := 0; i < n; i++ {
		path := paths[i%len(paths)]
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		out = append(out, continuationImage{url: dataURL(path, raw)})
	}
	if len(out) < n {
		return nil, false
	}
	return out, true
}

func isImageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return true
	}
	return false
}

func dataURL(path string, raw []byte) string {
	mime := "image/jpeg"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".webp":
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// generatedImagesForRep 每轮用不同噪声种子重新生成，颜色顺序保持一致，
// 这样同一轮的全量/续传共享同一批字节，而不同轮之间不会因图片字节相同而命中缓存。
func generatedImagesForRep(t *testing.T, rep int) []continuationImage {
	t.Helper()

	palette := []struct {
		name string
		r    uint8
		g    uint8
		b    uint8
	}{
		{"红色", 220, 30, 30},
		{"绿色", 30, 200, 60},
		{"蓝色", 30, 80, 220},
		{"黄色", 240, 220, 30},
		{"紫色", 130, 60, 200},
		{"橙色", 240, 140, 30},
		{"青色", 30, 200, 210},
		{"品红", 220, 40, 200},
		{"棕色", 120, 70, 30},
		{"粉色", 250, 150, 190},
		{"灰色", 128, 128, 128},
		{"黑色", 10, 10, 10},
	}
	out := make([]continuationImage, len(palette))
	for i, p := range palette {
		out[i] = continuationImage{
			url:      generatedNoisyPNG(t, p.r, p.g, p.b, rep*1000+40+i),
			expected: p.name,
		}
	}
	return out
}

// generatedNoisyPNG 生成一张中心纯色、边缘随机噪声的 PNG。
// 中心纯色保证视觉模型能稳定识别颜色，噪声边框把单张图片撑到几百 KB，
// 用来复现真实截图的上传体积。
func generatedNoisyPNG(t *testing.T, r, g, b uint8, seed int) string {
	t.Helper()

	const size = 512
	const border = 48
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	rng := rand.New(rand.NewSource(int64(seed)))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := y*img.Stride + x*4
			if x < border || y < border || x >= size-border || y >= size-border {
				img.Pix[i] = uint8(rng.Intn(256))
				img.Pix[i+1] = uint8(rng.Intn(256))
				img.Pix[i+2] = uint8(rng.Intn(256))
			} else {
				img.Pix[i] = r
				img.Pix[i+1] = g
				img.Pix[i+2] = b
			}
			img.Pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// ---------- 请求体 ----------

func continuationTurn1Body(modelName, session string, imgs []continuationImage) ([]byte, []any) {
	content := make([]any, 0, len(imgs)*2+1)
	for i, im := range imgs {
		content = append(content, map[string]any{
			"type": "input_text", "text": fmt.Sprintf("第 %d 张图：", i+1),
		})
		content = append(content, map[string]any{
			"type": "input_image", "image_url": im.url,
		})
	}
	content = append(content, map[string]any{
		"type": "input_text",
		"text": fmt.Sprintf("请记住这 %d 张图的顺序和颜色，只需回复 OK。", len(imgs)),
	})
	input := []any{
		map[string]any{
			"type": "message", "role": "system",
			"content": []any{map[string]any{"type": "input_text", "text": "你是一个精确的视觉助手，只按用户要求回答。"}},
		},
		map[string]any{"type": "message", "role": "user", "content": content},
	}
	body := mustJSON(map[string]any{
		"model":            modelName,
		"input":            input,
		"store":            true,
		"stream":           true,
		"prompt_cache_key": session,
	})
	return body, input
}

func continuationFullTurn2Body(modelName, session string, turn1Input []any, output []json.RawMessage, question string, store bool) []byte {
	input := append([]any{}, turn1Input...)
	for _, raw := range output {
		var item any
		if json.Unmarshal(raw, &item) == nil {
			input = append(input, item)
		}
	}
	input = append(input, map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": question}},
	})
	return mustJSON(map[string]any{
		"model":            modelName,
		"input":            input,
		"store":            store,
		"stream":           true,
		"prompt_cache_key": session,
	})
}

func continuationContinuationTurn2Body(modelName, session, question, previousID string) []byte {
	return mustJSON(map[string]any{
		"model":                modelName,
		"input":                []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": question}}}},
		"previous_response_id": previousID,
		"store":                true,
		"stream":               true,
		"prompt_cache_key":     session,
	})
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func hasReasoningItem(items []json.RawMessage) bool {
	for _, raw := range items {
		var it struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &it) == nil && it.Type == "reasoning" {
			return true
		}
	}
	return false
}

func stripReasoningItems(items []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		var it struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &it) == nil && it.Type == "reasoning" {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// ---------- 发送与解析 ----------

func sendContinuationRequest(t *testing.T, client *http.Client, target string, headers http.Header, body []byte) continuationResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	req, err := buildRequest(ctx, "POST", target, body, headers)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求上游失败: %v", err)
	}
	defer resp.Body.Close()

	out := continuationResult{status: resp.StatusCode, sentBytes: len(body)}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		out.body = strings.TrimSpace(string(raw))
		out.total = time.Since(start)
		return out
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		out.body = string(raw)
		out.total = time.Since(start)
		parseContinuationFinal(raw, &out)
		out.text = continuationOutputText(out.output)
		return out
	}

	an := newAnalyzer(model.FormatResponses, nil, contentType)
	framer := newSSEFramer(func(raw []byte, event, data string) {
		out.events++
		an.OnEvent(event, data)
		if an.HasContent() && out.ttft == 0 {
			out.ttft = time.Since(start)
		}
		parseContinuationEvent(data, &out, start)
	})

	buf := make([]byte, copyBufferSize)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			framer.Feed(buf[:n])
		}
		if readErr != nil {
			break
		}
	}
	framer.Flush()
	out.total = time.Since(start)
	if out.errMsg == "" {
		out.errMsg = an.ErrMsg()
	}
	if out.text == "" {
		out.text = continuationOutputText(out.output)
	}
	return out
}

func parseContinuationEvent(data string, out *continuationResult, start time.Time) {
	if data == "" || data == "[DONE]" {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Text     string `json:"text"`
		Status   string `json:"status"`
		Error    any    `json:"error"`
		Response struct {
			ID     string            `json:"id"`
			Store  *bool             `json:"store"`
			Status string            `json:"status"`
			Output []json.RawMessage `json:"output"`
			Usage  json.RawMessage   `json:"usage"`
			Error  json.RawMessage   `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil {
		return
	}

	if ev.Response.ID != "" {
		out.id = ev.Response.ID
	}
	if ev.Response.Store != nil {
		out.store = ev.Response.Store
	}
	if ev.Response.Status != "" {
		out.respState = ev.Response.Status
	}
	if len(ev.Response.Output) > 0 {
		out.output = ev.Response.Output
	}
	if len(ev.Response.Usage) > 0 {
		out.usage = parseContinuationUsage(ev.Response.Usage)
	}
	if len(ev.Response.Error) > 0 && string(ev.Response.Error) != "null" {
		out.errMsg = continuationTrim(string(ev.Response.Error), 300)
	}

	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta != "" {
			out.text += ev.Delta
			if out.firstText == 0 {
				out.firstText = time.Since(start)
			}
		}
	case "response.failed", "response.incomplete":
		if out.errMsg == "" {
			out.errMsg = ev.Type
		}
	}
}

func parseContinuationFinal(raw []byte, out *continuationResult) {
	var ev struct {
		ID     string            `json:"id"`
		Store  *bool             `json:"store"`
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
		Usage  json.RawMessage   `json:"usage"`
		Error  json.RawMessage   `json:"error"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	out.id = ev.ID
	out.store = ev.Store
	out.respState = ev.Status
	out.output = ev.Output
	if len(ev.Usage) > 0 {
		out.usage = parseContinuationUsage(ev.Usage)
	}
	if len(ev.Error) > 0 && string(ev.Error) != "null" {
		out.errMsg = continuationTrim(string(ev.Error), 300)
	}
}

func parseContinuationUsage(raw json.RawMessage) continuationUsage {
	var u struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		InputDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return continuationUsage{}
	}
	input, output := u.InputTokens, u.OutputTokens
	if input == 0 {
		input = u.PromptTokens
	}
	if output == 0 {
		output = u.CompletionTokens
	}
	return continuationUsage{
		input: input, output: output, cached: u.InputDetails.CachedTokens,
		reasoning: u.OutputDetails.ReasoningTokens,
		found:     input > 0 || output > 0,
	}
}

func continuationOutputText(items []json.RawMessage) string {
	var sb strings.Builder
	for _, raw := range items {
		var item struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" || c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
	}
	return sb.String()
}

// ---------- 校验与报告 ----------

func continuationRecall(mode string, res continuationResult, expected string) (bool, string) {
	if res.status != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d", res.status)
	}
	answer := strings.TrimSpace(res.text)
	if answer == "" {
		return false, "没有文字输出"
	}
	if expected != "" {
		if continuationMatchesColor(answer, expected) {
			return true, "命中预期颜色 " + expected
		}
		return false, "预期 " + expected + "，实际 " + continuationTrim(answer, 80)
	}
	return true, "真实图片，仅检查有输出"
}

func continuationMatchesColor(answer, expected string) bool {
	synonyms := map[string][]string{
		"红色": {"红", "red"},
		"绿色": {"绿", "green"},
		"蓝色": {"蓝", "blue"},
		"黄色": {"黄", "yellow"},
		"紫色": {"紫", "purple", "violet"},
		"橙色": {"橙", "orange"},
		"青色": {"青", "cyan", "teal"},
		"品红": {"品红", "洋红", "magenta", "pinkish purple"},
		"棕色": {"棕", "褐", "brown"},
		"粉色": {"粉", "pink"},
		"灰色": {"灰", "gray", "grey"},
		"黑色": {"黑", "black"},
	}
	lower := strings.ToLower(answer)
	for _, kw := range synonyms[expected] {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

func continuationReport(t *testing.T, runs []continuationRun, turn1 []continuationResult, blocked string) {
	t.Helper()

	var full, cont []time.Duration
	fullOK, contOK := true, true
	for _, r := range runs {
		switch r.mode {
		case "full":
			full = append(full, r.ttft)
			fullOK = fullOK && r.recallOK
		case "cont":
			cont = append(cont, r.ttft)
			contOK = contOK && r.recallOK
		}
	}

	t.Logf("========== 结果汇总 ==========")
	t.Logf("rep  mode  status  ttft_ms  total_ms  bytes  input  cached  recall  answer")
	for _, r := range runs {
		t.Logf("%-4d %-5s %-6d %-8d %-9d %-6d %-6d %-7d %-6v %q",
			r.rep, r.mode, r.status, r.ttft.Milliseconds(), r.total.Milliseconds(),
			r.sentBytes, r.usage.input, r.usage.cached, r.recallOK, r.answer)
	}

	fullMed, contMed := medianDuration(full), medianDuration(cont)
	delta := fullMed - contMed
	pct := 0.0
	if fullMed > 0 {
		pct = float64(delta) / float64(fullMed) * 100
	}

	status := "PASS"
	reasons := []string{}
	if blocked != "" {
		status = "BLOCKED"
		reasons = append(reasons, blocked)
	}
	if fullMed == 0 || contMed == 0 {
		status = "INCONCLUSIVE"
		reasons = append(reasons, "样本不足")
	}
	if pct < 20 || delta < time.Second {
		status = "FAIL"
		reasons = append(reasons, fmt.Sprintf("TTFT 收益不足（%.1f%%，%dms）", pct, delta.Milliseconds()))
	}
	if !fullOK || !contOK {
		status = "FAIL"
		reasons = append(reasons, "回忆校验未全部通过")
	}

	t.Logf("中位 TTFT：全量=%dms 续传=%dms 下降=%dms (%.1f%%)",
		fullMed.Milliseconds(), contMed.Milliseconds(), delta.Milliseconds(), pct)
	t.Logf("回忆：全量=%v 续传=%v", fullOK, contOK)
	if len(turn1) > 0 {
		var t1 []time.Duration
		for _, r := range turn1 {
			t1 = append(t1, r.ttft)
		}
		t.Logf("首轮 TTFT 中位=%dms（首轮无法优化，仍为全量上传）", medianDuration(t1).Milliseconds())
	}
	t.Logf("GATE: %s %s", status, strings.Join(reasons, "；"))
}

func medianDuration(v []time.Duration) time.Duration {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]time.Duration{}, v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func continuationTrim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- 探测 ----------

func continuationProbes(t *testing.T, prov model.Provider, client *http.Client, target, modelName string, headers http.Header) {
	t.Helper()

	invalidBody := mustJSON(map[string]any{
		"model":                modelName,
		"input":                "hi",
		"previous_response_id": "resp_continuation_probe_invalid",
		"store":                true,
		"stream":               false,
	})
	res := sendContinuationRequest(t, client, target, headers, invalidBody)
	t.Logf("PROBE invalid-id: status=%d body=%s", res.status, continuationTrim(res.body, 500))

	chatTarget, err := buildUpstreamURL(prov, "/v1/chat/completions", "")
	if err == nil {
		chatBody := mustJSON(map[string]any{
			"model":                modelName,
			"messages":             []any{map[string]any{"role": "user", "content": "hi"}},
			"previous_response_id": "resp_continuation_probe_invalid",
			"stream":               false,
		})
		chat := sendContinuationRequest(t, client, chatTarget, headers, chatBody)
		t.Logf("PROBE chat+previous_response_id: status=%d body=%s", chat.status, continuationTrim(chat.body, 300))
	}

	noSession := continuationHeaders(prov, "", false)
	tinyBody := mustJSON(map[string]any{
		"model": modelName, "input": "hi", "store": false, "stream": false,
	})
	tiny := sendContinuationRequest(t, client, target, noSession, tinyBody)
	t.Logf("PROBE 无 session 头: status=%d body=%s", tiny.status, continuationTrim(tiny.body, 300))
}
