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

// ErrNotFound 表示目标记录不存在。
var ErrNotFound = errors.New("记录不存在")

// 供应商相关的可辨识错误。上层据此映射成合适的 HTTP 状态码 ——
// 「短名重复」是客户端错误，不该报成 500。
var (
	ErrDuplicateName = errors.New("供应商短名已存在")
	ErrEmptyName     = errors.New("供应商短名不能为空")
)

const providerColumns = `id, name, display_name, remark, enabled, active, sort_order,
	base_url, api_format, custom_path, auth_header, auth_prefix,
	keys_json, extra_headers_json,
	timeout_seconds, connect_timeout_seconds, insecure_skip_tls,
	proxy_mode, proxy_json, prompt_mode, prompt_json, prompt_rules_json,
	usage_inject_mode, strip_usage_chunk, custom_usage_json, tags_json,
	created_at, updated_at`

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ListProviders 返回全部供应商，按 sort_order 升序。
func (s *Store) ListProviders() ([]model.Provider, error) {
	rows, err := s.db.Query(`SELECT ` + providerColumns + ` FROM providers ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询供应商失败: %w", err)
	}
	defer rows.Close()

	out := []model.Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider 按 ID 读取供应商。
func (s *Store) GetProvider(id int64) (model.Provider, error) {
	row := s.db.QueryRow(`SELECT `+providerColumns+` FROM providers WHERE id = ?`, id)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// GetProviderByName 按唯一短名读取供应商，供 /p/<name>/ 路由使用。
func (s *Store) GetProviderByName(name string) (model.Provider, error) {
	row := s.db.QueryRow(`SELECT `+providerColumns+` FROM providers WHERE name = ? COLLATE NOCASE`, name)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// GetActiveProvider 返回当前生效的供应商。
// 优先取标记为 active 的；若没有（例如刚删掉），回退到第一个启用的供应商。
func (s *Store) GetActiveProvider() (model.Provider, error) {
	row := s.db.QueryRow(`SELECT ` + providerColumns +
		` FROM providers WHERE active = 1 AND enabled = 1 ORDER BY sort_order ASC, id ASC LIMIT 1`)
	p, err := scanProvider(row)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	row = s.db.QueryRow(`SELECT ` + providerColumns +
		` FROM providers WHERE enabled = 1 ORDER BY sort_order ASC, id ASC LIMIT 1`)
	p, err = scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// CreateProvider 插入新供应商并返回带 ID 的完整对象。
func (s *Store) CreateProvider(p model.Provider) (model.Provider, error) {
	p.ApplyDefaults()
	now := time.Now()
	p.CreatedAt, p.UpdatedAt = now, now
	if p.Name == "" {
		return p, ErrEmptyName
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Name
	}

	keysJSON, headersJSON, rulesJSON, tagsJSON, proxyJSON, promptJSON, customUsageJSON, err := marshalProviderParts(p)
	if err != nil {
		return p, err
	}

	res, err := s.db.Exec(`INSERT INTO providers (
		name, display_name, remark, enabled, active, sort_order,
		base_url, api_format, custom_path, auth_header, auth_prefix,
		keys_json, extra_headers_json,
		timeout_seconds, connect_timeout_seconds, insecure_skip_tls,
		proxy_mode, proxy_json, prompt_mode, prompt_json, prompt_rules_json,
		usage_inject_mode, strip_usage_chunk, custom_usage_json, tags_json,
		created_at, updated_at
	) VALUES (?,?,?,?,?,?, ?,?,?,?,?, ?,?, ?,?,?, ?,?, ?,?,?, ?,?,?,?, ?,?)`,
		p.Name, p.DisplayName, p.Remark, boolToInt(p.Enabled), boolToInt(p.Active), p.SortOrder,
		p.BaseURL, string(p.APIFormat), p.CustomPath, p.AuthHeader, p.AuthPrefix,
		keysJSON, headersJSON,
		p.TimeoutSeconds, p.ConnectTimeoutSeconds, boolToInt(p.InsecureSkipTLS),
		p.ProxyMode, proxyJSON, p.PromptMode, promptJSON, rulesJSON,
		p.UsageInjectMode, boolToInt(p.StripUsageChunk), customUsageJSON, tagsJSON,
		tsToDB(p.CreatedAt), tsToDB(p.UpdatedAt),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return p, fmt.Errorf("%w: %q", ErrDuplicateName, p.Name)
		}
		return p, fmt.Errorf("创建供应商失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return p, err
	}
	p.ID = id

	// 第一个创建的供应商自动成为「当前生效」。
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM providers`).Scan(&count); err == nil && count == 1 {
		if err := s.ActivateProvider(id); err != nil {
			return p, err
		}
		p.Active = true
	}
	return p, nil
}

// UpdateProvider 全量覆盖供应商配置。
func (s *Store) UpdateProvider(p model.Provider) (model.Provider, error) {
	p.ApplyDefaults()
	existing, err := s.GetProvider(p.ID)
	if err != nil {
		return p, err
	}
	// 短名与启用状态允许改，但 active 由 ActivateProvider 单独维护，
	// 避免前端一次普通编辑就把生效供应商改掉。
	p.Active = existing.Active
	p.CreatedAt = existing.CreatedAt
	p.UpdatedAt = time.Now()

	keysJSON, headersJSON, rulesJSON, tagsJSON, proxyJSON, promptJSON, customUsageJSON, err := marshalProviderParts(p)
	if err != nil {
		return p, err
	}

	_, err = s.db.Exec(`UPDATE providers SET
		name=?, display_name=?, remark=?, enabled=?, sort_order=?,
		base_url=?, api_format=?, custom_path=?, auth_header=?, auth_prefix=?,
		keys_json=?, extra_headers_json=?,
		timeout_seconds=?, connect_timeout_seconds=?, insecure_skip_tls=?,
		proxy_mode=?, proxy_json=?, prompt_mode=?, prompt_json=?, prompt_rules_json=?,
		usage_inject_mode=?, strip_usage_chunk=?, custom_usage_json=?, tags_json=?,
		updated_at=?
		WHERE id=?`,
		p.Name, p.DisplayName, p.Remark, boolToInt(p.Enabled), p.SortOrder,
		p.BaseURL, string(p.APIFormat), p.CustomPath, p.AuthHeader, p.AuthPrefix,
		keysJSON, headersJSON,
		p.TimeoutSeconds, p.ConnectTimeoutSeconds, boolToInt(p.InsecureSkipTLS),
		p.ProxyMode, proxyJSON, p.PromptMode, promptJSON, rulesJSON,
		p.UsageInjectMode, boolToInt(p.StripUsageChunk), customUsageJSON, tagsJSON,
		tsToDB(p.UpdatedAt), p.ID,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return p, fmt.Errorf("%w: %q", ErrDuplicateName, p.Name)
		}
		return p, fmt.Errorf("更新供应商失败: %w", err)
	}
	return s.GetProvider(p.ID)
}

// DeleteProvider 删除供应商，同时清掉它的密钥状态。
func (s *Store) DeleteProvider(id int64) error {
	res, err := s.db.Exec(`DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除供应商失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	// 删掉的正好是当前生效供应商时，自动把生效位交给第一个启用的供应商。
	var activeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM providers WHERE active = 1 AND enabled = 1`).Scan(&activeCount); err == nil && activeCount == 0 {
		var nextID int64
		if err := s.db.QueryRow(
			`SELECT id FROM providers WHERE enabled = 1 ORDER BY sort_order ASC, id ASC LIMIT 1`).Scan(&nextID); err == nil {
			_ = s.ActivateProvider(nextID)
		}
	}
	return nil
}

// ActivateProvider 把指定供应商设为当前生效，并清除其它供应商的生效位。
func (s *Store) ActivateProvider(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE providers SET active = 0 WHERE active = 1`); err != nil {
		return fmt.Errorf("清除生效标记失败: %w", err)
	}
	res, err := tx.Exec(`UPDATE providers SET active = 1, updated_at = ? WHERE id = ?`,
		tsToDB(time.Now()), id)
	if err != nil {
		return fmt.Errorf("设置生效标记失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ReorderProviders 按传入的 ID 顺序重写 sort_order。
func (s *Store) ReorderProviders(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE providers SET sort_order = ? WHERE id = ?`, i, id); err != nil {
			return fmt.Errorf("更新排序失败: %w", err)
		}
	}
	return tx.Commit()
}

// ---------- 序列化辅助 ----------

func marshalProviderParts(p model.Provider) (keys, headers, rules, tags, proxy, prompt, customUsage string, err error) {
	marshal := func(v any, what string) (string, error) {
		b, e := json.Marshal(v)
		if e != nil {
			return "", fmt.Errorf("序列化 %s 失败: %w", what, e)
		}
		return string(b), nil
	}
	if keys, err = marshal(p.Keys, "密钥"); err != nil {
		return
	}
	if headers, err = marshal(p.ExtraHeaders, "额外请求头"); err != nil {
		return
	}
	if rules, err = marshal(p.PromptRules, "提示词规则"); err != nil {
		return
	}
	if tags, err = marshal(p.Tags, "标签"); err != nil {
		return
	}
	if proxy, err = marshal(p.Proxy, "代理配置"); err != nil {
		return
	}
	if prompt, err = marshal(p.Prompt, "提示词配置"); err != nil {
		return
	}
	if p.CustomUsage == nil {
		customUsage = ""
	} else if customUsage, err = marshal(p.CustomUsage, "自定义用量映射"); err != nil {
		return
	}
	return
}

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的公共部分。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanProvider(sc rowScanner) (model.Provider, error) {
	var (
		p                                        model.Provider
		enabled, active, insecureTLS, stripUsage int
		keysJSON, headersJSON, rulesJSON         string
		tagsJSON, proxyJSON, promptJSON          string
		customUsageJSON                          string
		apiFormat                                string
		createdAt, updatedAt                     string
	)
	err := sc.Scan(
		&p.ID, &p.Name, &p.DisplayName, &p.Remark, &enabled, &active, &p.SortOrder,
		&p.BaseURL, &apiFormat, &p.CustomPath, &p.AuthHeader, &p.AuthPrefix,
		&keysJSON, &headersJSON,
		&p.TimeoutSeconds, &p.ConnectTimeoutSeconds, &insecureTLS,
		&p.ProxyMode, &proxyJSON, &p.PromptMode, &promptJSON, &rulesJSON,
		&p.UsageInjectMode, &stripUsage, &customUsageJSON, &tagsJSON,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, err
		}
		return p, fmt.Errorf("读取供应商失败: %w", err)
	}

	p.Enabled = enabled == 1
	p.Active = active == 1
	p.InsecureSkipTLS = insecureTLS == 1
	p.StripUsageChunk = stripUsage == 1
	p.APIFormat = model.APIFormat(apiFormat)

	// 任何一段 JSON 坏掉都不应让整个列表读不出来，因此逐段容错。
	decode := func(raw string, dst any) {
		if raw == "" {
			return
		}
		_ = json.Unmarshal([]byte(raw), dst)
	}
	decode(keysJSON, &p.Keys)
	decode(headersJSON, &p.ExtraHeaders)
	decode(rulesJSON, &p.PromptRules)
	decode(tagsJSON, &p.Tags)
	decode(proxyJSON, &p.Proxy)
	decode(promptJSON, &p.Prompt)
	if customUsageJSON != "" {
		var cu model.CustomUsageMapping
		decode(customUsageJSON, &cu)
		p.CustomUsage = &cu
	}

	p.CreatedAt = tsFromDB(createdAt)
	p.UpdatedAt = tsFromDB(updatedAt)
	p.ApplyDefaults()
	return p, nil
}

// ---------- 路径与短名 ----------

// NormalizeProviderName 把用户输入的短名规整为可用于 URL 的形式。
func NormalizeProviderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
