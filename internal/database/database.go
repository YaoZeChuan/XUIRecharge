package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(absPath) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	_ = os.Chmod(filepath.Dir(absPath), 0700)
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := os.Stat(absPath); err == nil {
		_ = os.Chmod(absPath, 0600)
	}
	return db, nil
}

func Migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash TEXT NOT NULL UNIQUE,
  csrf_hash TEXT NOT NULL,
  admin_id INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_admin_sessions_expiry ON admin_sessions(expires_at);
CREATE TABLE IF NOT EXISTS login_attempts (
  ip TEXT PRIMARY KEY,
  failures INTEGER NOT NULL DEFAULT 0,
  blocked_until INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS packages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  type TEXT NOT NULL CHECK(type IN ('TIME','TRAFFIC','COMBO')),
  duration_days INTEGER NOT NULL DEFAULT 0 CHECK(duration_days >= 0),
  traffic_bytes INTEGER NOT NULL DEFAULT 0 CHECK(traffic_bytes >= 0),
  description TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS recharge_cards (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  card_hash TEXT NOT NULL UNIQUE,
  card_mask TEXT NOT NULL,
  package_id INTEGER NOT NULL REFERENCES packages(id),
  package_snapshot TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('UNUSED','PROCESSING','USED','DISABLED','EXPIRED','REVIEW')),
  bound_client_email TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0,
  used_at INTEGER NOT NULL DEFAULT 0,
  used_by TEXT NOT NULL DEFAULT '',
  order_id INTEGER,
  batch_no TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cards_status_created ON recharge_cards(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cards_batch ON recharge_cards(batch_no);
CREATE TABLE IF NOT EXISTS recharge_orders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_no TEXT NOT NULL UNIQUE,
  card_id INTEGER NOT NULL REFERENCES recharge_cards(id),
  order_access_hash TEXT NOT NULL DEFAULT '',
  client_email TEXT NOT NULL,
  status TEXT NOT NULL,
  duration_days INTEGER NOT NULL DEFAULT 0,
  traffic_bytes INTEGER NOT NULL DEFAULT 0,
  before_snapshot TEXT NOT NULL DEFAULT '',
  expected_snapshot TEXT NOT NULL DEFAULT '',
  after_snapshot TEXT NOT NULL DEFAULT '',
  external_request_state TEXT NOT NULL DEFAULT 'NOT_STARTED',
  error_message TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_orders_status_created ON recharge_orders(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_orders_client_created ON recharge_orders(client_email, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_card_active ON recharge_orders(card_id) WHERE status <> 'FAILED';
CREATE TABLE IF NOT EXISTS recharge_order_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER NOT NULL REFERENCES recharge_orders(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  event_status TEXT NOT NULL,
  details TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_order_events_order ON recharge_order_events(order_id, id);
CREATE TABLE IF NOT EXISTS recharge_previews (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash TEXT NOT NULL UNIQUE,
  card_id INTEGER NOT NULL REFERENCES recharge_cards(id) ON DELETE CASCADE,
  client_email TEXT NOT NULL,
  client_snapshot TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  consumed_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_previews_expiry ON recharge_previews(expires_at);
CREATE TABLE IF NOT EXISTS audit_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  admin_id INTEGER REFERENCES admin_users(id),
  action TEXT NOT NULL,
  target_type TEXT NOT NULL DEFAULT '',
  target_id TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT '',
  details TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC);
CREATE TABLE IF NOT EXISTS app_settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS client_reset_orders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_no TEXT NOT NULL UNIQUE,
  request_key TEXT NOT NULL UNIQUE,
  client_email TEXT NOT NULL,
  plan_code TEXT NOT NULL,
  plan_name TEXT NOT NULL,
  duration_days INTEGER NOT NULL,
  duration_months INTEGER NOT NULL DEFAULT 0,
  traffic_bytes INTEGER NOT NULL,
  points_cost INTEGER NOT NULL,
  payment_status TEXT NOT NULL DEFAULT 'NOT_REQUIRED',
  payment_trade_no TEXT NOT NULL DEFAULT '',
  payment_money TEXT NOT NULL DEFAULT '',
  payment_url TEXT NOT NULL DEFAULT '',
  payment_checked_at INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  external_request_state TEXT NOT NULL DEFAULT 'NOT_STARTED',
  before_snapshot TEXT NOT NULL DEFAULT '',
  expected_snapshot TEXT NOT NULL DEFAULT '',
  after_snapshot TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_client_reset_orders_email_created ON client_reset_orders(client_email, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_client_reset_orders_status_created ON client_reset_orders(status, created_at DESC);
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (1, strftime('%s','now') * 1000);
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (2, strftime('%s','now') * 1000);
`
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin database migration: %w", err)
	}
	if _, err := tx.Exec(schema); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply database migration 1: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit database migration: %w", err)
	}
	if err := ensureColumn(db, "client_reset_orders", "duration_months", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("apply database migration 3: %w", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (3, strftime('%s','now') * 1000)`); err != nil {
		return fmt.Errorf("record database migration 3: %w", err)
	}
	if err := ensureColumn(db, "client_reset_orders", "payment_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("apply database migration 4: %w", err)
	}
	if err := ensureColumn(db, "client_reset_orders", "payment_trade_no", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("apply database migration 4: %w", err)
	}
	if err := ensureColumn(db, "client_reset_orders", "payment_money", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("apply database migration 4: %w", err)
	}
	if err := ensureColumn(db, "client_reset_orders", "payment_checked_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("apply database migration 4: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_client_reset_orders_payment_poll ON client_reset_orders(payment_status, payment_checked_at, created_at)`); err != nil {
		return fmt.Errorf("apply database migration 4: %w", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (4, strftime('%s','now') * 1000)`); err != nil {
		return fmt.Errorf("record database migration 4: %w", err)
	}
	return nil
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

func NowMS() int64 { return time.Now().UTC().UnixMilli() }

func NormalizeEmail(value string) string {
	return strings.TrimSpace(value)
}
