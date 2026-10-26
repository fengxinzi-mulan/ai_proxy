package proxy

// 模拟 ZCode 并行 subagent 的续传验证：同一 session 下多条分支各自维护
// 独立历史并交替请求，检查每条分支是否都能稳定命中自己的链。
// 回放上一轮 output 时按真实客户端形态丢掉 reasoning 项。
//
// 这是对生产日志里 continued / prefix-mismatch 交替抖动的回归验证：
// 单槽位链会让分支互相覆盖，而严格全前缀匹配在客户端丢 reasoning 时必然失配，
// 修复后（多分支 + 丢项容忍）从第 2 轮起不该再出现 prefix-mismatch。
//
// commandcode 的存储是间歇性的（store 会回 false），所以先热身到存储可用再跑。
//
//	CONTINUATION_LIVE=1 CONTINUATION_CC_LIVE=1 CONTINUATION_BRANCH_LIVE=1 \
//	  go test ./internal/proxy -run TestLiveCommandCodeParallelBranches -v

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

func TestLiveCommandCodeParallelBranches(t *testing.T) {
	if os.Getenv("CONTINUATION_LIVE") != "1" || os.Getenv("CONTINUATION_CC_LIVE") != "1" ||
		os.Getenv("CONTINUATION_BRANCH_LIVE") != "1" {
		t.Skip("未设置 CONTINUATION_LIVE=1 / CONTINUATION_CC_LIVE=1 / CONTINUATION_BRANCH_LIVE=1，跳过多分支实验")
	}

	key, baseURL, settings := liveCommandCodeTarget(t)
	modelName := os.Getenv("COMMAND_CODE_MODEL")
	if modelName == "" {
		modelName = "deepseek/deepseek-v4.1-flash"
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveSettings(settings); err != nil {
		t.Fatalf("写入临时设置失败: %v", err)
	}

	p := model.Provider{
		Name:                               "cc-branch",
		DisplayName:                        "cc-branch",
		Enabled:                            true,
		BaseURL:                            baseURL,
		APIFormat:                          model.FormatResponses,
		ContinuationMode:                   model.ContinuationOn,
		ContinuationModels:                 []string{modelName},
		ContinuationCooldownSeconds:        0,
		ContinuationInvalidCooldownSeconds: 0,
		Keys:                               []model.APIKey{{ID: "live", Key: key, Enabled: true}},
	}
	if _, err := st.CreateProvider(p); err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	srv := NewServer(st, hub.New(), quietLogger())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := &http.Client{Timeout: 300 * time.Second}
	path := "/p/cc-branch/v1/responses"

	post := func(session string, input []any) *continuationResult {
		body := mustJSON(map[string]any{
			"model": modelName, "input": input, "store": false, "stream": true,
		})
		res := sendContinuationRequest(t, client, ts.URL+path,
			continuationProxyHeaders(settings.AccessKey, session), body)
		if res.status != http.StatusOK {
			t.Logf("上游返回 %d: %s", res.status, continuationTrim(res.body, 200))
		}
		return &res
	}

	// 上游存储间歇不可用：用独立会话热身，直到观察到一次真实续传成功。
	storageReady := false
	for attempt := 1; attempt <= 10 && !storageReady; attempt++ {
		probeSession := fmt.Sprintf("cc-warm-%d-%d", time.Now().UnixNano(), attempt)
		input := []any{contUser("只回复 OK。")}
		res := post(probeSession, input)
		if res.status != http.StatusOK {
			continue
		}
		input = append(input, contAssistant(res.text))
		input = append(input, contUser("再说一次。"))
		post(probeSession, input)
		log := lastLog(t, st)
		t.Logf("热身 %d：上一轮 mode=%s/%s", attempt, log.ContinuationMode, log.ContinuationReason)
		if log.ContinuationMode == "continued" {
			storageReady = true
		}
	}
	if !storageReady {
		t.Skip("commandcode 连续 10 次热身都未成功存储，无法验证分支隔离")
	}

	// ZCode 的并行分支共享 system 前缀与首个用户消息，之后各自分叉。
	shared := []any{
		contUser("你是 ZCode 的编程助手，回答简洁，只按用户要求回复。"),
		contUser("请准备分析仓库。"),
	}

	type branch struct {
		name    string
		history []any
		pending []any
		modes   []string
	}
	branches := []*branch{
		{name: "A", history: append([]any{}, shared...), pending: []any{contUser("只回复 A1。")}},
		{name: "B", history: append([]any{}, shared...), pending: []any{contUser("只回复 B1。")}},
		{name: "C", history: append([]any{}, shared...), pending: []any{contUser("只回复 C1。")}},
	}

	session := fmt.Sprintf("cc-branch-%d", time.Now().UnixNano())
	for round := 1; round <= 3; round++ {
		for _, b := range branches {
			input := append(append([]any{}, b.history...), b.pending...)
			res := post(session, input)
			if res.status != http.StatusOK {
				t.Fatalf("分支 %s 第 %d 轮返回 %d: %s", b.name, round, res.status, continuationTrim(res.body, 300))
			}
			log := lastLog(t, st)
			b.modes = append(b.modes, log.ContinuationMode+"/"+log.ContinuationReason)
			t.Logf("分支 %s 第 %d 轮：mode=%s/%s ttft=%dms saved=%d text=%q",
				b.name, round, log.ContinuationMode, log.ContinuationReason,
				log.TTFTMs, log.SavedBytes, continuationTrim(res.text, 40))

			b.history = append(b.history, b.pending...)
			out := res.output
			if len(out) == 0 {
				out = []json.RawMessage{mustJSON(contAssistant(res.text))}
			}
			// 按真实客户端形态回放（ZCode 不带 reasoning 项）。
			b.history = append(b.history, replayOutputItems(out)...)
			b.pending = []any{contUser(fmt.Sprintf("只回复 %s%d。", b.name, round+1))}
		}
	}

	// 核心断言：按客户端形态回放（丢 reasoning）后，从第 2 轮起不能再出现
	// prefix-mismatch。上游 store 是间歇性的，偶发 seed/no-chain 允许（那一轮
	// 没保存），但整轮验证至少要出现一次真实命中。
	hits := 0
	for _, b := range branches {
		for i, mode := range b.modes {
			if i == 0 {
				continue
			}
			switch mode {
			case "continued/prefix-match":
				hits++
			case "seed/no-chain":
				t.Logf("分支 %s 第 %d 轮：上游未存储，本轮无法验证", b.name, i+1)
			default:
				t.Errorf("分支 %s 第 %d 轮丢 reasoning 回放后仍失配（%s），丢项容忍失效", b.name, i+1, mode)
			}
		}
	}
	if hits == 0 && !t.Failed() {
		t.Skip("上游存储整轮不可用，没有验证到真实命中")
	}
	t.Logf("丢 reasoning 回放形态下共 %d 次真实命中", hits)
}
