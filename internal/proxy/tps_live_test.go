package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"

	_ "modernc.org/sqlite"
)

// 上游投递画像的真实请求验证，默认跳过，只在显式给了密钥时运行。
//
// 背景见 maxPlausibleTPS：opencode zen 对「reasoning_tokens=0 的小 tool_calls 响应」
// 会把整条流攒齐后一次性下发，代理量到的生成窗口（总耗时 − 首 token）塌成几毫秒，
// 于是 58 个 token 能算出 19000 t/s。这个用例打真实上游，用与转发同一套解析器
// （sseFramer + analyzer）量一遍投递过程，打印每次请求的画像：
// 事件数、首内容事件前的静默、内容事件的时间跨度、usage、按 computeTPS 算出的速度。
//
// 断言只有一条不变式：速度要么落在可信范围内，要么被判为不可测（0）——
// 任何情况下都不该出现超过上限的假值。上游是否整流下发不受我们控制，
// 所以不能断言「必须撞到整流」，但撞到时上面的画像会把它记下来。
//
//	OPENCODE_API_KEY=oc_sk_xxx go test ./internal/proxy -run LiveTPS -v
//
// 未设密钥时只读打开 data/ai_proxy.db 取 opencode 供应商的密钥与 BaseURL。
// 想要更长的上下文、更容易触发整流时，可用 TPS_LIVE_PAD 指定填充轮数（默认 0）。
func TestLiveUpstreamStreamingDeliveryProfile(t *testing.T) {
	key, baseURL, settings := liveOpenCodeTarget(t)

	prov := model.Provider{
		Name:      "live-oc",
		Enabled:   true,
		BaseURL:   baseURL,
		APIFormat: model.FormatOpenAI,
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
	target, err := buildUpstreamURL(prov, "/v1/chat/completions", "")
	if err != nil {
		t.Fatalf("拼接上游地址失败: %v", err)
	}
	name := os.Getenv("TPS_LIVE_MODEL")
	if name == "" {
		name = "deepseek-v4.1-flash"
	}
	t.Logf("上游=%s 代理=%v(%s) 模型=%s", target, netcfg.ProxyInUse, netcfg.ProxyDesc, name)

	body := liveToolCallBody(t, name)
	headers := liveProbeHeaders(prov)

	// 连打几次：整流下发是间歇性的，多跑几次更容易观测到。每次都必须满足不变式。
	const runs = 3
	for run := 1; run <= runs; run++ {
		liveMeasureOnce(t, client, headers, target, body, run)
	}
}

// liveProbeHeaders 是探测请求的头集合。
//
// 比 probeHeaders 多两样：SSE 的 Accept，以及 opencode zen 要求的会话头 ——
// 2026-10-13 实测缺 x-opencode-session 会被直接 400（MissingSessionID），
// 而真实客户端的每一次请求都带着它（代理原样转发客户端头）。
func liveProbeHeaders(prov model.Provider) http.Header {
	h := probeHeaders(prov)
	h.Set("Accept", "text/event-stream")
	h.Set("Content-Type", "application/json")
	session := os.Getenv("OPENCODE_SESSION")
	if session == "" {
		session = fmt.Sprintf("tps-live-%d", time.Now().UnixNano())
	}
	h.Set("x-opencode-session", session)
	return h
}

// liveToolCallBody 拼一条「强制调用工具」的流式请求，形状与 opencode 客户端一致。
// 上游是否整流下发跟请求内容无关，这里只是制造最可能出现整流的那种响应
// （无 reasoning、纯 tool_calls 的小输出）。
func liveToolCallBody(t *testing.T, modelName string) []byte {
	t.Helper()

	messages := []map[string]any{
		{"role": "system", "content": "你必须调用工具 read_file 读取指定文件，不要输出任何解释文字。只要调用工具，不要说别的。"},
	}
	// 可选的填充轮：把上下文撑长，逼近真实的长会话。
	if pad := os.Getenv("TPS_LIVE_PAD"); pad != "" {
		rounds, err := strconv.Atoi(pad)
		if err != nil || rounds < 0 {
			t.Fatalf("TPS_LIVE_PAD 应为非负整数（填充轮数），实际 %q", pad)
		}
		for i := 0; i < rounds; i++ {
			messages = append(messages,
				map[string]any{"role": "user", "content": strings.Repeat("背景资料。", 200)},
				map[string]any{"role": "assistant", "content": "收到。"},
			)
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": "读取 /tmp/tps-live-probe.txt"})

	payload := map[string]any{
		"model":          modelName,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       messages,
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "读取指定文件的内容",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []string{"path"},
				},
			},
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	return raw
}

// liveMeasureOnce 发一次真实请求，按转发同样的方式逐事件解析并统计投递画像。
func liveMeasureOnce(t *testing.T, client *http.Client, headers http.Header, target string, body []byte, run int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		t.Fatalf("上游返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var (
		events         int
		bytesRead      int
		contentEvents  int
		firstContentAt time.Duration
		lastContentAt  time.Duration
	)
	an := newAnalyzer(model.FormatOpenAI, nil, resp.Header.Get("Content-Type"))
	framer := newSSEFramer(func(raw []byte, event, data string) {
		events++
		bytesRead += len(raw)
		an.OnEvent(event, data)
		if an.HasContent() {
			contentEvents++
			if firstContentAt == 0 {
				firstContentAt = time.Since(start)
			}
			lastContentAt = time.Since(start)
		}
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
	end := time.Since(start)

	usage := an.Result()
	if !an.HasContent() {
		t.Errorf("第 %d 次：上游没有产出内容（events=%d bytes=%d），探测无效", run, events, bytesRead)
		return
	}
	tps := computeTPS(usage.CompletionTokens, firstContentAt.Milliseconds(), end.Milliseconds())
	spread := (lastContentAt - firstContentAt).Milliseconds()

	shape := "逐事件下发"
	if contentEvents > 1 && spread < 50 {
		shape = "整流下发"
	}
	t.Logf("第 %d 次（%s）：%d 个事件 / %d 字节；首内容事件 %dms，内容事件 %d 个、跨度 %dms；"+
		"usage output=%d（其中 reasoning=%d，cached=%d，估算=%v）；生成窗口 %dms；computeTPS=%.1f t/s",
		run, shape, events, bytesRead, firstContentAt.Milliseconds(), contentEvents, spread,
		usage.CompletionTokens, usage.ReasoningTokens, usage.CachedTokens, usage.Estimated,
		(end - firstContentAt).Milliseconds(), tps)

	if tps > maxPlausibleTPS {
		t.Errorf("第 %d 次算出超过可信上限的速度 %.1f t/s（整流下发时本应判为不可测）", run, tps)
	}
}

// liveOpenCodeTarget 取 opencode zen 的密钥与 BaseURL：
// 优先环境变量 OPENCODE_API_KEY，否则只读打开 data/ai_proxy.db 读该供应商的配置。
// 密钥只留在内存里，不写任何文件、也不打进日志。
func liveOpenCodeTarget(t *testing.T) (key, baseURL string, settings model.Settings) {
	t.Helper()

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
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'global'`).Scan(&raw); err != nil {
		t.Fatalf("读取全局设置失败: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("解析全局设置失败: %v", err)
	}
	settings.ApplyDefaults()

	var keysJSON string
	err = db.QueryRow(
		`SELECT base_url, keys_json FROM providers
		 WHERE display_name LIKE '%opencode%' AND enabled = 1
		 ORDER BY active DESC, sort_order, id LIMIT 1`,
	).Scan(&baseURL, &keysJSON)
	if err != nil && os.Getenv("OPENCODE_API_KEY") == "" {
		t.Fatalf("库中没有可用的 opencode 供应商，请设置 OPENCODE_API_KEY: %v", err)
	}
	if err != nil {
		baseURL = "https://opencode.ai/zen/go/v1"
	}

	if key = os.Getenv("OPENCODE_API_KEY"); key != "" {
		return key, baseURL, settings
	}

	var keys []model.APIKey
	if err := json.Unmarshal([]byte(keysJSON), &keys); err != nil {
		t.Fatalf("解析密钥失败: %v", err)
	}
	for _, k := range keys {
		if k.Enabled && strings.TrimSpace(k.Key) != "" {
			return k.Key, baseURL, settings
		}
	}
	t.Fatal("opencode 供应商没有可用密钥，请设置 OPENCODE_API_KEY")
	return "", baseURL, settings
}
