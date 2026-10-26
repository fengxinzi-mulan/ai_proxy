package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// ---------- 测试辅助 ----------

func newContinuationProvider(t *testing.T, st *store.Store, baseURL, mode string) model.Provider {
	t.Helper()
	p := addProvider(t, st, baseURL, model.FormatResponses)
	p.ContinuationMode = mode
	p.ContinuationCooldownSeconds = model.DefaultContinuationCooldownSeconds
	p.ContinuationInvalidCooldownSeconds = model.DefaultContinuationInvalidCooldownSeconds
	updated, err := st.UpdateProvider(p)
	if err != nil {
		t.Fatalf("更新续传供应商失败: %v", err)
	}
	return updated
}

func contUser(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

func contAssistant(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text}},
	}
}

func contBody(stream bool, input ...any) string {
	raw, err := json.Marshal(map[string]any{
		"model":  "test-model",
		"input":  input,
		"store":  false,
		"stream": stream,
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func contJSONString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func contSSE(id, text string) string {
	return fmt.Sprintf("event: response.output_text.delta\ndata: %s\n\n"+
		"event: response.completed\ndata: %s\n\n",
		contJSONString(map[string]any{"type": "response.output_text.delta", "delta": text}),
		contJSONString(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": id, "status": "completed",
				"output": []any{contAssistant(text)},
			},
		}),
	)
}

func contSSEStore(id, text string, store bool) string {
	return fmt.Sprintf("event: response.output_text.delta\ndata: %s\n\n"+
		"event: response.completed\ndata: %s\n\n",
		contJSONString(map[string]any{"type": "response.output_text.delta", "delta": text}),
		contJSONString(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": id, "status": "completed", "store": store,
				"output": []any{contAssistant(text)},
			},
		}),
	)
}

func contToolSSE(id string, fc map[string]any) string {
	return fmt.Sprintf("event: response.completed\ndata: %s\n\n",
		contJSONString(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": id, "status": "completed",
				"output": []any{fc},
			},
		}),
	)
}

// contItemsSSE 返回带多个 output 项的 completed 事件，用于构造 reasoning /
// 多工具调用的真实回执。
func contItemsSSE(id string, items ...any) string {
	return fmt.Sprintf("event: response.completed\ndata: %s\n\n",
		contJSONString(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": id, "status": "completed", "store": true,
				"output": items,
			},
		}),
	)
}

// contReasoning 构造上游回执里的 reasoning 项。带 id/status 是为了验证归一化：
// 客户端回放时不带这两个字段，归一化后必须视为同一条。
func contReasoning(text string) map[string]any {
	return map[string]any{
		"id": "rs_" + text, "type": "reasoning", "status": "completed",
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
}

// contCall 构造 function_call 项（上游回执形态，带 id/status）。
func contCall(callID, command string) map[string]any {
	return map[string]any{
		"type": "function_call", "call_id": callID, "name": "Bash",
		"arguments": contJSONString(map[string]any{"command": command}),
		"id":        "fc_" + callID, "status": "completed",
	}
}

// contReplayCall 构造客户端回放的 function_call 项：不带 id/status。
func contReplayCall(callID, command string) map[string]any {
	return map[string]any{
		"type": "function_call", "call_id": callID, "name": "Bash",
		"arguments": contJSONString(map[string]any{"command": command}),
	}
}

func contCallOutput(callID, output string) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}
}

func contFailedSSE(id, msg string) string {
	return fmt.Sprintf("event: response.failed\ndata: %s\n\n",
		contJSONString(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "status": "failed",
				"error": map[string]any{"message": msg},
			},
		}),
	)
}

func postContinuation(t *testing.T, srv *Server, body, session string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Session-Id", session)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func parseContBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("解析上游请求体失败: %v body=%s", err, raw)
	}
	return body
}

func contInput(t *testing.T, body map[string]any) []any {
	t.Helper()
	input, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input 不是数组: %#v", body["input"])
	}
	return input
}

// ---------- 种子与续传 ----------

func TestContinuationSeedAndContinue(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			io.WriteString(w, contSSE("resp_1", "OK"))
		case 2:
			io.WriteString(w, contSSE("resp_2", "OK2"))
		default:
			io.WriteString(w, contSSE("resp_3", "OK3"))
		}
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(true, user1), "session-1"); rec.Code != http.StatusOK {
		t.Fatalf("首轮应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if log := lastLog(t, st); log.ContinuationMode != "seed" {
		t.Fatalf("首轮应记 seed，实际 %q（原因 %q）", log.ContinuationMode, log.ContinuationReason)
	}

	mu.Lock()
	b1 := parseContBody(t, bodies[0])
	mu.Unlock()
	if b1["store"] != true {
		t.Errorf("首轮应补 store:true，实际 %#v", b1["store"])
	}
	if _, ok := b1["previous_response_id"]; ok {
		t.Errorf("首轮不应带 previous_response_id")
	}

	input2 := []any{user1, contAssistant("OK"), contUser("again")}
	if rec := postContinuation(t, srv, contBody(true, input2...), "session-1"); rec.Code != http.StatusOK {
		t.Fatalf("第二轮应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	log2 := lastLog(t, st)
	if log2.ContinuationMode != "continued" {
		t.Fatalf("第二轮应记 continued，实际 %q（原因 %q）", log2.ContinuationMode, log2.ContinuationReason)
	}
	if log2.SavedBytes <= 0 {
		t.Errorf("第二轮应记录节省字节，实际 %d", log2.SavedBytes)
	}
	if !log2.RequestModified {
		t.Error("续传改写应标记 RequestModified")
	}

	mu.Lock()
	b2 := parseContBody(t, bodies[1])
	mu.Unlock()
	if b2["previous_response_id"] != "resp_1" {
		t.Errorf("第二轮 previous_response_id 应为 resp_1，实际 %#v", b2["previous_response_id"])
	}
	if got := contInput(t, b2); len(got) != 1 {
		t.Fatalf("第二轮只应发增量 1 项，实际 %d", len(got))
	}

	input3 := []any{user1, contAssistant("OK"), contUser("again"), contAssistant("OK2"), contUser("third")}
	if rec := postContinuation(t, srv, contBody(true, input3...), "session-1"); rec.Code != http.StatusOK {
		t.Fatalf("第三轮应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	b3 := parseContBody(t, bodies[2])
	mu.Unlock()
	if b3["previous_response_id"] != "resp_2" {
		t.Errorf("第三轮 previous_response_id 应为 resp_2，实际 %#v", b3["previous_response_id"])
	}
}

// ---------- 工具轮 ----------

func TestContinuationToolRound(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	fc := map[string]any{
		"type": "function_call", "call_id": "call_1", "name": "read",
		"arguments": `{"path":"a.txt"}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, contToolSSE("resp_tool_1", fc))
			return
		}
		io.WriteString(w, contSSE("resp_tool_2", "done"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	user1 := contUser("read a.txt")
	if rec := postContinuation(t, srv, contBody(true, user1), "tool-session"); rec.Code != http.StatusOK {
		t.Fatalf("首轮应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 客户端把 function_call 原样放回历史，再追加工具结果。
	input2 := []any{user1, fc, map[string]any{
		"type": "function_call_output", "call_id": "call_1", "output": "file content",
	}}
	if rec := postContinuation(t, srv, contBody(true, input2...), "tool-session"); rec.Code != http.StatusOK {
		t.Fatalf("工具轮应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	b2 := parseContBody(t, bodies[1])
	mu.Unlock()
	if b2["previous_response_id"] != "resp_tool_1" {
		t.Fatalf("工具轮 previous_response_id 应为 resp_tool_1，实际 %#v", b2["previous_response_id"])
	}
	got := contInput(t, b2)
	if len(got) != 1 {
		t.Fatalf("工具轮只应发 function_call_output，实际 %d 项", len(got))
	}
	if item, _ := got[0].(map[string]any); item["type"] != "function_call_output" {
		t.Fatalf("增量第一项应为 function_call_output，实际 %#v", got[0])
	}
	if log := lastLog(t, st); log.ContinuationMode != "continued" {
		t.Fatalf("工具轮应记 continued，实际 %q", log.ContinuationMode)
	}
}

// 真实 ZCode 客户端回放历史时不带回上一轮的 reasoning 项，偶尔还丢一个
// 没执行的 function_call。链前缀必须容忍这种缺项，否则这两类轮次全部失配。
func TestContinuationReplayDropsOutputItems(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			io.WriteString(w, contItemsSSE("resp_drop1",
				contReasoning("先看看目录"),
				contCall("call_1", "ls"),
				contCall("call_2", "pwd"),
			))
		default:
			io.WriteString(w, contItemsSSE(fmt.Sprintf("resp_drop%d", n), contAssistant("OK")))
		}
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	const session = "replay-drop"
	u1 := contUser("第一轮")
	if rec := postContinuation(t, srv, contBody(true, u1), session); rec.Code != http.StatusOK {
		t.Fatalf("第一轮应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if log := lastLog(t, st); log.ContinuationMode != "seed" {
		t.Fatalf("第一轮应 seed，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}

	// ZCode 形态：不回放 reasoning，也不回放没执行的 call_1；只回放 call_2 与工具结果。
	turn2 := []any{u1, contReplayCall("call_2", "pwd"), contCallOutput("call_2", "/tmp")}
	postContinuation(t, srv, contBody(true, turn2...), session)
	log := lastLog(t, st)
	if log.ContinuationMode != "continued" {
		t.Fatalf("丢掉 reasoning 与未执行的 fc 后应继续续传，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}

	mu.Lock()
	last := parseContBody(t, bodies[len(bodies)-1])
	mu.Unlock()
	if got := contInput(t, last); len(got) != 1 {
		t.Fatalf("续传轮应只发 1 项增量，实际 %d: %s", len(got), contJSONString(got))
	}
	if last["previous_response_id"] != "resp_drop1" {
		t.Errorf("应引用第一轮的 response id，实际 %v", last["previous_response_id"])
	}
}

// output 段被客户端全部丢弃时也要能续传：链的 input 段仍然对得上，
// 丢掉的多半是上游已经存过的内容。
func TestContinuationReplayDropsWholeOutput(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			io.WriteString(w, contItemsSSE("resp_whole1", contReasoning("想一想"), contAssistant("第 1 轮回答")))
		default:
			io.WriteString(w, contItemsSSE(fmt.Sprintf("resp_whole%d", n), contAssistant("OK")))
		}
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	const session = "replay-whole"
	u1 := contUser("第一轮")
	postContinuation(t, srv, contBody(true, u1), session)

	// 不回放任何 output 项，直接接着历史往下说。
	turn2 := []any{u1, contUser("第二轮")}
	postContinuation(t, srv, contBody(true, turn2...), session)
	log := lastLog(t, st)
	if log.ContinuationMode != "continued" {
		t.Fatalf("output 段全丢时仍应续传，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}
	mu.Lock()
	last := parseContBody(t, bodies[len(bodies)-1])
	mu.Unlock()
	if got := contInput(t, last); len(got) != 1 {
		t.Fatalf("增量应只剩新的一轮用户消息，实际 %d 项", len(got))
	}
}

// 历史被真正改写（input 段不一致）时必须放弃续传，否则会给上游送去错位的上下文。
func TestContinuationReplayRewritesHistory(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contItemsSSE("resp_rw", contAssistant("OK")))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	const session = "replay-rewrite"
	postContinuation(t, srv, contBody(true, contUser("原始问题")), session)
	postContinuation(t, srv, contBody(true, contUser("另一条完全不同的问题"), contUser("追问")), session)
	log := lastLog(t, st)
	if log.ContinuationMode != "seed" || log.ContinuationReason != "prefix-mismatch" {
		t.Fatalf("input 段改写后应判 prefix-mismatch，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}
}

// ZCode 的并行 subagent 共用会话头但各自维护独立历史。
// 单一槽位会让它们互相覆盖，表现为 continued 与 prefix-mismatch 反复抖动；
// 多条分支链 + 最长前缀匹配才能各自接回自己的分支。
func TestContinuationParallelBranchesDoNotEvict(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contSSE(fmt.Sprintf("resp_%d", n), fmt.Sprintf("OK%d", n)))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	const session = "multi-branch"
	root := contUser("root")

	// 发一次并返回上游给的 assistant 文本，供下一轮原样回放。
	answerOf := func(body string) string {
		t.Helper()
		rec := postContinuation(t, srv, body, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d: %s", rec.Code, rec.Body.String())
		}
		m := regexp.MustCompile(`"delta":"([^"]+)"`).FindStringSubmatch(rec.Body.String())
		if len(m) < 2 {
			t.Fatalf("响应里没有 delta: %s", rec.Body.String())
		}
		return m[1]
	}

	// 两个分支共享同一段开头（system + root），之后各走各的。
	branchA1 := []any{contUser("system-A"), root, contUser("A step 1")}
	branchB1 := []any{contUser("system-B"), root, contUser("B step 1")}

	// 分支 A 建链。
	aAnswer1 := answerOf(contBody(true, branchA1...))
	if log := lastLog(t, st); log.ContinuationMode != "seed" {
		t.Fatalf("A1 应 seed，实际 %q", log.ContinuationMode)
	}

	// 分支 B 前缀不匹配，应自建一条链，而不是挤掉 A 的链。
	bAnswer1 := answerOf(contBody(true, branchB1...))
	if log := lastLog(t, st); log.ContinuationMode != "seed" || log.ContinuationReason != "prefix-mismatch" {
		t.Fatalf("B1 应 seed/prefix-mismatch，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}

	aHistory := append(append([]any{}, branchA1...), contAssistant(aAnswer1))
	bHistory := append(append([]any{}, branchB1...), contAssistant(bAnswer1))

	// 两个分支交替推进 3 轮，每轮都必须续上自己那条链。
	for round := 2; round <= 4; round++ {
		aInput := append(append([]any{}, aHistory...), contUser(fmt.Sprintf("A step %d", round)))
		aAnswer := answerOf(contBody(true, aInput...))
		if log := lastLog(t, st); log.ContinuationMode != "continued" {
			t.Errorf("A%d 应 continued，实际 %s/%s", round, log.ContinuationMode, log.ContinuationReason)
		}
		aHistory = append(append([]any{}, aInput...), contAssistant(aAnswer))

		bInput := append(append([]any{}, bHistory...), contUser(fmt.Sprintf("B step %d", round)))
		bAnswer := answerOf(contBody(true, bInput...))
		if log := lastLog(t, st); log.ContinuationMode != "continued" {
			t.Errorf("B%d 应 continued，实际 %s/%s", round, log.ContinuationMode, log.ContinuationReason)
		}
		bHistory = append(append([]any{}, bInput...), contAssistant(bAnswer))
	}

	// 续传轮只应发增量：最后一条请求的 input 项数应为 1。
	mu.Lock()
	last := parseContBody(t, bodies[len(bodies)-1])
	mu.Unlock()
	if got := contInput(t, last); len(got) != 1 {
		t.Errorf("续传轮应只发 1 项增量，实际 %d", len(got))
	}
}

// ---------- 隔离 ----------
func TestContinuationIsolation(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(body map[string]any, session *string)
		wantMode  string
		wantCause string
	}{
		{
			name: "prefix-mismatch",
			mutate: func(body map[string]any, _ *string) {
				body["input"] = []any{contUser("hello!"), contAssistant("OK"), contUser("again")}
			},
			wantMode: "seed", wantCause: "prefix-mismatch",
		},
		{
			name: "params-change",
			mutate: func(body map[string]any, _ *string) {
				body["temperature"] = 0.5
			},
			wantMode: "seed", wantCause: "no-chain",
		},
		{
			name: "model-change",
			mutate: func(body map[string]any, _ *string) {
				body["model"] = "other-model"
			},
			wantMode: "seed", wantCause: "no-chain",
		},
		{
			name: "session-change",
			mutate: func(_ map[string]any, session *string) {
				*session = "session-2"
			},
			wantMode: "seed", wantCause: "no-chain",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(raw))
				n := len(bodies)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, contSSE(fmt.Sprintf("resp_%d", n), "OK"))
			}))
			defer upstream.Close()

			srv, st := newTestServer(t)
			newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

			user1 := contUser("hello")
			if rec := postContinuation(t, srv, contBody(true, user1), "session-1"); rec.Code != http.StatusOK {
				t.Fatalf("首轮应 200，实际 %d", rec.Code)
			}

			session := "session-1"
			full := map[string]any{
				"model":  "test-model",
				"input":  []any{user1, contAssistant("OK"), contUser("again")},
				"store":  false,
				"stream": true,
			}
			tc.mutate(full, &session)
			raw, _ := json.Marshal(full)
			if rec := postContinuation(t, srv, string(raw), session); rec.Code != http.StatusOK {
				t.Fatalf("第二轮应 200，实际 %d", rec.Code)
			}

			mu.Lock()
			b2 := parseContBody(t, bodies[1])
			mu.Unlock()
			if _, ok := b2["previous_response_id"]; ok {
				t.Errorf("不应复用旧链，实际带了 %#v", b2["previous_response_id"])
			}
			if got := contInput(t, b2); len(got) != 3 {
				t.Errorf("应发全量 3 项，实际 %d", len(got))
			}
			log := lastLog(t, st)
			if log.ContinuationMode != tc.wantMode || log.ContinuationReason != tc.wantCause {
				t.Errorf("应记 %s/%s，实际 %s/%s", tc.wantMode, tc.wantCause, log.ContinuationMode, log.ContinuationReason)
			}
		})
	}
}

// ---------- 回退与冷却 ----------

func TestContinuationFallbackAndCooldown(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"store: stored responses require a native Responses target; use store=false and send prior output items in input"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contSSE(fmt.Sprintf("resp_fallback_%d", n), "OK"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(true, user1), "fallback-session"); rec.Code != http.StatusOK {
		t.Fatalf("回退后应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if len(bodies) != 2 {
		t.Fatalf("应发生一次回退（2 次上游调用），实际 %d", len(bodies))
	}
	b1 := parseContBody(t, bodies[0])
	b2 := parseContBody(t, bodies[1])
	mu.Unlock()
	if b1["store"] != true {
		t.Errorf("首次应按续传补 store:true，实际 %#v", b1["store"])
	}
	if b2["store"] != false {
		t.Errorf("回退应恢复 store:false，实际 %#v", b2["store"])
	}
	if _, ok := b2["previous_response_id"]; ok {
		t.Errorf("回退不应带 previous_response_id")
	}
	if log := lastLog(t, st); log.ContinuationMode != "fallback" || log.ContinuationReason != "provider-rejected" {
		t.Errorf("应记 fallback/provider-rejected，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}

	// 冷却期内不再尝试续传，直接透传。
	if rec := postContinuation(t, srv, contBody(true, user1), "fallback-session"); rec.Code != http.StatusOK {
		t.Fatalf("冷却期请求应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	if len(bodies) != 3 {
		t.Fatalf("冷却期应只调用一次上游，实际累计 %d", len(bodies))
	}
	b3 := parseContBody(t, bodies[2])
	mu.Unlock()
	if _, ok := b3["previous_response_id"]; ok {
		t.Errorf("冷却期不应带 previous_response_id")
	}
	if b3["store"] != false {
		t.Errorf("冷却期应保持 store:false，实际 %#v", b3["store"])
	}
	if log := lastLog(t, st); log.ContinuationMode != "unsupported" {
		t.Errorf("冷却期应记 unsupported，实际 %q", log.ContinuationMode)
	}
}

// commandcode 对失效的 previous_response_id 有时只回一个泛化的
// “invalid request error”；续传轮遇到这种 400 必须回退，否则客户端直接吃 400。
func TestContinuationContinuedGeneric400FallsBack(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		if n == 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"invalid request error trace_id: abc","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contSSE(fmt.Sprintf("resp_generic_%d", n), "OK"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(true, user1), "generic-400"); rec.Code != http.StatusOK {
		t.Fatalf("首轮应 200，实际 %d", rec.Code)
	}
	input2 := []any{user1, contAssistant("OK"), contUser("again")}
	if rec := postContinuation(t, srv, contBody(true, input2...), "generic-400"); rec.Code != http.StatusOK {
		t.Fatalf("回退后应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if len(bodies) != 3 {
		t.Fatalf("应发生一次回退（3 次上游调用），实际 %d", len(bodies))
	}
	b2 := parseContBody(t, bodies[1])
	b3 := parseContBody(t, bodies[2])
	mu.Unlock()
	if b2["previous_response_id"] != "resp_generic_1" {
		t.Errorf("续传轮应带 resp_generic_1，实际 %#v", b2["previous_response_id"])
	}
	if _, ok := b3["previous_response_id"]; ok {
		t.Errorf("回退不应带 previous_response_id")
	}
	if b3["store"] != false {
		t.Errorf("回退应恢复 store:false，实际 %#v", b3["store"])
	}
	log := lastLog(t, st)
	if log.ContinuationMode != "fallback" || log.ContinuationReason != "invalid-id" {
		t.Errorf("应记 fallback/invalid-id，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}

	// 冷却期内直接透传，不再拿坏 id 去撞。
	if rec := postContinuation(t, srv, contBody(true, input2...), "generic-400"); rec.Code != http.StatusOK {
		t.Fatalf("冷却期请求应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	if len(bodies) != 4 {
		t.Fatalf("冷却期应只调用一次上游，实际累计 %d", len(bodies))
	}
	b4 := parseContBody(t, bodies[3])
	mu.Unlock()
	if _, ok := b4["previous_response_id"]; ok {
		t.Errorf("冷却期不应带 previous_response_id")
	}
	if log := lastLog(t, st); log.ContinuationMode != "unsupported" || log.ContinuationReason != "invalid-id" {
		t.Errorf("冷却期应记 unsupported/invalid-id，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}
}

// 冷却填 0 时，回退后的下一轮应立刻重新建链，而不是进入 unsupported。
func TestContinuationZeroInvalidCooldownReseeds(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		if n == 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"invalid request error trace_id: abc","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contSSE(fmt.Sprintf("resp_zero_%d", n), "OK"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	p := newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)
	p.ContinuationInvalidCooldownSeconds = 0
	if _, err := st.UpdateProvider(p); err != nil {
		t.Fatalf("更新冷却配置失败: %v", err)
	}

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(true, user1), "zero-cool"); rec.Code != http.StatusOK {
		t.Fatalf("首轮应 200，实际 %d", rec.Code)
	}
	input2 := []any{user1, contAssistant("OK"), contUser("again")}
	if rec := postContinuation(t, srv, contBody(true, input2...), "zero-cool"); rec.Code != http.StatusOK {
		t.Fatalf("回退后应 200，实际 %d", rec.Code)
	}
	if log := lastLog(t, st); log.ContinuationMode != "fallback" || log.ContinuationReason != "invalid-id" {
		t.Fatalf("应记 fallback/invalid-id，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}

	// 冷却为 0：同一 scope 的下一轮应立刻 seed（store:true + 全量），不是 unsupported。
	if rec := postContinuation(t, srv, contBody(true, input2...), "zero-cool"); rec.Code != http.StatusOK {
		t.Fatalf("冷却 0 的下一轮应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	if len(bodies) != 4 {
		t.Fatalf("应共 4 次上游调用，实际 %d", len(bodies))
	}
	b4 := parseContBody(t, bodies[3])
	mu.Unlock()
	if _, ok := b4["previous_response_id"]; ok {
		t.Errorf("重新建链不应带 previous_response_id")
	}
	if b4["store"] != true {
		t.Errorf("重新建链应发 store:true，实际 %#v", b4["store"])
	}
	if got := contInput(t, b4); len(got) != 3 {
		t.Errorf("重新建链应发全量 3 项，实际 %d", len(got))
	}
	if log := lastLog(t, st); log.ContinuationMode != "seed" || log.ContinuationReason != "no-chain" {
		t.Errorf("应记 seed/no-chain，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}
}

// ---------- dry-run ----------
func TestContinuationDryRun(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contSSE(fmt.Sprintf("resp_dry_%d", n), "OK"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationDryRun)

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(true, user1), "dry-session"); rec.Code != http.StatusOK {
		t.Fatalf("首轮应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	b1 := parseContBody(t, bodies[0])
	mu.Unlock()
	if b1["store"] != false {
		t.Errorf("dry-run 不应改 store，实际 %#v", b1["store"])
	}
	if log := lastLog(t, st); log.ContinuationMode != "dry-miss" || log.RequestModified {
		t.Errorf("首轮应记 dry-miss 且未改写，实际 %s modified=%v", log.ContinuationMode, log.RequestModified)
	}

	input2 := []any{user1, contAssistant("OK"), contUser("again")}
	if rec := postContinuation(t, srv, contBody(true, input2...), "dry-session"); rec.Code != http.StatusOK {
		t.Fatalf("第二轮应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	b2 := parseContBody(t, bodies[1])
	mu.Unlock()
	if _, ok := b2["previous_response_id"]; ok {
		t.Errorf("dry-run 不应写 previous_response_id")
	}
	if got := contInput(t, b2); len(got) != 3 {
		t.Errorf("dry-run 应原样发全量 3 项，实际 %d", len(got))
	}
	log := lastLog(t, st)
	if log.ContinuationMode != "dry-hit" {
		t.Errorf("第二轮应记 dry-hit，实际 %q（原因 %q）", log.ContinuationMode, log.ContinuationReason)
	}
	if log.SavedBytes <= 0 {
		t.Errorf("dry-hit 应估算节省字节，实际 %d", log.SavedBytes)
	}
	if log.RequestModified {
		t.Error("dry-run 不应标记 RequestModified")
	}
}

// ---------- 非流式 ----------

func TestContinuationNonStreaming(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, contJSONString(map[string]any{
			"id": fmt.Sprintf("resp_ns_%d", n), "status": "completed", "store": true,
			"output": []any{contAssistant("OK")},
		}))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(false, user1), "ns-session"); rec.Code != http.StatusOK {
		t.Fatalf("首轮应 200，实际 %d", rec.Code)
	}
	input2 := []any{user1, contAssistant("OK"), contUser("again")}
	if rec := postContinuation(t, srv, contBody(false, input2...), "ns-session"); rec.Code != http.StatusOK {
		t.Fatalf("第二轮应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	b2 := parseContBody(t, bodies[1])
	mu.Unlock()
	if b2["previous_response_id"] != "resp_ns_1" {
		t.Errorf("非流式也应续传，previous_response_id=%#v", b2["previous_response_id"])
	}
	if got := contInput(t, b2); len(got) != 1 {
		t.Errorf("非流式第二轮应只发增量，实际 %d", len(got))
	}
	if log := lastLog(t, st); log.ContinuationMode != "continued" {
		t.Errorf("非流式应记 continued，实际 %q", log.ContinuationMode)
	}
}

// 供应商明确回 store:false 时不要建链；下一轮重新发 store:true，
// 否则会拿着一个不可用的 id 去撞 400（commandcode 偶发）。
func TestContinuationExplicitStoreFalseDoesNotCommit(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, contSSEStore("resp_nostore_1", "OK", false))
			return
		}
		io.WriteString(w, contSSEStore(fmt.Sprintf("resp_nostore_%d", n), "OK", true))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(true, user1), "nostore-session"); rec.Code != http.StatusOK {
		t.Fatalf("首轮应 200，实际 %d", rec.Code)
	}
	if log := lastLog(t, st); log.ContinuationMode != "seed" {
		t.Fatalf("首轮应记 seed，实际 %q", log.ContinuationMode)
	}

	input2 := []any{user1, contAssistant("OK"), contUser("again")}
	if rec := postContinuation(t, srv, contBody(true, input2...), "nostore-session"); rec.Code != http.StatusOK {
		t.Fatalf("第二轮应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	b2 := parseContBody(t, bodies[1])
	mu.Unlock()
	if _, ok := b2["previous_response_id"]; ok {
		t.Errorf("store:false 不应建链，实际带了 %#v", b2["previous_response_id"])
	}
	if b2["store"] != true {
		t.Errorf("下一轮应重新发 store:true，实际 %#v", b2["store"])
	}
	if got := contInput(t, b2); len(got) != 3 {
		t.Errorf("没有链时应发全量 3 项，实际 %d", len(got))
	}
	if log := lastLog(t, st); log.ContinuationMode != "seed" || log.ContinuationReason != "no-chain" {
		t.Errorf("应记 seed/no-chain，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}
}

// ---------- 失败不提交 ----------
func TestContinuationFailedDoesNotCommit(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, contFailedSSE("resp_failed", "boom"))
			return
		}
		io.WriteString(w, contSSE(fmt.Sprintf("resp_after_%d", n), "OK"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	user1 := contUser("hello")
	if rec := postContinuation(t, srv, contBody(true, user1), "fail-session"); rec.Code != http.StatusOK {
		t.Fatalf("首轮 HTTP 应 200，实际 %d", rec.Code)
	}
	input2 := []any{user1, contAssistant("OK"), contUser("again")}
	if rec := postContinuation(t, srv, contBody(true, input2...), "fail-session"); rec.Code != http.StatusOK {
		t.Fatalf("第二轮应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	b2 := parseContBody(t, bodies[1])
	mu.Unlock()
	if _, ok := b2["previous_response_id"]; ok {
		t.Errorf("失败响应不应提交链，实际带了 %#v", b2["previous_response_id"])
	}
	if got := contInput(t, b2); len(got) != 3 {
		t.Errorf("失败后应发全量 3 项，实际 %d", len(got))
	}
}

// ---------- ZDR ----------

func TestContinuationSkippedNoSession(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contSSE("resp_no_session", "OK"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	// 不带 X-Session-Id，也没有 prompt_cache_key。
	if rec := postContinuation(t, srv, contBody(true, contUser("hello")), ""); rec.Code != http.StatusOK {
		t.Fatalf("无会话请求应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	b1 := parseContBody(t, bodies[0])
	mu.Unlock()
	if b1["store"] != false {
		t.Errorf("缺会话标识时不应改 store，实际 %#v", b1["store"])
	}
	log := lastLog(t, st)
	if log.ContinuationMode != "skipped" || log.ContinuationReason != "no-session" {
		t.Errorf("应记 skipped/no-session，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}
	if log.RequestModified {
		t.Error("跳过时不应标记改写")
	}
}

func TestContinuationFailReleasesActive(t *testing.T) {
	m := newContinuationManager()
	m.mu.Lock()
	m.active["chain"] = 7
	m.generations["chain"] = 7
	m.mu.Unlock()

	m.fail(&continuationAttempt{chainKey: "chain", generation: 7})
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) != 0 || len(m.generations) != 0 {
		t.Errorf("失败后应释放 active/generation，实际 active=%v generations=%v", m.active, m.generations)
	}
}

func TestContinuationZDRPassThrough(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contSSE("resp_zdr", "OK"))
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	newContinuationProvider(t, st, upstream.URL, model.ContinuationOn)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(contBody(true, contUser("hello"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", "zdr-session")
	req.Header.Set("x-zero-data-retention", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ZDR 请求应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	b1 := parseContBody(t, bodies[0])
	mu.Unlock()
	if b1["store"] != false {
		t.Errorf("ZDR 不应开启 store，实际 %#v", b1["store"])
	}
	log := lastLog(t, st)
	if log.RequestModified {
		t.Error("ZDR 不应改写请求")
	}
	if log.ContinuationMode != "skipped" || log.ContinuationReason != "zdr" {
		t.Errorf("ZDR 应记 skipped/zdr，实际 %s/%s", log.ContinuationMode, log.ContinuationReason)
	}
}

// ---------- 归一化 ----------

func TestContinuationNormalization(t *testing.T) {
	same := []struct {
		name string
		a    any
		b    any
	}{
		{
			name: "content string vs text block",
			a:    map[string]any{"role": "user", "content": "hi"},
			b:    map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
		{
			name: "strip id and status",
			a:    map[string]any{"type": "message", "role": "assistant", "id": "msg_1", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}},
			b:    map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "OK"}}},
		},
		{
			name: "function arguments string vs object",
			a:    map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": `{"a":1,"b":2}`},
			b:    map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": map[string]any{"b": 2, "a": 1}},
		},
		{
			name: "reasoning content vs summary",
			a:    map[string]any{"type": "reasoning", "encrypted_content": "enc", "content": []any{map[string]any{"type": "reasoning_text", "text": "think"}}},
			b:    map[string]any{"type": "reasoning", "encrypted_content": "enc", "summary": []any{map[string]any{"type": "summary_text", "text": "think"}}},
		},
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			ha := continuationHash(continuationNormalizeItem(tc.a))
			hb := continuationHash(continuationNormalizeItem(tc.b))
			if ha != hb {
				t.Fatalf("应视为同一历史项，hash 不同: %s vs %s", ha, hb)
			}
		})
	}

	phaseA := continuationHash(continuationNormalizeItem(map[string]any{
		"role": "assistant", "phase": "final_answer", "content": "hi",
	}))
	phaseB := continuationHash(continuationNormalizeItem(map[string]any{
		"role": "assistant", "phase": "commentary", "content": "hi",
	}))
	if phaseA == phaseB {
		t.Error("phase 不同不应视为同一历史项")
	}
}

// ---------- 链键与前缀 ----------

func TestContinuationChainKeyAndPrefix(t *testing.T) {
	prov := model.Provider{ID: 1}
	base := continuationChainKey(prov, "m", "k1", "s1", "p1", "/v1/responses")
	cases := []struct{ name, got string }{
		{"model", continuationChainKey(prov, "m2", "k1", "s1", "p1", "/v1/responses")},
		{"key", continuationChainKey(prov, "m", "k2", "s1", "p1", "/v1/responses")},
		{"session", continuationChainKey(prov, "m", "k1", "s2", "p1", "/v1/responses")},
		{"params", continuationChainKey(prov, "m", "k1", "s1", "p2", "/v1/responses")},
		{"path", continuationChainKey(prov, "m", "k1", "s1", "p1", "/v1/other")},
	}
	for _, tc := range cases {
		if tc.got == base {
			t.Errorf("%s 变化不应得到同一个链键", tc.name)
		}
	}

	// 匹配语义：input 段严格相等，output 段允许客户端丢项。
	chain := &continuationChain{
		prefix:   []string{"s", "u1", "a1", "r1", "fc1", "fc2"},
		inputLen: 3,
	}
	if got := continuationMatch([]string{"s", "u1", "a1", "r1", "fc1", "fc2", "new"}, chain); got != 6 {
		t.Errorf("完整回放应在 6 处起算增量，实际 %d", got)
	}
	if got := continuationMatch([]string{"s", "u1", "a1", "fc1", "fc2", "new"}, chain); got != 5 {
		t.Errorf("丢 reasoning 后仍应续传（增量起点 5），实际 %d", got)
	}
	if got := continuationMatch([]string{"s", "u1", "a1", "new"}, chain); got != 3 {
		t.Errorf("output 段全丢时增量起点应为 input 段末尾 3，实际 %d", got)
	}
	if got := continuationMatch([]string{"s", "u1", "x1", "new"}, chain); got != -1 {
		t.Errorf("input 段不一致不应续传，实际 %d", got)
	}
	if got := continuationMatch([]string{"s", "u1", "a1", "r1", "fc1", "fc2"}, chain); got != -1 {
		t.Errorf("没有新增内容不应续传（避免空增量），实际 %d", got)
	}
	if got := continuationMatch([]string{"s", "u1", "a1"}, chain); got != -1 {
		t.Errorf("只回放到 input 段末尾不应续传，实际 %d", got)
	}
}
