package proxy

// Responses 会话续传。
//
// 目标：把客户端每轮重发的完整 input 截成增量，配合 store:true +
// previous_response_id，让上游复用已保存的上下文。是否真的有效完全取决于
// 上游模型；本文件只负责「命中就改写、失败就回退、全程留痕」。
//
// 安全边界：
//   - 只有供应商显式开启 on 才会修改请求；默认 off。
//   - dry-run 不改请求、不设置 store，只统计前缀命中与可节省字节。
//   - 前缀必须逐项匹配（归一化后哈希），历史、参数、模型、账号任一变化都不复用。
//   - 同一链有请求在飞时，新请求发全量；只有最新完成的请求能提交链。
//   - 失败（400/404/409/422/503）回退到原始改写后 body，并冷却该
//     provider+model+key 一段时间，避免反复打失败请求。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai_proxy/internal/model"
)

const (
	continuationTTL       = 6 * time.Hour
	continuationMaxChains = 512
	continuationMaxItems  = 8192
	continuationMaxBody   = 64 << 20
	continuationErrorBody = 1 << 20
)

type continuationManager struct {
	mu          sync.Mutex
	chains      map[string][]*continuationChain
	caps        map[string]*continuationCap
	generations map[string]uint64
	active      map[string]uint64
	serial      uint64
	now         func() time.Time
}

// continuationChain 是一条可续传的分支。
//
// 同一会话会同时存在多条分支：ZCode 的并行 subagent 共用会话头，
// 但各自维护独立历史。如果只留一个槽位，它们会互相覆盖，
// 表现为 continued 与 prefix-mismatch 反复抖动。
type continuationChain struct {
	responseID string
	prefix     []string
	// inputLen 是 prefix 中属于「本轮请求 input」的项数，其后为 output 段。
	// 匹配时 input 段逐项严格相等；output 段允许客户端丢项（ZCode 不回放
	// reasoning，偶尔还丢一个没执行的 function_call）——那部分上游已经存过。
	inputLen  int
	expiresAt time.Time
	updatedAt time.Time
}

type continuationCap struct {
	state     string
	reason    string
	expiresAt time.Time
}

// continuationAttempt 是一次请求上的续传决策，也承载提交链所需的上下文。
// baseBody 始终是「常规改写之后、续传改写之前」的 body，回退时用它重发。
type continuationAttempt struct {
	enabled     bool
	dryRun      bool
	mode        string
	reason      string
	chainKey    string
	branchKey   string
	scope       string
	generation  uint64
	baseBody    []byte
	inputHashes []string
	inputLen    int
	savedBytes  int64
	model       string
}

func newContinuationManager() *continuationManager {
	return &continuationManager{
		chains:      map[string][]*continuationChain{},
		caps:        map[string]*continuationCap{},
		generations: map[string]uint64{},
		active:      map[string]uint64{},
		now:         time.Now,
	}
}

func continuationSkipped(reason string) *continuationAttempt {
	return &continuationAttempt{mode: "skipped", reason: reason}
}

func (m *continuationManager) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chains = map[string][]*continuationChain{}
	m.caps = map[string]*continuationCap{}
	m.generations = map[string]uint64{}
	m.active = map[string]uint64{}
}

// prepare 决定本次请求是否续传，并直接改写 in.outBody。
func (m *continuationManager) prepare(in *forwardInput, r *http.Request, key model.APIKey) *continuationAttempt {
	prov := in.provider
	mode := prov.ContinuationMode
	if mode != model.ContinuationOn && mode != model.ContinuationDryRun {
		return nil
	}
	if prov.APIFormat != model.FormatResponses || !continuationPath(in.upstreamPath) {
		return nil
	}
	if len(in.outBody) == 0 {
		return continuationSkipped("empty-body")
	}
	if len(in.outBody) > continuationMaxBody {
		return continuationSkipped("body-too-large")
	}
	if continuationZDR(r.Header) {
		return continuationSkipped("zdr")
	}

	body, err := decodeJSONObject(in.outBody)
	if err != nil || body == nil {
		return continuationSkipped("parse-error")
	}
	// 客户端自己管理续传时完全不插手。
	if _, ok := body["previous_response_id"]; ok {
		return continuationSkipped("client-managed")
	}
	if _, ok := body["conversation"]; ok {
		return continuationSkipped("conversation")
	}
	if background, _ := body["background"].(bool); background {
		return continuationSkipped("background")
	}

	input, ok := continuationInput(body)
	if !ok {
		return continuationSkipped("input-unsupported")
	}
	if len(input) > continuationMaxItems {
		return continuationSkipped("input-too-large")
	}
	session := continuationSessionKey(r, body)
	if session == "" {
		return continuationSkipped("no-session")
	}
	modelName := continuationModel(body, in)
	if modelName == "" {
		return continuationSkipped("no-model")
	}
	if !continuationModelInScope(prov, modelName) {
		return continuationSkipped("model-filtered")
	}

	// 某些供应商的 key 没有配置 ID；用 key 值哈希兜底，避免不同账号共用同一条链。
	keyID := strings.TrimSpace(key.ID)
	if keyID == "" {
		keyID = "key:" + continuationHashString(key.Key)
	}

	hashes := continuationItemHashes(input)
	params := continuationParamsHash(body)
	chainKey := continuationChainKey(prov, modelName, keyID, session, params, in.upstreamPath)
	scope := continuationCapKey(prov, modelName, keyID)

	attempt := &continuationAttempt{
		enabled:     mode == model.ContinuationOn,
		dryRun:      mode == model.ContinuationDryRun,
		mode:        mode,
		chainKey:    chainKey,
		scope:       scope,
		baseBody:    in.outBody,
		inputHashes: hashes,
		inputLen:    len(input),
		model:       modelName,
	}

	m.mu.Lock()
	m.pruneLocked()
	if mode == model.ContinuationOn {
		if cap := m.caps[scope]; cap != nil && cap.expiresAt.After(m.now()) {
			m.mu.Unlock()
			attempt.enabled = false
			attempt.mode = "unsupported"
			attempt.reason = cap.reason
			return attempt
		}
	}

	now := m.now()
	chains := m.chains[chainKey]
	// 在同会话的多条分支里挑 input 段最长的那条：并行的 subagent 各自维护
	// 独立历史，最长匹配才能把请求接回它自己那条分支。
	var chain *continuationChain
	deltaStart := -1
	for _, c := range chains {
		if !c.expiresAt.After(now) {
			continue
		}
		start := continuationMatch(hashes, c)
		if start < 0 {
			continue
		}
		if chain == nil || c.inputLen > chain.inputLen || (c.inputLen == chain.inputLen && start > deltaStart) {
			chain = c
			deltaStart = start
		}
	}
	prefixOK := chain != nil
	useID := prefixOK && (mode == model.ContinuationDryRun || chain.responseID != "")
	reason := ""
	switch {
	case len(chains) == 0:
		reason = "no-chain"
	case !prefixOK:
		reason = "prefix-mismatch"
	case !useID:
		reason = "not-stored"
	}

	// 分支键用本轮的完整输入指纹：不同分支各自独立占用 active，
	// 同一条分支上仍然只允许一个在飞请求。
	branchKey := chainKey + "\x00" + continuationHashString(strings.Join(hashes, ""))
	if chain != nil {
		branchKey = chainKey + "\x00" + continuationHashString(strings.Join(chain.prefix, ""))
	}
	if active := m.active[branchKey]; active != 0 {
		// 同一分支已在飞：改为全量，避免两路同时引用同一个 response id。
		chain = nil
		useID = false
		reason = "active-inflight"
	}

	// 所有请求都拿一个 generation；只有最新完成的才能提交，避免并发分支互相覆盖。
	m.serial++
	gen := m.serial
	m.generations[branchKey] = gen
	m.active[branchKey] = gen
	m.mu.Unlock()
	attempt.generation = gen
	attempt.branchKey = branchKey

	if mode == model.ContinuationDryRun {
		if useID {
			if raw := continuationDeltaBody(body, input, deltaStart, chain.responseID); raw != nil {
				attempt.mode = "dry-hit"
				attempt.reason = "prefix-match"
				attempt.savedBytes = int64(len(in.outBody) - len(raw))
			}
		} else {
			attempt.mode = "dry-miss"
			attempt.reason = reason
		}
		return attempt
	}

	wire := continuationClone(body)
	if useID {
		wire["input"] = input[deltaStart:]
		wire["previous_response_id"] = chain.responseID
		wire["store"] = true
		attempt.mode = "continued"
		attempt.reason = "prefix-match"
	} else {
		wire["store"] = true
		attempt.mode = "seed"
		attempt.reason = reason
	}
	out, err := json.Marshal(wire)
	if err != nil {
		m.mu.Lock()
		m.releaseLocked(chainKey, gen)
		m.mu.Unlock()
		return nil
	}

	if useID {
		attempt.savedBytes = int64(len(in.outBody) - len(out))
		in.modifications = append(in.modifications, "responses_continuation")
	} else {
		attempt.savedBytes = 0
		in.modifications = append(in.modifications, "responses_continuation_seed")
	}
	if original, ok := body["store"].(bool); ok && !original {
		in.modifications = append(in.modifications, "responses_store_enabled")
	}
	in.outBody = out
	return attempt
}

// commit 在响应结束后更新链。dry-run 也更新前缀，但不写入可用的 response id。
func (m *continuationManager) commit(attempt *continuationAttempt, res logResult) {
	if attempt == nil || attempt.chainKey == "" || attempt.generation == 0 {
		return
	}
	branchKey := attempt.branchKey
	if branchKey == "" {
		branchKey = attempt.chainKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.releaseLocked(branchKey, attempt.generation)

	if m.generations[branchKey] != attempt.generation {
		return
	}
	if res.status != http.StatusOK || res.responseStatus != "completed" || len(res.responseOutput) == 0 {
		return
	}
	if !attempt.dryRun && (!attempt.enabled || res.responseID == "") {
		return
	}
	// 供应商明确回 store:false 说明这条响应没有被保存，id 下次多半不可用
	// （commandcode 偶发这种情况）。不要建链，下一轮重新发一次 store:true。
	if !attempt.dryRun && res.responseStore != nil && !*res.responseStore {
		return
	}

	outputHashes := continuationOutputHashes(res.responseOutput)
	prefix := make([]string, 0, len(attempt.inputHashes)+len(outputHashes))
	prefix = append(prefix, attempt.inputHashes...)
	prefix = append(prefix, outputHashes...)
	id := res.responseID
	if attempt.dryRun {
		// dry-run 的响应没有 store，不能拿去给 on 模式当 previous_response_id。
		id = ""
	}
	now := m.now()

	// 覆盖同一分支的旧链（旧链的 input 段是本次 input 段的前缀），其余分支保留。
	kept := make([]*continuationChain, 0, len(m.chains[attempt.chainKey])+1)
	for _, c := range m.chains[attempt.chainKey] {
		if !c.expiresAt.After(now) {
			continue
		}
		// 只比 input 段：客户端回放本来就丢项，拿完整前缀比较判不出「同一分支」，
		// 会让本分支的旧链一直留着。
		if continuationInputOverlap(c.prefix, c.inputLen, attempt.inputHashes) {
			continue
		}
		kept = append(kept, c)
	}
	kept = append(kept, &continuationChain{
		responseID: id,
		prefix:     prefix,
		inputLen:   attempt.inputLen,
		expiresAt:  now.Add(continuationTTL),
		updatedAt:  now,
	})
	m.chains[attempt.chainKey] = kept
	if attempt.enabled {
		delete(m.caps, attempt.scope)
	}
	m.enforceLimitLocked()
}

// continuationMatch 判断本轮 input 能否接到链上，返回增量起点，-1 表示接不上。
//
// input 段必须逐项相等：这是「同一条历史线」的判据，分支隔离靠它。
// output 段允许客户端丢项：回放了就消费，没回放就跳过——上一轮的响应整体
// 已在上游上下文里，少回放几项不改变语义。ZCode 就从不回放 reasoning，
// 偶尔还丢一个没执行的 function_call，按完整前缀比较会让这些轮次全部失配。
func continuationMatch(input []string, chain *continuationChain) int {
	if chain == nil {
		return -1
	}
	il := chain.inputLen
	if il <= 0 || il > len(chain.prefix) || len(input) <= il {
		return -1
	}
	for i := 0; i < il; i++ {
		if input[i] != chain.prefix[i] {
			return -1
		}
	}
	i := il
	for j := il; j < len(chain.prefix); j++ {
		if i < len(input) && input[i] == chain.prefix[j] {
			i++
		}
	}
	if i >= len(input) {
		return -1 // 没有新内容，发空增量没有意义
	}
	return i
}

// continuationInputOverlap 报告旧链 a 的 input 段是否为新链 input 段（b）的前缀。
//
// 只有真前缀才算同一分支的推进：并行分支常常共享开头（同一条 system +
// 首个用户消息），用「有公共前缀」判断会误删别人的链。
func continuationInputOverlap(a []string, alen int, b []string) bool {
	if alen <= 0 || alen > len(a) || alen > len(b) {
		return false
	}
	for i := 0; i < alen; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *continuationManager) markUnsupported(scope, reason string, ttl time.Duration) {
	if scope == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caps[scope] = &continuationCap{state: "unsupported", reason: reason, expiresAt: m.now().Add(ttl)}
}

// drop 丢弃该会话下与本次请求同分支（input 段重叠）的链。下一轮在冷却结束后
// 会重新发一次 store:true 建链，而不是反复拿着一个坏 id 去撞 400。
func (m *continuationManager) drop(attempt *continuationAttempt) {
	if attempt == nil || attempt.chainKey == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt.branchKey != "" {
		delete(m.generations, attempt.branchKey)
		delete(m.active, attempt.branchKey)
	}
	chains := m.chains[attempt.chainKey]
	if len(chains) == 0 {
		return
	}
	kept := chains[:0]
	for _, c := range chains {
		if continuationInputOverlap(c.prefix, c.inputLen, attempt.inputHashes) {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		delete(m.chains, attempt.chainKey)
		return
	}
	m.chains[attempt.chainKey] = kept
}

func (m *continuationManager) releaseLocked(branchKey string, gen uint64) {
	if branchKey == "" || gen == 0 {
		return
	}
	if m.active[branchKey] == gen {
		delete(m.active, branchKey)
	}
	if m.generations[branchKey] == gen {
		delete(m.generations, branchKey)
	}
}

// fail 释放一次没有走到 relay 的尝试；否则 active 会永久占住该分支，
// 后续同会话请求会一直被判为 active-inflight。
func (m *continuationManager) fail(attempt *continuationAttempt) {
	if attempt == nil || attempt.generation == 0 {
		return
	}
	branchKey := attempt.branchKey
	if branchKey == "" {
		branchKey = attempt.chainKey
	}
	if branchKey == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseLocked(branchKey, attempt.generation)
}

func (m *continuationManager) pruneLocked() {
	now := m.now()
	for k, chains := range m.chains {
		kept := chains[:0]
		for _, c := range chains {
			if c.expiresAt.After(now) {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			delete(m.chains, k)
			continue
		}
		m.chains[k] = kept
	}
	for k, c := range m.caps {
		if !c.expiresAt.After(now) {
			delete(m.caps, k)
		}
	}
	m.enforceLimitLocked()
}

func (m *continuationManager) enforceLimitLocked() {
	total := 0
	for _, chains := range m.chains {
		total += len(chains)
	}
	if total <= continuationMaxChains {
		return
	}
	type ref struct {
		key   string
		chain *continuationChain
	}
	all := make([]ref, 0, total)
	for k, chains := range m.chains {
		for _, c := range chains {
			all = append(all, ref{k, c})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].chain.updatedAt.Before(all[j].chain.updatedAt)
	})
	drop := total - continuationMaxChains
	for _, r := range all[:drop] {
		chains := m.chains[r.key]
		kept := make([]*continuationChain, 0, len(chains))
		for _, c := range chains {
			if c != r.chain {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			delete(m.chains, r.key)
		} else {
			m.chains[r.key] = kept
		}
	}
}

// ---------- 请求判定 ----------

func continuationPath(path string) bool {
	return strings.HasSuffix(strings.TrimSuffix(path, "/"), "/responses")
}

func continuationZDR(headers http.Header) bool {
	for _, name := range []string{"x-cmd-zdr", "x-zdr", "x-zero-data-retention"} {
		switch strings.ToLower(strings.TrimSpace(headers.Get(name))) {
		case "1", "true", "enabled", "enable":
			return true
		}
	}
	return false
}

func continuationSessionKey(r *http.Request, body map[string]any) string {
	for _, name := range []string{"X-Session-Id", "X-Opencode-Session", "Session_id"} {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	if v, ok := body["prompt_cache_key"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func continuationModel(body map[string]any, in *forwardInput) string {
	if v, ok := body["model"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if in.modelMapped != "" {
		return in.modelMapped
	}
	return strings.TrimSpace(in.model)
}

func continuationModelInScope(prov model.Provider, modelName string) bool {
	if len(prov.ContinuationModels) == 0 {
		return true
	}
	for _, m := range prov.ContinuationModels {
		if strings.EqualFold(strings.TrimSpace(m), modelName) {
			return true
		}
	}
	return false
}

func continuationInput(body map[string]any) ([]any, bool) {
	switch v := body["input"].(type) {
	case []any:
		return v, true
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, false
		}
		return []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": v}},
		}}, true
	default:
		return nil, false
	}
}

// continuationDeltaBody 生成只带增量 input 的预览 body，供 dry-run 统计可省字节。
func continuationDeltaBody(body map[string]any, input []any, start int, id string) []byte {
	wire := continuationClone(body)
	wire["input"] = input[start:]
	wire["previous_response_id"] = id
	wire["store"] = true
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil
	}
	return raw
}

func continuationClone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---------- 链键与前缀 ----------

func continuationChainKey(prov model.Provider, modelName, keyID, session, params, path string) string {
	return continuationHashString(fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s",
		prov.ID, modelName, keyID, session, params, path))
}

func continuationCapKey(prov model.Provider, modelName, keyID string) string {
	return fmt.Sprintf("%d|%s|%s", prov.ID, modelName, keyID)
}

func continuationParamsHash(body map[string]any) string {
	cp := continuationClone(body)
	delete(cp, "input")
	delete(cp, "stream")
	delete(cp, "store")
	delete(cp, "prompt_cache_key")
	return continuationHash(cp)
}

func continuationHashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func continuationHash(v any) string {
	raw, err := json.Marshal(continuationSortValue(v))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func continuationSortValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			out[k] = continuationSortValue(x[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = continuationSortValue(item)
		}
		return out
	default:
		return v
	}
}

func continuationItemHashes(items []any) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = continuationHash(continuationNormalizeItem(item))
	}
	return out
}

func continuationOutputHashes(items []json.RawMessage) []string {
	out := make([]string, 0, len(items))
	for _, raw := range items {
		var item any
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		out = append(out, continuationHash(continuationNormalizeItem(item)))
	}
	return out
}

// continuationNormalizeItem 把客户端回放与上游回执里「同一条历史项」归一成
// 同一个对象：去掉会变化的 id/status，合并 text 类型，解析 function_call 参数。
// 这是前缀命中率的关键；任何会改变语义的字段都保留。
func continuationNormalizeItem(item any) any {
	m, ok := item.(map[string]any)
	if !ok {
		return continuationSortValue(item)
	}
	if role, ok := m["role"].(string); ok {
		out := map[string]any{"role": role}
		if phase, ok := m["phase"]; ok {
			out["phase"] = continuationSortValue(phase)
		}
		out["content"] = continuationNormalizeContent(m["content"])
		return out
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "reasoning":
		out := map[string]any{"type": "reasoning", "text": continuationReasoningText(m)}
		if v, ok := m["encrypted_content"]; ok {
			out["encrypted_content"] = continuationSortValue(v)
		}
		return out
	case "function_call":
		out := map[string]any{"type": "function_call"}
		for _, k := range []string{"call_id", "name"} {
			if v, ok := m[k]; ok {
				out[k] = continuationSortValue(v)
			}
		}
		args := m["arguments"]
		if s, ok := args.(string); ok {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				args = parsed
			}
		}
		out["arguments"] = continuationSortValue(args)
		return out
	default:
		out := make(map[string]any, len(m))
		for k, v := range m {
			if k == "id" || k == "status" {
				continue
			}
			out[k] = continuationSortValue(v)
		}
		return out
	}
}

func continuationNormalizeContent(v any) []any {
	switch c := v.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		out := make([]any, 0, len(c))
		for _, block := range c {
			if bm, ok := block.(map[string]any); ok {
				if t, _ := bm["type"].(string); t == "text" || t == "input_text" || t == "output_text" {
					out = append(out, map[string]any{"type": "text", "text": bm["text"]})
					continue
				}
			}
			out = append(out, continuationSortValue(block))
		}
		return out
	default:
		return []any{}
	}
}

func continuationReasoningText(m map[string]any) string {
	for _, key := range []string{"content", "summary"} {
		blocks, ok := m[key].([]any)
		if !ok {
			continue
		}
		var sb strings.Builder
		for _, block := range blocks {
			if bm, ok := block.(map[string]any); ok {
				if s, ok := bm["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}

// ---------- 失败回退 ----------

// retryContinuation 在响应头写出之前判断是否需要回退。返回的 resp 已经
// 重新填充过 body（未回退时也一样），调用方可以直接继续 relay。
func (s *Server) retryContinuation(resp *http.Response, in *forwardInput, client *http.Client,
	target string, headers http.Header, ctx context.Context) (*http.Response, error) {
	attempt := in.continuation
	if attempt == nil || !attempt.enabled {
		return resp, nil
	}
	if !continuationRetryStatus(resp.StatusCode) {
		return resp, nil
	}

	// 判断是不是续传错误用不着完整响应体，但错误响应可能很大：超限或读失败
	// 就不重试，把已读部分和剩余流拼回去原样转发，不能截断上游的报错。
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, continuationErrorBody+1))
	if readErr != nil || len(raw) > continuationErrorBody {
		resp.Body = &replayReadCloser{
			Reader: io.MultiReader(bytes.NewReader(raw), resp.Body),
			Closer: resp.Body,
		}
		return resp, nil
	}
	resp.Body.Close()
	body := raw

	retry, reason := continuationRetryBody(resp.StatusCode, body, attempt.mode)
	if !retry {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return resp, nil
	}

	s.logger.Warn("续传被上游拒绝，回退全量",
		"provider", in.provider.DisplayName, "model", attempt.model,
		"status", resp.StatusCode, "reason", reason, "body", continuationSnippet(body))

	// 续传命中轮失败说明上游接受过 store:true，回退重发时把它补回去，
	// 这次全量响应也会被保存，下一轮才有链可用。seed 轮失败不补——那多半
	// 就是 store 本身不被上游接受，补了只会再撞一次。
	wasContinued := attempt.mode == "continued"

	attempt.enabled = false
	attempt.dryRun = false
	attempt.mode = "fallback"
	attempt.reason = reason
	attempt.savedBytes = 0
	in.outBody = attempt.baseBody
	if wasContinued {
		if withStore := continuationWithStore(attempt.baseBody); withStore != nil {
			in.outBody = withStore
			in.modifications = append(in.modifications, "responses_store_enabled")
		}
	}
	in.modifications = append(in.modifications, "responses_continuation_fallback")
	ttl := time.Duration(in.provider.ContinuationCooldownSeconds) * time.Second
	if reason == "invalid-id" {
		ttl = time.Duration(in.provider.ContinuationInvalidCooldownSeconds) * time.Second
	}
	if ttl < 0 {
		ttl = 0
	}
	s.continuation.markUnsupported(attempt.scope, reason, ttl)
	s.continuation.drop(attempt)

	baseReq, err := buildRequest(ctx, http.MethodPost, target, attempt.baseBody, headers)
	if err != nil {
		return nil, err
	}
	return client.Do(baseReq)
}

func continuationRetryStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusServiceUnavailable:
		return true
	}
	return false
}

func continuationRetryBody(status int, body []byte, mode string) (bool, string) {
	// opencode 对不支持的模型/端点会给泛化的 503/404，直接回退一次最稳。
	if status == http.StatusServiceUnavailable {
		return true, "provider-rejected"
	}
	if status == http.StatusNotFound {
		return true, "invalid-id"
	}
	// commandcode 对失效的 previous_response_id 有时只回一个泛化的
	// “invalid request error”，不点名 response id；只要这是续传轮，
	// 就当作 id 不可用回退一次，下一轮重新建链。
	if mode == "continued" {
		switch status {
		case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
			return true, "invalid-id"
		}
	}
	text := strings.ToLower(string(body))
	if !continuationMentionsContinuation(text) {
		return false, ""
	}
	if status == http.StatusBadRequest && strings.Contains(text, "native responses") {
		return true, "provider-rejected"
	}
	if !continuationMentionsFailure(text) {
		return false, ""
	}
	if continuationMentionsInvalid(text) {
		return true, "invalid-id"
	}
	return true, "provider-rejected"
}

func continuationMentionsContinuation(text string) bool {
	for _, kw := range []string{"previous_response_id", "previous response", "response id", "response_id", "store", "native responses"} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func continuationMentionsFailure(text string) bool {
	for _, kw := range []string{
		"unsupported", "not supported", "unrecognized", "unknown parameter",
		"not allowed", "not permitted", "not implemented", "does not support",
		"must be false", "only false", "require", "endpoint is unavailable",
	} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func continuationMentionsInvalid(text string) bool {
	for _, kw := range []string{"not found", "invalid", "expired", "does not exist", "no longer", "deleted", "missing"} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func continuationSnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// continuationWithStore 返回把 store 置为 true 的 body；已经是 true 或解析
// 失败时返回 nil，调用方保持原 body 不变。
func continuationWithStore(body []byte) []byte {
	m, err := decodeJSONObject(body)
	if err != nil || m == nil {
		return nil
	}
	if v, ok := m["store"].(bool); ok && v {
		return nil
	}
	m["store"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// replayReadCloser 把「已经读出来的前缀」和「还没读的剩余流」拼成一体，
// 供提前探过头的错误响应原样转发。
type replayReadCloser struct {
	io.Reader
	io.Closer
}
