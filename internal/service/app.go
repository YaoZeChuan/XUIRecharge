package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"xui-recharge/internal/config"
	"xui-recharge/internal/ldc"
	"xui-recharge/internal/panel"
	"xui-recharge/internal/security"
)

const gib = int64(1024 * 1024 * 1024)

type App struct {
	DB                *sql.DB
	Panel             panel.ClientAPI
	Cfg               config.Config
	LDC               *ldc.Client
	Now               func() time.Time
	dummyPasswordHash string
	ldcMu             sync.Mutex
}

type Package struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	DurationDays int    `json:"durationDays"`
	TrafficBytes int64  `json:"trafficBytes"`
	Description  string `json:"description"`
	Enabled      bool   `json:"enabled"`
	SortOrder    int    `json:"sortOrder"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

type Snapshot struct {
	ExpiryMS            int64   `json:"expiryTime"`
	TotalBytes          int64   `json:"totalBytes"`
	UploadBytes         int64   `json:"uploadBytes"`
	DownloadBytes       int64   `json:"downloadBytes"`
	HasTrafficBreakdown bool    `json:"hasTrafficBreakdown"`
	UsedBytes           int64   `json:"usedBytes"`
	RemainingBytes      int64   `json:"remainingBytes"`
	Enabled             bool    `json:"enabled"`
	HasEnabled          bool    `json:"hasEnabled"`
	InboundCount        int     `json:"inboundCount"`
	InboundIDs          []int64 `json:"inboundIds"`
}

type PreviewResult struct {
	PreviewToken string   `json:"previewToken"`
	ExpiresAt    int64    `json:"expiresAt"`
	ClientEmail  string   `json:"clientEmail"`
	Package      Package  `json:"package"`
	Before       Snapshot `json:"before"`
	Expected     Snapshot `json:"expected"`
	NodeSync     string   `json:"nodeSync"`
}

type OrderResult struct {
	OrderNo      string   `json:"orderNo"`
	OrderToken   string   `json:"orderToken"`
	Status       string   `json:"status"`
	ClientEmail  string   `json:"clientEmail"`
	Package      Package  `json:"package"`
	Before       Snapshot `json:"before"`
	Expected     Snapshot `json:"expected"`
	After        Snapshot `json:"after"`
	ErrorMessage string   `json:"errorMessage,omitempty"`
	NodeSync     string   `json:"nodeSync"`
	CreatedAt    int64    `json:"createdAt"`
	UpdatedAt    int64    `json:"updatedAt"`
}

type OrderListItem struct {
	OrderNo      string `json:"orderNo"`
	Status       string `json:"status"`
	ClientEmail  string `json:"clientEmail"`
	CardMask     string `json:"cardMask"`
	PackageName  string `json:"packageName"`
	DurationDays int    `json:"durationDays"`
	TrafficBytes int64  `json:"trafficBytes"`
	ErrorMessage string `json:"errorMessage"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

type CardListItem struct {
	ID          int64  `json:"id"`
	Mask        string `json:"mask"`
	PackageName string `json:"packageName"`
	Status      string `json:"status"`
	BoundEmail  string `json:"boundClientEmail"`
	ExpiresAt   int64  `json:"expiresAt"`
	UsedAt      int64  `json:"usedAt"`
	CreatedAt   int64  `json:"createdAt"`
	BatchNo     string `json:"batchNo"`
}

func New(db *sql.DB, client panel.ClientAPI, cfg config.Config) *App {
	dummy, _ := security.HashPassword("dummy password used only for timing")
	var paymentClient *ldc.Client
	if cfg.LDC.Enabled() {
		paymentClient, _ = ldc.New(cfg.LDC.BaseURL, cfg.LDC.PublicBaseURL, cfg.LDC.ClientID, cfg.LDC.ClientSecret, cfg.LDC.Ed25519PrivateKey)
	}
	return &App{DB: db, Panel: client, Cfg: cfg, LDC: paymentClient, Now: time.Now, dummyPasswordHash: dummy}
}

func (a *App) nowMS() int64 { return a.Now().UTC().UnixMilli() }

func (a *App) EnsureInitialAdmin(ctx context.Context) error {
	var count int
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return fmt.Errorf("count administrator accounts: %w", err)
	}
	if count > 0 {
		return nil
	}
	username := a.Cfg.Security.AdminUsername
	if username == "" {
		username = "admin"
	}
	if strings.TrimSpace(a.Cfg.Security.AdminInitialPassword) == "" {
		return errors.New("no administrator exists; set ADMIN_INITIAL_PASSWORD (at least 12 characters) for first startup")
	}
	hash, err := security.HashPassword(a.Cfg.Security.AdminInitialPassword)
	if err != nil {
		return fmt.Errorf("initialize administrator password: %w", err)
	}
	now := a.nowMS()
	_, err = a.DB.ExecContext(ctx, `INSERT INTO admin_users(username,password_hash,created_at,updated_at) VALUES(?,?,?,?)`, username, hash, now, now)
	if err != nil {
		return fmt.Errorf("create initial administrator: %w", err)
	}
	return nil
}

func (a *App) PanelStatus(ctx context.Context) (panel.Info, error) {
	return a.Panel.CheckConnection(ctx)
}

func (a *App) RecoverIncompleteOrders(ctx context.Context) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,card_id FROM recharge_orders WHERE status IN ('RESERVED','APPLYING','VERIFYING')`)
	if err != nil {
		return err
	}
	type pending struct{ orderID, cardID int64 }
	var orders []pending
	for rows.Next() {
		var order pending
		if err := rows.Scan(&order.orderID, &order.cardID); err != nil {
			rows.Close()
			return err
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, order := range orders {
		message := "服务重启时订单尚未完成，系统未重发面板请求；请核对面板状态"
		if _, err := tx.ExecContext(ctx, `UPDATE recharge_orders SET status='REVIEW_REQUIRED',external_request_state='RECOVERED_AFTER_RESTART',error_message=?,updated_at=? WHERE id=?`, message, a.nowMS(), order.orderID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recharge_cards SET status='REVIEW',updated_at=? WHERE id=?`, a.nowMS(), order.cardID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recharge_order_events(order_id,event_type,event_status,details,created_at) VALUES(?,?,?,?,?)`, order.orderID, "RECOVERY", "REVIEW_REQUIRED", message, a.nowMS()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func clientSnapshot(client panel.Client) Snapshot {
	used := client.UsedBytes
	hasTrafficBreakdown := client.HasTrafficBreakdown || client.UploadBytes > 0 || client.DownloadBytes > 0
	if hasTrafficBreakdown {
		used = client.UploadBytes + client.DownloadBytes
	}
	remaining := int64(0)
	if client.TotalBytes > 0 && client.TotalBytes > used {
		remaining = client.TotalBytes - used
	}
	return Snapshot{
		ExpiryMS: client.ExpiryMS, TotalBytes: client.TotalBytes,
		UploadBytes: client.UploadBytes, DownloadBytes: client.DownloadBytes,
		HasTrafficBreakdown: hasTrafficBreakdown,
		UsedBytes:           used, RemainingBytes: remaining, Enabled: client.Enabled,
		HasEnabled: client.HasEnabled, InboundCount: len(client.InboundIDs), InboundIDs: slices.Clone(client.InboundIDs),
	}
}

func snapshotMatches(expected Snapshot, actual panel.Client) bool {
	return expected.ExpiryMS == actual.ExpiryMS && expected.TotalBytes == actual.TotalBytes
}

func orderNumber() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "R" + time.Now().UTC().Format("20060102T150405") + "-" + strings.ToUpper(hex.EncodeToString(raw[:])), nil
}

func (a *App) recordEvent(ctx context.Context, tx *sql.Tx, orderID int64, typ, status, details string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO recharge_order_events(order_id,event_type,event_status,details,created_at) VALUES(?,?,?,?,?)`, orderID, typ, status, details, a.nowMS())
	return err
}

func (a *App) writeAudit(ctx context.Context, adminID int64, action, targetType, targetID, ip, details string) error {
	_, err := a.DB.ExecContext(ctx, `INSERT INTO audit_logs(admin_id,action,target_type,target_id,ip,details,created_at) VALUES(?,?,?,?,?,?,?)`, adminID, action, targetType, targetID, ip, details, a.nowMS())
	return err
}

func (a *App) WriteAudit(ctx context.Context, adminID int64, action, targetType, targetID, ip, details string) error {
	return a.writeAudit(ctx, adminID, action, targetType, targetID, ip, details)
}

func (a *App) packageFromJSON(data string) (Package, error) {
	var pkg Package
	if err := json.Unmarshal([]byte(data), &pkg); err != nil {
		return Package{}, fmt.Errorf("decode package snapshot: %w", err)
	}
	return pkg, nil
}

func bytesForPackage(pkg Package) int64 {
	return pkg.TrafficBytes
}

func expectedSnapshot(before Snapshot, pkg Package, now time.Time) Snapshot {
	after := before
	if pkg.DurationDays > 0 {
		base := before.ExpiryMS
		if base <= now.UTC().UnixMilli() {
			base = now.UTC().UnixMilli()
		}
		after.ExpiryMS = base + int64(pkg.DurationDays)*int64(24*time.Hour/time.Millisecond)
	}
	if pkg.TrafficBytes > 0 {
		after.TotalBytes = before.TotalBytes + pkg.TrafficBytes
	}
	if after.TotalBytes > 0 && after.TotalBytes > after.UsedBytes {
		after.RemainingBytes = after.TotalBytes - after.UsedBytes
	} else {
		after.RemainingBytes = 0
	}
	return after
}

func reasonableInt64Add(left, right int64) bool {
	return right >= 0 && left >= 0 && left <= math.MaxInt64-right
}
