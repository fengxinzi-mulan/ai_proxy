package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"

	_ "modernc.org/sqlite"
)

// responses 格式的真实请求验证。默认跳过，只在显式给了密钥时运行。
//
// 用例一：把 ZCode 在 responses 格式下真实发出的载荷（assistant 历史项缺 type）
// 过一遍 rewriteRequestBody 再发上游，验证补 type 之后不再被拒。
// 用例二：把真实照片按同一形状（function_call_output 里塞 input_image）发出去，
// 验证图片确实到达模型并且能被描述出来。
//
// 密钥与图片都只从环境变量读，不落文件：
//
//	COMMAND_CODE_API_KEY=user_xxx \
//	RESPONSES_LIVE_IMAGE="E:\Pictures\Saved Pictures\wallhaven-k7jd2q.jpg" \
//	RESPONSES_LIVE_IMAGE_EXPECT="金,绿,项链,女" \
//	  go test ./internal/proxy -run LiveResponses -v
//
// 未设 RESPONSES_LIVE_IMAGE 时退化为一张 1×1 红图，只验证链路通、不校验内容。
// 未设 COMMAND_CODE_API_KEY 时从 data/ai_proxy.db 只读取出 active 供应商的密钥。
func liveResponsesEnv(t *testing.T) (key string, settings model.Settings) {
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

	if key = os.Getenv("COMMAND_CODE_API_KEY"); key != "" {
		return key, settings
	}

	var keysJSON string
	if err := db.QueryRow(
		`SELECT keys_json FROM providers WHERE active = 1 AND enabled = 1 ORDER BY sort_order, id LIMIT 1`,
	).Scan(&keysJSON); err != nil {
		t.Fatalf("读取 active 供应商失败: %v", err)
	}
	var keys []model.APIKey
	if err := json.Unmarshal([]byte(keysJSON), &keys); err != nil {
		t.Fatalf("解析密钥失败: %v", err)
	}
	for _, k := range keys {
		if k.Enabled && strings.TrimSpace(k.Key) != "" {
			return k.Key, settings
		}
	}
	t.Fatal("active 供应商没有可用密钥，请设置 COMMAND_CODE_API_KEY")
	return "", settings
}

// liveResponsesClient 是一次真实上游调用的最小载体。
type liveResponsesClient struct {
	t      *testing.T
	prov   model.Provider
	client *http.Client
	target string
	model  string
}

func newLiveResponsesClient(t *testing.T) *liveResponsesClient {
	t.Helper()
	key, settings := liveResponsesEnv(t)

	prov := model.Provider{
		Name:      "live",
		Enabled:   true,
		BaseURL:   "https://api.commandcode.ai/provider/v1",
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

	name := os.Getenv("RESPONSES_LIVE_MODEL")
	if name == "" {
		name = "deepseek/deepseek-v4.1-flash"
	}
	t.Logf("上游=%s 代理=%v(%s) 模型=%s", target, netcfg.ProxyInUse, netcfg.ProxyDesc, name)
	return &liveResponsesClient{t: t, prov: prov, client: client, target: target, model: name}
}

// post 把已经过改写流程的 body 真发给上游。
func (c *liveResponsesClient) post(body []byte) (int, []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	req, err := buildRequest(ctx, "POST", c.target, body, probeHeaders(c.prov))
	if err != nil {
		c.t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("请求上游失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	return resp.StatusCode, raw
}

// buildClientShapedBody 拼出 ZCode 在 responses 格式下真实发出的形状：
// 用户提问 + assistant 历史项（缺 type）+ function_call + function_call_output。
// toolOutput 为空时用普通字符串结果，否则按图片数组给。
func buildClientShapedBody(modelName, question string, toolOutput any) []byte {
	if toolOutput == nil {
		toolOutput = "ok"
	}
	body, err := json.Marshal(map[string]any{
		"model": modelName,
		"input": []any{
			map[string]any{"role": "system", "content": "You are ZCode."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": question}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "我先读这张图片。"}}},
			map[string]any{"type": "function_call", "call_id": "call_read_1",
				"name": "Read", "arguments": `{"file_path":"x.jpg"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_read_1",
				"output": toolOutput},
		},
		"stream": false,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// dataURLFromFile 把图片读成 data URL；读不到时返回一张 1×1 红图。
func dataURLFromFile(t *testing.T) (string, bool) {
	t.Helper()
	path := os.Getenv("RESPONSES_LIVE_IMAGE")
	if path == "" {
		return tinyPNGDataURL(t), false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取图片失败: %v", err)
	}
	mime := "image/jpeg"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".webp":
		mime = "image/webp"
	}
	t.Logf("图片 %s（%d 字节）", path, len(raw))
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), true
}

func tinyPNGDataURL(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 40, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// responsesOutputText 从非流式 responses 响应里取出模型的文字输出。
func responsesOutputText(raw []byte) string {
	var obj struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, item := range obj.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				sb.WriteString(part.Text)
			}
		}
	}
	return sb.String()
}

func summarize(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

// 用例一：补 type 之后，带 assistant 历史的请求不再被上游拒绝。
func TestLiveResponsesInputFix(t *testing.T) {
	if os.Getenv("COMMAND_CODE_API_KEY") == "" && os.Getenv("RESPONSES_LIVE") == "" {
		t.Skip("未设置 COMMAND_CODE_API_KEY（或 RESPONSES_LIVE），跳过实查")
	}
	lc := newLiveResponsesClient(t)

	clientBody := buildClientShapedBody(lc.model, "你好，先跟我打个招呼。", nil)
	out, mods := rewriteRequestBody(lc.prov, model.DefaultSettings(), clientBody, modelNameFromBody(clientBody))
	if len(mods) != 1 || mods[0] != "responses_input_type" {
		t.Fatalf("改写清单应为 [responses_input_type]，实际 %v", mods)
	}
	var sent struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatalf("改写结果解析失败: %v", err)
	}
	for i := range 3 {
		if sent.Input[i]["type"] != "message" {
			t.Errorf("input[%d] 应补上 type=message，实际 %v", i, sent.Input[i]["type"])
		}
	}
	if sent.Input[3]["type"] != "function_call" || sent.Input[4]["type"] != "function_call_output" {
		t.Error("已有 type 的工具项不该被改写")
	}

	status, raw := lc.post(out)
	t.Logf("状态=%d 响应=%s", status, summarize(raw))
	if status != 200 {
		t.Fatalf("补 type 后仍被上游拒绝: %d %s", status, summarize(raw))
	}
	if text := responsesOutputText(raw); strings.TrimSpace(text) == "" {
		t.Errorf("上游返回 200 但没读到文字输出: %s", summarize(raw))
	}
}

// 用例二：图片按 function_call_output 的 input_image 形状发出，模型能描述它。
func TestLiveResponsesVision(t *testing.T) {
	if os.Getenv("COMMAND_CODE_API_KEY") == "" && os.Getenv("RESPONSES_LIVE") == "" {
		t.Skip("未设置 COMMAND_CODE_API_KEY（或 RESPONSES_LIVE），跳过实查")
	}
	lc := newLiveResponsesClient(t)

	imageURL, real := dataURLFromFile(t)
	if !real {
		t.Log("未设置 RESPONSES_LIVE_IMAGE，用 1×1 红图只验证链路")
	}
	clientBody := buildClientShapedBody(lc.model,
		"使用纯视觉能力看这张图片内容能看到吗？请描述你看到的画面。",
		[]any{map[string]any{"type": "input_image", "image_url": imageURL}})

	out, mods := rewriteRequestBody(lc.prov, model.DefaultSettings(), clientBody, modelNameFromBody(clientBody))
	if len(mods) != 1 || mods[0] != "responses_input_type" {
		t.Fatalf("改写清单应为 [responses_input_type]，实际 %v", mods)
	}

	status, raw := lc.post(out)
	text := responsesOutputText(raw)
	t.Logf("状态=%d 输出=%s", status, text)
	if status != 200 {
		t.Fatalf("图片请求被上游拒绝: %d %s", status, summarize(raw))
	}
	if !real {
		if strings.TrimSpace(text) == "" {
			t.Errorf("上游返回 200 但没读到文字输出: %s", summarize(raw))
		}
		return
	}
	if strings.TrimSpace(text) == "" {
		t.Fatalf("读了真实图片却没有任何文字输出: %s", summarize(raw))
	}

	// 关键词命中过半即认为确实看到了画面（避免个别词表述不同导致误报）。
	expect := os.Getenv("RESPONSES_LIVE_IMAGE_EXPECT")
	if expect == "" {
		return
	}
	var words []string
	for _, w := range strings.Split(expect, ",") {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, w)
		}
	}
	hits := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			hits++
		}
	}
	if hits*2 < len(words) {
		t.Errorf("模型输出与图片内容不符：%d/%d 个关键词命中（%v）\n输出: %s",
			hits, len(words), words, text)
	}
}
