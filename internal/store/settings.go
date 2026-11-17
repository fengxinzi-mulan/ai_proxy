package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ai_proxy/internal/model"
)

const settingsKey = "global"

// GetSettings 读取全局设置。首次运行时返回默认值并落库。
func (s *Store) GetSettings() (model.Settings, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		def := model.DefaultSettings()
		if err := s.SaveSettings(def); err != nil {
			return def, err
		}
		return def, nil
	}
	if err != nil {
		return model.Settings{}, fmt.Errorf("读取设置失败: %w", err)
	}

	// 从旧版本升级时可能缺少新增字段，先灌入默认值再反序列化，
	// 这样缺字段的 JSON 会自然回落到默认值。
	st := model.DefaultSettings()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return model.DefaultSettings(), fmt.Errorf("解析设置失败: %w", err)
		}
	}
	st.ApplyDefaults()
	return st, nil
}

// SaveSettings 覆盖写入全局设置。
func (s *Store) SaveSettings(st model.Settings) error {
	st.ApplyDefaults()
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("序列化设置失败: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingsKey, string(raw),
	)
	if err != nil {
		return fmt.Errorf("保存设置失败: %w", err)
	}
	return nil
}

// PatchSettings 以回调方式做读改写，避免并发更新互相覆盖。
func (s *Store) PatchSettings(fn func(*model.Settings) error) (model.Settings, error) {
	st, err := s.GetSettings()
	if err != nil {
		return st, err
	}
	if err := fn(&st); err != nil {
		return st, err
	}
	if err := s.SaveSettings(st); err != nil {
		return st, err
	}
	return st, nil
}

// GetKV 读取一段与全局设置无关的键值状态（例如「已跳过的更新版本」）。
//
// 为什么不塞进 model.Settings：前端保存设置时是整份 PUT 覆盖，
// 把这类「运行态小状态」混在里面，迟早被一次普通的保存抹掉。
// 复用 settings 表而不是新建一张表 —— 它本来就是 key/value 结构。
func (s *Store) GetKV(key string) (string, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", key, err)
	}
	return raw, nil
}

// PutKV 覆盖写入一段键值状态。
func (s *Store) PutKV(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("保存 %s 失败: %w", key, err)
	}
	return nil
}
