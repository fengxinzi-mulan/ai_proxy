package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ai_proxy/internal/model"
)

// 用量快照的存取。
//
// 这里只负责「最近一次查到的结果」，不做任何历史保留：用量是随时可重新查的
// 运行态数据，留历史既没用又会一直涨。每个供应商一行，下次刷新直接覆盖。

// SaveProviderUsage 写入（或覆盖）一个供应商的用量快照。
func (s *Store) SaveProviderUsage(snap model.UsageSnapshot) error {
	payload, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("序列化用量快照失败: %w", err)
	}
	fetchedAt := snap.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}
	_, err = s.db.Exec(`INSERT INTO provider_usage (provider_id, fetched_at, payload_json)
		VALUES (?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET fetched_at = excluded.fetched_at, payload_json = excluded.payload_json`,
		snap.ProviderID, tsToDB(fetchedAt), string(payload))
	if err != nil {
		return fmt.Errorf("保存用量快照失败: %w", err)
	}
	return nil
}

// GetProviderUsage 读取一个供应商的用量快照。没有查过时返回 nil，不视为错误。
func (s *Store) GetProviderUsage(providerID int64) (*model.UsageSnapshot, error) {
	row := s.db.QueryRow(
		`SELECT payload_json FROM provider_usage WHERE provider_id = ?`, providerID)
	snap, err := scanUsageSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return snap, err
}

// ListProviderUsage 读取全部供应商的用量快照，按供应商 id 索引。
// 供管理接口一次性把快照随列表下发，省掉「列表 + N 次查询」的往返。
func (s *Store) ListProviderUsage() (map[int64]model.UsageSnapshot, error) {
	rows, err := s.db.Query(`SELECT payload_json FROM provider_usage`)
	if err != nil {
		return nil, fmt.Errorf("查询用量快照失败: %w", err)
	}
	defer rows.Close()

	out := map[int64]model.UsageSnapshot{}
	for rows.Next() {
		snap, err := scanUsageSnapshot(rows)
		if err != nil {
			return nil, err
		}
		if snap == nil {
			continue
		}
		out[snap.ProviderID] = *snap
	}
	return out, rows.Err()
}

// DeleteProviderUsage 删除一个供应商的用量快照。
func (s *Store) DeleteProviderUsage(providerID int64) error {
	if _, err := s.db.Exec(`DELETE FROM provider_usage WHERE provider_id = ?`, providerID); err != nil {
		return fmt.Errorf("删除用量快照失败: %w", err)
	}
	return nil
}

type usageScanner interface {
	Scan(dest ...any) error
}

func scanUsageSnapshot(sc usageScanner) (*model.UsageSnapshot, error) {
	var payload string
	if err := sc.Scan(&payload); err != nil {
		return nil, err
	}
	var snap model.UsageSnapshot
	if payload == "" {
		return nil, nil
	}
	// 坏掉的 JSON 不该让列表读不出来：这种情况当作「没有快照」，
	// 下一次定时刷新就会把它覆盖掉。
	if err := json.Unmarshal([]byte(payload), &snap); err != nil {
		return nil, nil
	}
	return &snap, nil
}
