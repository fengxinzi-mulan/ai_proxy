// Package api 提供管理后台的 REST 接口与实时事件通道。
//
// 它只做参数解析、调用 store/proxy、序列化结果三件事，
// 业务规则（谁能用、什么算成功）都留在各自的包里。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/proxy"
	"ai_proxy/internal/store"
)

// Server 是管理接口的处理器集合。
type Server struct {
	store  *store.Store
	hub    *hub.Hub
	proxy  *proxy.Server
	logger *slog.Logger

	// listenHost / listenPort 是进程实际监听的地址。
	// 它可能来自命令行参数而非数据库里的设置，所以单独持有，
	// 让界面展示的客户端接入地址永远与实际一致。
	listenHost string
	listenPort int
}

// New 构造管理接口。
func New(st *store.Store, h *hub.Hub, p *proxy.Server, logger *slog.Logger, listenHost string, listenPort int) *Server {
	return &Server{
		store: st, hub: h, proxy: p, logger: logger,
		listenHost: listenHost, listenPort: listenPort,
	}
}

// Register 把所有管理路由挂到 mux 上。
//
// 路径放在 /api/ 前缀下，转发 handler 只接管其余路径，两者互不干扰。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/health", s.handleHealth)

	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	mux.HandleFunc("GET /api/providers", s.handleListProviders)
	mux.HandleFunc("POST /api/providers", s.handleCreateProvider)
	mux.HandleFunc("POST /api/providers/reorder", s.handleReorderProviders)
	mux.HandleFunc("GET /api/providers/{id}", s.handleGetProvider)
	mux.HandleFunc("PUT /api/providers/{id}", s.handleUpdateProvider)
	mux.HandleFunc("DELETE /api/providers/{id}", s.handleDeleteProvider)
	mux.HandleFunc("POST /api/providers/{id}/activate", s.handleActivateProvider)
	mux.HandleFunc("POST /api/providers/{id}/test", s.handleTestProvider)
	mux.HandleFunc("POST /api/providers/{id}/models", s.handleProviderModels)
	mux.HandleFunc("GET /api/providers/{id}/keys", s.handleKeyHealth)

	mux.HandleFunc("GET /api/logs", s.handleListLogs)
	mux.HandleFunc("DELETE /api/logs", s.handleDeleteLogs)
	mux.HandleFunc("POST /api/logs/clear", s.handleClearLogs)
	mux.HandleFunc("GET /api/logs/{id}", s.handleGetLog)

	mux.HandleFunc("GET /api/models", s.handleDistinctModels)
	mux.HandleFunc("GET /api/stats/overview", s.handleOverview)
	mux.HandleFunc("GET /api/stats/timeseries", s.handleTimeSeries)
	mux.HandleFunc("GET /api/stats/by-model", s.handleByModel)
	mux.HandleFunc("GET /api/stats/by-provider", s.handleByProvider)
	mux.HandleFunc("GET /api/live", s.handleLive)
	mux.HandleFunc("GET /api/events", s.handleEvents)
}

// Auth 是套在所有管理接口外面的访问密钥校验。
//
// 只在设置了 accessKey 时生效；默认监听 127.0.0.1 的本地用法下无需开启。
func (s *Server) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, err := s.store.GetSettings()
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "读取配置失败")
			return
		}
		if st.AccessKey != "" && !checkKey(r, st.AccessKey) {
			s.writeError(w, http.StatusUnauthorized, "访问密钥无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func checkKey(r *http.Request, want string) bool {
	if r.Header.Get("X-AI-Proxy-Key") == want {
		return true
	}
	if r.URL.Query().Get("key") == want {
		return true
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ") == want
	}
	return false
}

// ---------- 基础 ----------

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	type authDefault struct {
		Format string `json:"format"`
		Header string `json:"header"`
		Prefix string `json:"prefix"`
	}
	auths := make([]authDefault, 0, len(model.AllAPIFormats))
	defaults := make([]map[string]any, 0, len(model.AllAPIFormats))
	for _, f := range model.AllAPIFormats {
		h, p := model.DefaultAuthForFormat(f)
		auths = append(auths, authDefault{Format: string(f), Header: h, Prefix: p})
		defaults = append(defaults, map[string]any{
			"format": f, "authHeader": h, "authPrefix": p, "modelsPath": modelsPathHint(f),
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"formats":      model.AllAPIFormats,
		"strategies":   []string{model.StrategyAppend, model.StrategyReplace, model.StrategyPrependUser},
		"proxyTypes":   []string{model.ProxyHTTP, model.ProxySOCKS5},
		"authDefaults": auths,
		"formatHints":  defaults,
		"liveCount":    s.proxy.Live().Count(),
		// 实际监听地址，可能与数据库中的设置不同（命令行参数优先）。
		"listen": map[string]any{"host": s.listenHost, "port": s.listenPort},
	})
}

func modelsPathHint(f model.APIFormat) string {
	if f == model.FormatGemini {
		return "/v1beta/models"
	}
	return "/v1/models"
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now()})
}

// ---------- 设置 ----------

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.GetSettings()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, st)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var incoming model.Settings
	if !s.decode(w, r, &incoming) {
		return
	}
	// 访问密钥留空表示「保持原值」，这样前端不必为了改代理而回传密钥。
	if incoming.AccessKey == "" {
		if cur, err := s.store.GetSettings(); err == nil {
			incoming.AccessKey = cur.AccessKey
		}
	}
	if err := s.store.SaveSettings(incoming); err != nil {
		s.fail(w, err)
		return
	}
	// 代理设置可能变了，旧的连接池必须丢弃。
	s.proxy.InvalidateClients()
	st, _ := s.store.GetSettings()
	s.hub.Broadcast("settings", st)
	s.writeJSON(w, http.StatusOK, st)
}

// ---------- 供应商 ----------

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListProviders()
	if err != nil {
		s.fail(w, err)
		return
	}
	// 附上密钥健康状态，前端不用再逐个请求。
	type providerView struct {
		model.Provider
		KeyHealth map[string]proxy.KeyHealth `json:"keyHealth"`
	}
	out := make([]providerView, 0, len(list))
	for _, p := range list {
		out = append(out, providerView{Provider: p, KeyHealth: s.proxy.KeyHealth(p)})
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	p, err := s.store.GetProvider(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, p)
}

// providerPayload 是供应商的请求体。
//
// 两个时间戳字段故意用 string「遮蔽」model.Provider 里的同名 time.Time 字段。
// 时间戳由服务端管理，客户端本不该传，但真传了（哪怕只是空串）也不该让整个请求
// 解析失败 —— 直接解到 time.Time 上时，"" 会触发
// `parsing time "" as "2006-01-02T15:04:05Z07:00"` 并让请求整体 400。
// 外层字段的嵌套层级比内嵌结构体的字段浅，encoding/json 会优先选择外层字段。
type providerPayload struct {
	model.Provider
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var payload providerPayload
	if !s.decode(w, r, &payload) {
		return
	}
	p := payload.Provider
	p.ID = 0
	p.ApplyDefaults()
	created, err := s.store.CreateProvider(p)
	if err != nil {
		s.fail(w, err)
		return
	}
	// 新建供应商可能触发生效供应商变化（第一个供应商会自动生效）。
	if updated, err := s.store.GetProvider(created.ID); err == nil {
		created = updated
	}
	s.hub.Broadcast("providers", nil)
	s.writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var payload providerPayload
	if !s.decode(w, r, &payload) {
		return
	}
	p := payload.Provider
	p.ID = id
	updated, err := s.store.UpdateProvider(p)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.hub.Broadcast("providers", nil)
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteProvider(id); err != nil {
		s.fail(w, err)
		return
	}
	s.hub.Broadcast("providers", nil)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleActivateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.ActivateProvider(id); err != nil {
		s.fail(w, err)
		return
	}
	s.hub.Broadcast("providers", nil)
	list, err := s.store.ListProviders()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleReorderProviders(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if err := s.store.ReorderProviders(body.IDs); err != nil {
		s.fail(w, err)
		return
	}
	s.hub.Broadcast("providers", nil)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	prov, err := s.store.GetProvider(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	// 允许前端临时传入「待保存」的配置，这样用户不用先保存再测试。
	var override *model.Provider
	var body struct {
		model.Provider
		ProbeModel string `json:"probeModel"`
	}
	if s.decodeOptional(w, r, &body) && body.BaseURL != "" {
		candidate := body.Provider
		candidate.ID = prov.ID
		candidate.ApplyDefaults()
		override = &candidate
	}
	target := prov
	if override != nil {
		target = *override
	}

	settings, _ := s.store.GetSettings()
	// 测试请求要有独立的超时，不能沿用生成请求那种长超时。
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res := s.proxy.ProbeProvider(ctx, target, body.ProbeModel, settings)
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	prov, err := s.store.GetProvider(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	settings, _ := s.store.GetSettings()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	models, err := s.proxy.ListUpstreamModels(ctx, prov, settings)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) handleKeyHealth(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	prov, err := s.store.GetProvider(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.proxy.KeyHealth(prov))
}

// ---------- 日志 ----------

func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	logs, total, err := s.store.ListLogs(q)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"items":    logs,
		"total":    total,
		"page":     q.Page,
		"pageSize": q.PageSize,
	})
}

func (s *Server) handleGetLog(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	entry, err := s.store.GetLog(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleDeleteLogs(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.store.DeleteLogs(q)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.Broadcast("logs-cleared", map[string]any{"deleted": n})
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	// 清空是不可逆操作，要求显式确认，避免误点。
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !s.decode(w, r, &body) || !body.Confirm {
		s.writeError(w, http.StatusBadRequest, "请显式确认后再清空日志（confirm=true）")
		return
	}
	n, err := s.store.ClearLogs()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.hub.Broadcast("logs-cleared", map[string]any{"deleted": n})
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

func (s *Server) handleDistinctModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.store.DistinctModels(300)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, models)
}

// parseLogQuery 解析日志筛选与分页参数。
func parseLogQuery(r *http.Request) (model.LogQuery, error) {
	qv := r.URL.Query()
	q := model.LogQuery{
		Page:     atoiDefault(qv.Get("page"), 1),
		PageSize: atoiDefault(qv.Get("pageSize"), 50),
		Model:    qv.Get("model"),
		Status:   qv.Get("status"),
		Keyword:  qv.Get("q"),
	}
	if v := qv.Get("providerId"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return q, fmt.Errorf("providerId 不是合法数字: %s", v)
		}
		q.ProviderID = id
	}
	from, to, err := parseRange(qv.Get("range"), qv.Get("from"), qv.Get("to"))
	if err != nil {
		return q, err
	}
	q.From, q.To = from, to
	return q, nil
}

// ---------- 统计 ----------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.rangeFromRequest(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	st, err := s.store.Overview(from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	// 顺带把当前生效供应商和进行中请求数带上，仪表盘一次请求拿全。
	resp := map[string]any{
		"stats":     st,
		"liveCount": s.proxy.Live().Count(),
		"range":     map[string]any{"from": from, "to": to},
	}
	if prov, err := s.store.GetActiveProvider(); err == nil {
		resp["activeProvider"] = prov
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTimeSeries(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.rangeFromRequest(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	bucket := store.Bucket(r.URL.Query().Get("bucket"))
	if bucket == "" {
		bucket = autoBucket(from, to)
	}
	points, err := s.store.TimeSeries(from, to, bucket)
	if err != nil {
		s.fail(w, err)
		return
	}
	// 一并返回区间端点：前端据此补齐没有请求的时间桶，
	// 让趋势图的横轴始终覆盖所选的整个时间范围。
	s.writeJSON(w, http.StatusOK, map[string]any{
		"bucket": bucket,
		"from":   from,
		"to":     to,
		"points": points,
	})
}

func (s *Server) handleByModel(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.rangeFromRequest(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.store.ByModel(from, to, atoiDefault(r.URL.Query().Get("limit"), 20))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleByProvider(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.rangeFromRequest(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.store.ByProvider(from, to, atoiDefault(r.URL.Query().Get("limit"), 20))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.proxy.Live().Snapshot())
}

// ---------- 实时事件 ----------

// handleEvents 通过 SSE 把新日志与进行中请求推给管理界面。
//
// 管理界面是唯一消费者，因此不在这里做压缩与缓冲，直接小批量即时推送。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "当前服务端不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	// 进行中请求是高频变化的状态，用固定节奏推送；日志则收到即推。
	liveTicker := time.NewTicker(700 * time.Millisecond)
	defer liveTicker.Stop()

	// 初始快照，让页面一打开就有内容。
	s.writeSSE(w, "live", s.proxy.Live().Snapshot())
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			// payload 已经是 {"type":...,"data":...} 的完整 JSON。
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		case <-liveTicker.C:
			s.writeSSE(w, "live", s.proxy.Live().Snapshot())
			flusher.Flush()
		}
	}
}

func (s *Server) writeSSE(w http.ResponseWriter, eventType string, data any) {
	payload, err := json.Marshal(hub.Event{Type: eventType, Data: data})
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", payload)
}

// ---------- 工具 ----------

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("写响应失败", "err", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]any{"error": msg})
}

// fail 把 store 层的错误翻译成合适的 HTTP 状态码。
//
// 短名重复、短名为空这类都是调用方的问题，报成 500 会误导排查方向
// （日志里看着像服务端故障，实际改个名字就好）。
func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrDuplicateName):
		status = http.StatusConflict
	case errors.Is(err, store.ErrEmptyName):
		status = http.StatusBadRequest
	}
	if status >= 500 {
		s.logger.Error("管理接口内部错误", "err", err)
	} else {
		s.logger.Warn("管理接口拒绝请求", "err", err)
	}
	s.writeError(w, status, err.Error())
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		s.writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

// decodeOptional 允许空请求体，用于「带可选参数的动作类接口」。
func (s *Server) decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		// 可选参数解析失败不阻断主流程。
		return false
	}
	return true
}

func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "路径参数 id 不是合法数字")
		return 0, false
	}
	return id, true
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// parseRange 把 range 简写或显式的 from/to 解析成时间区间。
//
// range 支持 1h / 6h / 24h / 7d / 30d / all；不给则默认最近 24 小时。
func parseRange(rangeSpec, fromSpec, toSpec string) (time.Time, time.Time, error) {
	now := time.Now()

	if fromSpec != "" || toSpec != "" {
		var from, to time.Time
		if fromSpec != "" {
			t, err := time.Parse(time.RFC3339, fromSpec)
			if err != nil {
				return from, to, fmt.Errorf("from 参数格式应为 RFC3339: %w", err)
			}
			from = t
		}
		if toSpec != "" {
			t, err := time.Parse(time.RFC3339, toSpec)
			if err != nil {
				return from, to, fmt.Errorf("to 参数格式应为 RFC3339: %w", err)
			}
			to = t
		}
		return from, to, nil
	}

	switch rangeSpec {
	case "", "24h":
		return now.Add(-24 * time.Hour), now, nil
	case "all":
		return time.Time{}, now, nil
	case "1h":
		return now.Add(-time.Hour), now, nil
	case "6h":
		return now.Add(-6 * time.Hour), now, nil
	case "7d":
		return now.AddDate(0, 0, -7), now, nil
	case "30d":
		return now.AddDate(0, 0, -30), now, nil
	}

	// 其余情况按 Go 的 duration 语法解析，例如 90m、48h。
	if d, err := time.ParseDuration(rangeSpec); err == nil && d > 0 {
		return now.Add(-d), now, nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("无法识别的 range 参数: %q", rangeSpec)
}

func (s *Server) rangeFromRequest(r *http.Request) (time.Time, time.Time, error) {
	qv := r.URL.Query()
	return parseRange(qv.Get("range"), qv.Get("from"), qv.Get("to"))
}

// autoBucket 按时间跨度自动选择分桶粒度，让趋势图点数保持在可读范围。
func autoBucket(from, to time.Time) store.Bucket {
	if from.IsZero() {
		return store.BucketDay
	}
	span := to.Sub(from)
	switch {
	case span <= 6*time.Hour:
		return store.BucketMinute
	case span <= 72*time.Hour:
		return store.BucketHour
	default:
		return store.BucketDay
	}
}
