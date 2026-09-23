// Package store 负责 SQLite 持久化。
//
// 设计取舍：表结构很小，因此不用 ORM，直接手写 SQL + 版本化迁移，
// 让依赖保持在 modernc.org/sqlite 一个之上。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无需 CGO
)

// Store 聚合所有数据访问方法。
type Store struct {
	db *sql.DB
}

// dbTimeLayout 是所有时间列的存储格式。
//
// 选用它有两个硬性理由：
//  1. 定宽且按字典序 == 按时间序，SQL 里的字符串比较等价于时间比较；
//  2. 恰好 3 位小数 + 结尾 Z，SQLite 的 strftime() 能直接解析，
//     而 RFC3339Nano 的 9 位纳秒会被 SQLite 拒绝，导致分桶统计全部失败。
//
// 时间一律以 UTC 存储。
const dbTimeLayout = "2006-01-02T15:04:05.000Z"

// tsToDB 把时间序列化为存储格式，零值存为空串。
func tsToDB(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(dbTimeLayout)
}

// tsFromDB 解析存储格式，失败返回零值。
func tsFromDB(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(dbTimeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Open 打开（必要时创建）指定目录下的 SQLite 数据库并执行迁移。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	dsn := filepath.Join(dataDir, "ai_proxy.db") +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// modernc 的驱动是纯 Go 实现，写并发下仍建议限制连接数，避免 database is locked。
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// migration 是一次结构变更。版本号单调递增，且一旦发布不可修改。
type migration struct {
	version int
	stmts   []string
}

var migrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS providers (
				id                      INTEGER PRIMARY KEY AUTOINCREMENT,
				name                    TEXT    NOT NULL UNIQUE,
				display_name            TEXT    NOT NULL DEFAULT '',
				remark                  TEXT    NOT NULL DEFAULT '',
				enabled                 INTEGER NOT NULL DEFAULT 1,
				active                  INTEGER NOT NULL DEFAULT 0,
				sort_order              INTEGER NOT NULL DEFAULT 0,
				base_url                TEXT    NOT NULL DEFAULT '',
				api_format              TEXT    NOT NULL DEFAULT 'openai',
				custom_path             TEXT    NOT NULL DEFAULT '',
				auth_header             TEXT    NOT NULL DEFAULT 'Authorization',
				auth_prefix             TEXT    NOT NULL DEFAULT 'Bearer ',
				keys_json               TEXT    NOT NULL DEFAULT '[]',
				extra_headers_json      TEXT    NOT NULL DEFAULT '{}',
				timeout_seconds         INTEGER NOT NULL DEFAULT 300,
				connect_timeout_seconds INTEGER NOT NULL DEFAULT 15,
				insecure_skip_tls       INTEGER NOT NULL DEFAULT 0,
				proxy_mode              TEXT    NOT NULL DEFAULT 'inherit',
				proxy_json              TEXT    NOT NULL DEFAULT '{}',
				prompt_mode             TEXT    NOT NULL DEFAULT 'inherit',
				prompt_json             TEXT    NOT NULL DEFAULT '{}',
				prompt_rules_json       TEXT    NOT NULL DEFAULT '[]',
				usage_inject_mode       TEXT    NOT NULL DEFAULT 'inherit',
				strip_usage_chunk       INTEGER NOT NULL DEFAULT 0,
				custom_usage_json       TEXT    NOT NULL DEFAULT '',
				tags_json               TEXT    NOT NULL DEFAULT '[]',
				created_at              TEXT    NOT NULL,
				updated_at              TEXT    NOT NULL
			)`,

			`CREATE TABLE IF NOT EXISTS settings (
				key   TEXT PRIMARY KEY,
				value TEXT NOT NULL
			)`,

			`CREATE TABLE IF NOT EXISTS request_logs (
				id                  INTEGER PRIMARY KEY AUTOINCREMENT,
				ts_start            TEXT    NOT NULL,
				ts_end              TEXT    NOT NULL DEFAULT '',
				provider_id         INTEGER NOT NULL DEFAULT 0,
				provider_name       TEXT    NOT NULL DEFAULT '',
				upstream_url        TEXT    NOT NULL DEFAULT '',
				proxy_used          TEXT    NOT NULL DEFAULT '',
				method              TEXT    NOT NULL DEFAULT '',
				path                TEXT    NOT NULL DEFAULT '',
				model               TEXT    NOT NULL DEFAULT '',
				model_response      TEXT    NOT NULL DEFAULT '',
				api_format          TEXT    NOT NULL DEFAULT '',
				stream              INTEGER NOT NULL DEFAULT 0,
				reasoning_effort    TEXT    NOT NULL DEFAULT '',
				client_ip           TEXT    NOT NULL DEFAULT '',
				request_modified    INTEGER NOT NULL DEFAULT 0,
				response_modified   INTEGER NOT NULL DEFAULT 0,
				modifications       TEXT    NOT NULL DEFAULT '',
				http_status         INTEGER NOT NULL DEFAULT 0,
				success             INTEGER NOT NULL DEFAULT 0,
				error_msg           TEXT    NOT NULL DEFAULT '',
				cancelled           INTEGER NOT NULL DEFAULT 0,
				prompt_tokens       INTEGER NOT NULL DEFAULT 0,
				cached_tokens       INTEGER NOT NULL DEFAULT 0,
				cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
				completion_tokens   INTEGER NOT NULL DEFAULT 0,
				reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
				total_tokens        INTEGER NOT NULL DEFAULT 0,
				tokens_estimated    INTEGER NOT NULL DEFAULT 0,
				ttft_ms             INTEGER NOT NULL DEFAULT 0,
				total_ms            INTEGER NOT NULL DEFAULT 0,
				tps                 REAL    NOT NULL DEFAULT 0,
				req_headers         TEXT    NOT NULL DEFAULT '',
				req_body            TEXT    NOT NULL DEFAULT '',
				req_body_original   TEXT    NOT NULL DEFAULT '',
				resp_body           TEXT    NOT NULL DEFAULT ''
			)`,

			`CREATE INDEX IF NOT EXISTS idx_rl_ts_start ON request_logs(ts_start DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_rl_provider ON request_logs(provider_id)`,
			`CREATE INDEX IF NOT EXISTS idx_rl_model    ON request_logs(model)`,
			`CREATE INDEX IF NOT EXISTS idx_rl_status   ON request_logs(http_status)`,
		},
	},
	{
		// 供应商的套餐余量查询配置。空串表示未启用，与 custom_usage_json 的处理一致。
		version: 2,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN usage_query_json TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// 服务端定时查到的用量快照，每个供应商一行。
		//
		// 单独一张表而不是挂在 providers 上：providers 存的是配置，前端编辑时会整份覆盖写回，
		// 把定时刷出来的运行态数据混在里面，迟早被一次普通的「保存」抹掉。
		version: 3,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS provider_usage (
				provider_id  INTEGER PRIMARY KEY,
				fetched_at   TEXT    NOT NULL,
				payload_json TEXT    NOT NULL DEFAULT ''
			)`,
		},
	},
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("创建迁移表失败: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("读取迁移版本失败: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("开启迁移事务失败: %w", err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("迁移 v%d 执行失败: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, datetime('now'))`,
			m.version,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("记录迁移 v%d 失败: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交迁移 v%d 失败: %w", m.version, err)
		}
	}
	return nil
}
