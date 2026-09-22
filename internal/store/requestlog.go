package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai_proxy/internal/model"
)

const logColumns = `id, ts_start, ts_end, provider_id, provider_name, upstream_url, proxy_used,
	method, path, model, model_response, api_format, stream, reasoning_effort, client_ip,
	request_modified, response_modified, modifications,
	http_status, success, error_msg, cancelled,
	prompt_tokens, cached_tokens, cache_write_tokens, completion_tokens, reasoning_tokens,
	total_tokens, tokens_estimated, ttft_ms, total_ms, tps,
	req_headers, req_body, req_body_original, resp_body`

// tsToDB / tsFromDB 定义在 db.go，全 store 包共用同一套时间格式。

// InsertLog 写入一条请求日志，返回自增 ID。
func (s *Store) InsertLog(l *model.RequestLog) (int64, error) {
	mods, err := json.Marshal(l.Modifications)
	if err != nil {
		mods = []byte("[]")
	}
	res, err := s.db.Exec(`INSERT INTO request_logs (
		ts_start, ts_end, provider_id, provider_name, upstream_url, proxy_used,
		method, path, model, model_response, api_format, stream, reasoning_effort, client_ip,
		request_modified, response_modified, modifications,
		http_status, success, error_msg, cancelled,
		prompt_tokens, cached_tokens, cache_write_tokens, completion_tokens, reasoning_tokens,
		total_tokens, tokens_estimated, ttft_ms, total_ms, tps,
		req_headers, req_body, req_body_original, resp_body
	) VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?,?,?)`,
		tsToDB(l.TSStart), tsToDB(l.TSEnd), l.ProviderID, l.ProviderName, l.UpstreamURL, l.ProxyUsed,
		l.Method, l.Path, l.Model, l.ModelResponse, l.APIFormat, boolToInt(l.Stream), l.ReasoningEffort, l.ClientIP,
		boolToInt(l.RequestModified), boolToInt(l.ResponseModified), string(mods),
		l.HTTPStatus, boolToInt(l.Success), l.ErrorMsg, boolToInt(l.Cancelled),
		l.PromptTokens, l.CachedTokens, l.CacheWriteTokens, l.CompletionTokens, l.ReasoningTokens,
		l.TotalTokens, boolToInt(l.TokensEstimated), l.TTFTMs, l.TotalMs, l.TPS,
		l.ReqHeaders, l.ReqBody, l.ReqBodyOriginal, l.RespBody,
	)
	if err != nil {
		return 0, fmt.Errorf("写入请求日志失败: %w", err)
	}
	return res.LastInsertId()
}

// ListLogs 按条件分页查询日志（不含报文正文，避免列表接口过大）。
func (s *Store) ListLogs(q model.LogQuery) ([]model.RequestLog, int64, error) {
	if q.PageSize <= 0 {
		q.PageSize = 50
	}
	if q.PageSize > 500 {
		q.PageSize = 500
	}
	if q.Page < 1 {
		q.Page = 1
	}

	where, args := buildLogFilter(q)

	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计日志条数失败: %w", err)
	}

	// 列表不返回报文正文，正文只在详情接口按需读取。
	listCols := `id, ts_start, ts_end, provider_id, provider_name, upstream_url, proxy_used,
		method, path, model, model_response, api_format, stream, reasoning_effort, client_ip,
		request_modified, response_modified, modifications,
		http_status, success, error_msg, cancelled,
		prompt_tokens, cached_tokens, cache_write_tokens, completion_tokens, reasoning_tokens,
		total_tokens, tokens_estimated, ttft_ms, total_ms, tps,
		'', '', '', ''`

	args = append(args, q.PageSize, (q.Page-1)*q.PageSize)
	rows, err := s.db.Query(
		`SELECT `+listCols+` FROM request_logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询日志失败: %w", err)
	}
	defer rows.Close()

	out := []model.RequestLog{}
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// GetLog 读取单条日志，包含完整报文。
func (s *Store) GetLog(id int64) (model.RequestLog, error) {
	row := s.db.QueryRow(`SELECT `+logColumns+` FROM request_logs WHERE id = ?`, id)
	l, err := scanLog(row)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// DeleteLogs 按条件删除日志；条件为空时不删除任何东西，防止误清空。
func (s *Store) DeleteLogs(q model.LogQuery) (int64, error) {
	where, args := buildLogFilter(q)
	if where == "" {
		return 0, errors.New("缺少删除条件，已拒绝清空全部日志")
	}
	res, err := s.db.Exec(`DELETE FROM request_logs`+where, args...)
	if err != nil {
		return 0, fmt.Errorf("删除日志失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearLogs 清空全部日志（仅供用户在前端明确确认后调用）。
func (s *Store) ClearLogs() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM request_logs`)
	if err != nil {
		return 0, fmt.Errorf("清空日志失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CleanupLogs 按保留策略清理历史日志，返回删除条数。
func (s *Store) CleanupLogs(retentionDays, maxLogs int) (int64, error) {
	var deleted int64
	if retentionDays > 0 {
		cutoff := tsToDB(time.Now().AddDate(0, 0, -retentionDays))
		res, err := s.db.Exec(`DELETE FROM request_logs WHERE ts_start < ?`, cutoff)
		if err != nil {
			return deleted, fmt.Errorf("按时间清理日志失败: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if maxLogs > 0 {
		// 保留 id 最大的 maxLogs 条。
		var boundary sql.NullInt64
		if err := s.db.QueryRow(
			`SELECT id FROM request_logs ORDER BY id DESC LIMIT 1 OFFSET ?`, maxLogs-1,
		).Scan(&boundary); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return deleted, nil // 条数未超限
			}
			return deleted, fmt.Errorf("定位日志边界失败: %w", err)
		}
		res, err := s.db.Exec(`DELETE FROM request_logs WHERE id < ?`, boundary.Int64)
		if err != nil {
			return deleted, fmt.Errorf("按条数清理日志失败: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

// buildLogFilter 把查询条件翻译成 WHERE 子句。
func buildLogFilter(q model.LogQuery) (string, []any) {
	var conds []string
	var args []any

	if q.ProviderID > 0 {
		conds = append(conds, "provider_id = ?")
		args = append(args, q.ProviderID)
	}
	if q.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, q.Model)
	}
	switch q.Status {
	case "success":
		conds = append(conds, "success = 1")
	case "failed":
		conds = append(conds, "success = 0 AND cancelled = 0")
	case "cancelled":
		conds = append(conds, "cancelled = 1")
	}
	if !q.From.IsZero() {
		conds = append(conds, "ts_start >= ?")
		args = append(args, tsToDB(q.From))
	}
	if !q.To.IsZero() {
		conds = append(conds, "ts_start <= ?")
		args = append(args, tsToDB(q.To))
	}
	if q.Keyword != "" {
		// 关键字只搜轻量列，避免全表扫描大报文。
		conds = append(conds,
			"(model LIKE ? OR model_response LIKE ? OR path LIKE ? OR error_msg LIKE ? OR provider_name LIKE ?)")
		kw := "%" + q.Keyword + "%"
		args = append(args, kw, kw, kw, kw, kw)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func scanLog(sc rowScanner) (model.RequestLog, error) {
	var (
		l                             model.RequestLog
		tsStart, tsEnd                string
		stream, reqMod, respMod       int
		success, cancelled, estimated int
		mods                          string
	)
	err := sc.Scan(
		&l.ID, &tsStart, &tsEnd, &l.ProviderID, &l.ProviderName, &l.UpstreamURL, &l.ProxyUsed,
		&l.Method, &l.Path, &l.Model, &l.ModelResponse, &l.APIFormat, &stream, &l.ReasoningEffort, &l.ClientIP,
		&reqMod, &respMod, &mods,
		&l.HTTPStatus, &success, &l.ErrorMsg, &cancelled,
		&l.PromptTokens, &l.CachedTokens, &l.CacheWriteTokens, &l.CompletionTokens, &l.ReasoningTokens,
		&l.TotalTokens, &estimated, &l.TTFTMs, &l.TotalMs, &l.TPS,
		&l.ReqHeaders, &l.ReqBody, &l.ReqBodyOriginal, &l.RespBody,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return l, err
		}
		return l, fmt.Errorf("读取日志失败: %w", err)
	}
	l.TSStart = tsFromDB(tsStart)
	l.TSEnd = tsFromDB(tsEnd)
	l.Stream = stream == 1
	l.RequestModified = reqMod == 1
	l.ResponseModified = respMod == 1
	l.Success = success == 1
	l.Cancelled = cancelled == 1
	l.TokensEstimated = estimated == 1
	l.Modifications = []string{}
	if mods != "" {
		_ = json.Unmarshal([]byte(mods), &l.Modifications)
	}
	if l.Modifications == nil {
		l.Modifications = []string{}
	}
	return l, nil
}
