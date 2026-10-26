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
	chains      map[string]*continuationChain
	caps        map[string]*continuationCap
	generations map[string]uint64
	active      map[string]uint64
	serial      uint64
	now         func() time.Time
}

type continuationChain struct {
	responseID string
	prefix     []string
	expiresAt  time.Time
	updatedAt  time.Time
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
	scope       string
	generation  uint64
	baseBody    []byte
	inputHashes []string
	savedBytes  int64
	model       string
}

func newContinuationManager() *continuationManager {
	return &continuationManager{
		chains:      map[string]*continuationChain{},
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
	m.chains = map[string]*continuationChain{}
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

	chain := m.chains[chainKey]
	active := m.active[chainKey]
	now := m.now()
	prefixOK := chain != nil && continuationIsPrefix(hashes, chain.prefix)
	useID := prefixOK && chain.expiresAt.After(now) && active == 0 &&
		(mode == model.ContinuationDryRun || chain.responseID != "")
	reason := ""
	switch {
	case chain == nil:
		reason = "no-chain"
	case !chain.expiresAt.After(now):
		reason = "chain-expired"
	case active != 0:
		reason = "active-inflight"
	case !prefixOK:
		reason = "prefix-mismatch"
	}

	// 所有请求都拿一个 generation；只有最新完成的才能提交，避免并发分支互相覆盖。
	m.serial++
	gen := m.serial
	m.generations[chainKey] = gen
	m.active[chainKey] = gen
	m.mu.Unlock()
	attempt.generation = gen

	if mode == model.ContinuationDryRun {
		if useID {
			if delta := continuationDeltaBody(body, input, len(chain.prefix), chain.responseID); delta != nil {
				attempt.mode = "dry-hit"
				attempt.reason = "prefix-match"
				attempt.savedBytes = int64(len(in.outBody) - len(delta))
			}
		} else {
			attempt.mode = "dry-miss"
			attempt.reason = reason
		}
		return attempt
	}

	wire := continuationClone(body)
	if useID {
		wire["input"] = input[len(chain.prefix):]
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
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.releaseLocked(attempt.chainKey, attempt.generation)

	if m.generations[attempt.chainKey] != attempt.generation {
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
	m.chains[attempt.chainKey] = &continuationChain{
		responseID: id,
		prefix:     prefix,
		expiresAt:  now.Add(continuationTTL),
		updatedAt:  now,
	}
	if attempt.enabled {
		delete(m.caps, attempt.scope)
	}
	m.enforceLimitLocked()
}

func (m *continuationManager) markUnsupported(scope, reason string, ttl time.Duration) {
	if scope == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caps[scope] = &continuationCap{state: "unsupported", reason: reason, expiresAt: m.now().Add(ttl)}
}

// drop 丢弃已经失效的链。下一轮在冷却结束后会重新发一次 store:true 建链，
// 而不是反复拿着一个坏 id 去撞 400。
func (m *continuationManager) drop(chainKey string) {
	if chainKey == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.chains, chainKey)
	delete(m.generations, chainKey)
	delete(m.active, chainKey)
}

func (m *continuationManager) releaseLocked(chainKey string, gen uint64) {
	if chainKey == "" || gen == 0 {
		return
	}
	if m.active[chainKey] == gen {
		delete(m.active, chainKey)
	}
	if m.generations[chainKey] == gen {
		delete(m.generations, chainKey)
	}
}

// fail 释放一次没有走到 relay 的尝试；否则 active 会永久占住该链，
// 后续同会话请求会一直被判为 active-inflight。
func (m *continuationManager) fail(attempt *continuationAttempt) {
	if attempt == nil || attempt.chainKey == "" || attempt.generation == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseLocked(attempt.chainKey, attempt.generation)
}

func (m *continuationManager) pruneLocked() {
	now := m.now()
	for k, c := range m.chains {
		if !c.expiresAt.After(now) {
			delete(m.chains, k)
		}
	}
	for k, c := range m.caps {
		if !c.expiresAt.After(now) {
			delete(m.caps, k)
		}
	}
	m.enforceLimitLocked()
}

func (m *continuationManager) enforceLimitLocked() {
	if len(m.chains) <= continuationMaxChains {
		return
	}
	keys := make([]string, 0, len(m.chains))
	for k := range m.chains {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return m.chains[keys[i]].updatedAt.Before(m.chains[keys[j]].updatedAt)
	})
	for _, k := range keys[:len(keys)-continuationMaxChains] {
		delete(m.chains, k)
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

func continuationDeltaBody(body map[string]any, input []any, prefixLen int, id string) []byte {
	wire := continuationClone(body)
	wire["input"] = input[prefixLen:]
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

func continuationIsPrefix(all, prefix []string) bool {
	if len(all) <= len(prefix) {
		return false
	}
	for i, h := range prefix {
		if all[i] != h {
			return false
		}
	}
	return true
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

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, continuationErrorBody))
	resp.Body.Close()
	if readErr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return resp, nil
	}

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

	attempt.enabled = false
	attempt.dryRun = false
	attempt.mode = "fallback"
	attempt.reason = reason
	attempt.savedBytes = 0
	in.outBody = attempt.baseBody
	in.modifications = append(in.modifications, "responses_continuation_fallback")
	ttl := time.Duration(in.provider.ContinuationCooldownSeconds) * time.Second
	if reason == "invalid-id" {
		ttl = time.Duration(in.provider.ContinuationInvalidCooldownSeconds) * time.Second
	}
	if ttl < 0 {
		ttl = 0
	}
	s.continuation.markUnsupported(attempt.scope, reason, ttl)
	s.continuation.drop(attempt.chainKey)

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
