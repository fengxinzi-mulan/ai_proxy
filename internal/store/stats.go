package store

import (
	"fmt"
	"time"

	"ai_proxy/internal/model"
)

// 聚合查询共用的「时间段」参数经 store 内部统一转换，
// 调用方只需传零值时间表示不限制。

func rangeClause(from, to time.Time) (string, []any) {
	where := " WHERE 1=1"
	args := []any{}
	if !from.IsZero() {
		where += " AND ts_start >= ?"
		args = append(args, tsToDB(from))
	}
	if !to.IsZero() {
		where += " AND ts_start <= ?"
		args = append(args, tsToDB(to))
	}
	return where, args
}

// Overview 返回指标卡所需的聚合数据。
func (s *Store) Overview(from, to time.Time) (model.OverviewStats, error) {
	where, args := rangeClause(from, to)
	row := s.db.QueryRow(`SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN success = 0 AND cancelled = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END), 0),
			COALESCE(AVG(CASE WHEN tps > 0 THEN tps END), 0),
			COALESCE(AVG(CASE WHEN total_ms > 0 THEN total_ms END), 0)
		FROM request_logs`+where, args...)

	var st model.OverviewStats
	if err := row.Scan(
		&st.TotalRequests, &st.SuccessRequests, &st.FailedRequests,
		&st.PromptTokens, &st.CachedTokens, &st.CompletionTokens, &st.TotalTokens,
		&st.AvgTTFTMs, &st.AvgTPS, &st.AvgDurationMs,
	); err != nil {
		return st, fmt.Errorf("聚合总览失败: %w", err)
	}
	if st.TotalRequests > 0 {
		st.SuccessRate = float64(st.SuccessRequests) / float64(st.TotalRequests) * 100
	}
	st.CacheHitRate = model.CacheHitRate(st.CachedTokens, st.PromptTokens)
	return st, nil
}

// Bucket 是趋势图的时间分桶粒度。
type Bucket string

const (
	BucketMinute Bucket = "minute"
	BucketHour   Bucket = "hour"
	BucketDay    Bucket = "day"
)

// format 返回该粒度对应的 strftime 格式，输出可直接被前端解析。
func (b Bucket) format() string {
	switch b {
	case BucketMinute:
		return "%Y-%m-%dT%H:%M:00Z"
	case BucketDay:
		return "%Y-%m-%d"
	default:
		return "%Y-%m-%dT%H:00Z"
	}
}

// TimeSeries 返回按时间分桶的请求量、token 与性能趋势。
func (s *Store) TimeSeries(from, to time.Time, bucket Bucket) ([]model.TimeSeriesPoint, error) {
	where, args := rangeClause(from, to)
	// 分桶格式作为绑定参数传入，SQLite 允许 strftime 的格式参数是表达式。
	q := `SELECT
			strftime(?, ts_start) AS bucket,
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 0 AND cancelled = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END), 0),
			COALESCE(AVG(CASE WHEN tps > 0 THEN tps END), 0)
		FROM request_logs` + where + `
		GROUP BY bucket ORDER BY bucket ASC`

	rows, err := s.db.Query(q, append([]any{bucket.format()}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("聚合趋势失败: %w", err)
	}
	defer rows.Close()

	out := []model.TimeSeriesPoint{}
	for rows.Next() {
		var pt model.TimeSeriesPoint
		if err := rows.Scan(&pt.Bucket, &pt.Requests, &pt.Failed,
			&pt.PromptTokens, &pt.CachedTokens, &pt.CompletionTokens,
			&pt.AvgTTFTMs, &pt.AvgTPS); err != nil {
			return nil, fmt.Errorf("读取趋势数据失败: %w", err)
		}
		pt.CacheHitRate = model.CacheHitRate(pt.CachedTokens, pt.PromptTokens)
		out = append(out, pt)
	}
	return out, rows.Err()
}

// ByModel 返回模型维度排行，按请求数降序。
func (s *Store) ByModel(from, to time.Time, limit int) ([]model.ModelStatsRow, error) {
	if limit <= 0 {
		limit = 20
	}
	where, args := rangeClause(from, to)
	q := `SELECT
			model,
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 0 AND cancelled = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END), 0),
			COALESCE(AVG(CASE WHEN tps > 0 THEN tps END), 0)
		FROM request_logs` + where + ` AND model != ''
		GROUP BY model ORDER BY COUNT(*) DESC, model ASC LIMIT ?`

	rows, err := s.db.Query(q, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("聚合模型排行失败: %w", err)
	}
	defer rows.Close()

	out := []model.ModelStatsRow{}
	for rows.Next() {
		var r model.ModelStatsRow
		if err := rows.Scan(&r.Model, &r.Requests, &r.Failed,
			&r.PromptTokens, &r.CachedTokens, &r.CompletionTokens,
			&r.AvgTTFTMs, &r.AvgTPS); err != nil {
			return nil, fmt.Errorf("读取模型排行失败: %w", err)
		}
		r.CacheHitRate = model.CacheHitRate(r.CachedTokens, r.PromptTokens)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ByProvider 返回供应商维度排行，按请求数降序。
func (s *Store) ByProvider(from, to time.Time, limit int) ([]model.ProviderStatsRow, error) {
	if limit <= 0 {
		limit = 20
	}
	where, args := rangeClause(from, to)
	q := `SELECT
			provider_id,
			provider_name,
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 0 AND cancelled = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END), 0),
			COALESCE(AVG(CASE WHEN tps > 0 THEN tps END), 0)
		FROM request_logs` + where + `
		GROUP BY provider_id, provider_name ORDER BY COUNT(*) DESC LIMIT ?`

	rows, err := s.db.Query(q, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("聚合供应商排行失败: %w", err)
	}
	defer rows.Close()

	out := []model.ProviderStatsRow{}
	for rows.Next() {
		var r model.ProviderStatsRow
		if err := rows.Scan(&r.ProviderID, &r.ProviderName, &r.Requests, &r.Failed,
			&r.PromptTokens, &r.CachedTokens, &r.CompletionTokens,
			&r.AvgTTFTMs, &r.AvgTPS); err != nil {
			return nil, fmt.Errorf("读取供应商排行失败: %w", err)
		}
		r.CacheHitRate = model.CacheHitRate(r.CachedTokens, r.PromptTokens)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DistinctModels 返回日志中出现过的模型名，供前端筛选下拉框使用。
func (s *Store) DistinctModels(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT model FROM request_logs WHERE model != '' GROUP BY model ORDER BY MAX(id) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询模型列表失败: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("读取模型列表失败: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
