package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenEnablesSQLiteSafetyOptions(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "data", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var foreignKeys int
	if err := db.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatal("foreign key enforcement is disabled")
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations WHERE version=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("schema migration version 1 count = %d, error = %v", count, err)
	}
}
