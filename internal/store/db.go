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
	// path 是主数据库文件路径，用来报告 VACUUM 前后真实的磁盘占用。
	path string
	// dir 是数据目录，更新暂存文件等「与库同级的运行态文件」放在这里。
	dir string
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
	s := &Store{db: db, path: filepath.Join(dataDir, "ai_proxy.db"), dir: dataDir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DataDir 返回数据目录。
//
// 更新包的暂存文件、以及需要落在「库旁边」的运行态文件都用它，
// 这样用户清理数据目录时不会漏掉这些中间产物。
func (s *Store) DataDir() string { return s.dir }

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Vacuum 重建数据库文件，把删除留下的空闲页真正还给文件系统。
//
// SQLite 的 DELETE 只把页面挂进 freelist，文件不会自己缩小；清空日志这类一次
// 释放大量空间的场景必须显式 VACUUM。代价是重建期间独占连接（本包
// MaxOpenConns=1，期间到达的日志写入会排队等待）。
//
// WAL 模式下 VACUUM 的结果先落在 WAL 里，主文件要等 checkpoint 才会被截断，
// 所以后面必须补一次 TRUNCATE checkpoint：少了它，page_count 已经降下来、
// 磁盘占用却纹丝不动。返回主文件 + WAL 合计回收的字节数供界面展示。
func (s *Store) Vacuum() (int64, error) {
	before := s.diskBytes()
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return 0, fmt.Errorf("回收数据库空间失败: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return 0, fmt.Errorf("回收数据库空间失败: %w", err)
	}
	after := s.diskBytes()
	if after >= before {
		return 0, nil
	}
	return before - after, nil
}

// diskBytes 是数据库主文件与 WAL 的合计占用；取不到的文件按 0 计。
//
// 把 WAL 算进来是因为清空前数据往往还压在 WAL 里，只量主文件会低估释放量。
func (s *Store) diskBytes() int64 {
	var total int64
	for _, p := range []string{s.path, s.path + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// DBStats 描述 SQLite 的页级占用。
//
// 删除行只会把页面挂进 freelist，文件不会自己缩小；界面据此告诉用户
// 「现在点回收能腾出多少」，而不是让人对着一个 2 GB 的文件猜。
type DBStats struct {
	PageCount int64   `json:"pageCount"`
	FreeCount int64   `json:"freeCount"`
	PageSize  int64   `json:"pageSize"`
	FileBytes int64   `json:"fileBytes"` // 主文件 + WAL
	FreeBytes int64   `json:"freeBytes"` // 可回收的上限
	FreeRatio float64 `json:"freeRatio"` // 空闲页占比 0..1
}

// PageStats 读取数据库的页级占用。
func (s *Store) PageStats() (DBStats, error) {
	var st DBStats
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&st.PageCount); err != nil {
		return st, fmt.Errorf("读取数据库页数失败: %w", err)
	}
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&st.FreeCount); err != nil {
		return st, fmt.Errorf("读取数据库空闲页数失败: %w", err)
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&st.PageSize); err != nil {
		return st, fmt.Errorf("读取数据库页大小失败: %w", err)
	}
	st.FileBytes = s.diskBytes()
	st.FreeBytes = st.FreeCount * st.PageSize
	if st.PageCount > 0 {
		st.FreeRatio = float64(st.FreeCount) / float64(st.PageCount)
	}
	return st, nil
}

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
	{
		// 供应商的模型映射（请求模型名 → 上游模型名），以及日志里记下这次实际发出去的模型名。
		//
		// model_mapped 单独一列而不是塞进 modifications：它是可以按值筛/搜的字符串，
		// 塞进 JSON 数组里就只能扫全表了。
		version: 4,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN model_map_json TEXT NOT NULL DEFAULT '[]'`,
			`ALTER TABLE request_logs ADD COLUMN model_mapped TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// Responses 会话续传。默认 off，只有用户显式开启才改写请求。
		//
		// 续传字段单独成列而不是塞进 modifications：模式、原因和省下的字节数
		// 都是需要按值排查的运行态事实，尤其是 dry-run 阶段要看真实命中率。
		version: 5,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN continuation_mode TEXT NOT NULL DEFAULT 'off'`,
			`ALTER TABLE providers ADD COLUMN continuation_models_json TEXT NOT NULL DEFAULT '[]'`,
			`ALTER TABLE request_logs ADD COLUMN continuation_mode TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE request_logs ADD COLUMN continuation_reason TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE request_logs ADD COLUMN saved_bytes INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		// 续传失败后的冷却时间（秒），由用户按供应商配置。-1 表示未配置，
		// 读取时补默认值；0 表示不冷却（回退后下一轮立刻重新建链）。
		version: 6,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN continuation_cooldown_seconds INTEGER NOT NULL DEFAULT -1`,
			`ALTER TABLE providers ADD COLUMN continuation_invalid_cooldown_seconds INTEGER NOT NULL DEFAULT -1`,
		},
	},
	{
		// 严格上游兼容：剔除非标准顶层字段。默认 0，避免影响认 thinking 的网关。
		version: 7,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN strip_unsupported_params INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		// prompt_cache_key 注入模式。默认 off：这是上游厂商（Mistral 等）的
		// 缓存亲和提示，不认这个字段的严格网关会 422，必须按供应商显式开启。
		version: 8,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN prompt_cache_key_mode TEXT NOT NULL DEFAULT 'off'`,
		},
	},
	{
		// 用量查询的账号密码（目前只有 Mistral 模板在用）。
		//
		// 单独成列而不是塞进 usage_query_json：这两项是凭据，与其它配置项的
		// 生命周期不同 —— 改模板不该顺手把密码清掉。
		version: 9,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN usage_account  TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE providers ADD COLUMN usage_password TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// 请求体内联图片压缩的留痕。
		//
		// 单独成列而不是塞进 modifications：这两个是可累加的量，
		// 用户想知道的是「开了这个功能到底省了多少流量」，而不是「这次改过没有」。
		version: 10,
		stmts: []string{
			`ALTER TABLE request_logs ADD COLUMN image_compressed  INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE request_logs ADD COLUMN image_saved_bytes INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		// 单请求图片数量上限（供应商级）与被裁掉的张数（日志级）。
		//
		// 上游按请求内图片总数硬性拒绝（Mistral 超过 20 张整条 400），而客户端
		// 每轮回放全部历史图片，数量不受代理控制。默认 0 表示不限制 ——
		// 老库升级上来时所有供应商保持今天的行为，不会突然开始裁图。
		version: 11,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN max_images_per_request INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE request_logs ADD COLUMN images_dropped INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		// 成批替换的步长。
		//
		// 逐张推进时，丢弃边界每轮都往前挪一格，「第一张保留下来的图」的位置
		// 随之变化，前缀缓存在那里断开、该轮后面十几张图全价重算。按批推进能
		// 让边界在一段窗口内固定，缓存照常命中。默认 0（逐张），老库行为不变。
		version: 12,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN image_trim_batch INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		// Mistral 思维链对称还原开关：把客户端回放的 assistant.reasoning_content
		// 还原成 Mistral 原生 ThinkChunk，而不是直接剥离。
		// 对已开启严格兼容的 mistral.ai 供应商平滑默认开启，老库其他供应商保持 0。
		//
		// 只认官方主机的精确前缀：用 LIKE '%mistral.ai%' 会命中
		// api.mistral.ai.evil.com 这种仿冒域名，也会把第三方网关一起打开，
		// 而它们未必接受 think 内容块。
		version: 13,
		stmts: []string{
			`ALTER TABLE providers ADD COLUMN replay_reasoning_as_think_chunk INTEGER NOT NULL DEFAULT 0`,
			`UPDATE providers SET replay_reasoning_as_think_chunk = 1
			 WHERE strip_unsupported_params = 1 AND (
				lower(base_url) = 'https://api.mistral.ai'
				OR lower(base_url) LIKE 'https://api.mistral.ai/%'
				OR lower(base_url) LIKE 'https://api.mistral.ai:%'
				OR lower(base_url) = 'http://api.mistral.ai'
				OR lower(base_url) LIKE 'http://api.mistral.ai/%'
				OR lower(base_url) LIKE 'http://api.mistral.ai:%'
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
