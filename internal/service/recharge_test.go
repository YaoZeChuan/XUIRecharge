package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"xui-recharge/internal/config"
	"xui-recharge/internal/database"
	"xui-recharge/internal/panel"
	"xui-recharge/internal/security"
)

func testApp(t *testing.T) (*App, *sql.DB, *panel.Mock) {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/recharge.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO admin_users(id,username,password_hash,created_at,updated_at) VALUES(1,'test-admin','test-hash',1,1)`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Panel:    panelConfigForTest(),
		Recharge: config.RechargeConfig{AllowExpiredRenewal: true, MaxDaysPerOrder: 3650, MaxTrafficGBPerOrder: 10000},
		Security: config.SecurityConfig{MaxFailedAttempts: 5, AdminSessionHours: 12, CardHMACSecret: []byte(strings.Repeat("c", 64)), SessionSecret: []byte(strings.Repeat("s", 64))},
	}
	mock := panel.NewMock()
	return New(db, mock, cfg), db, mock
}

func panelConfigForTest() config.PanelConfig {
	return config.PanelConfig{Mode: "mock", TimeoutSeconds: 5, TLSVerify: true}
}

func TestRechargeLifecycleAndSingleUse(t *testing.T) {
	ctx := context.Background()
	app, db, mock := testApp(t)
	combo, err := app.SavePackage(ctx, 1, "127.0.0.1", 0, PackageInput{Name: "30 天 + 50 GiB", Type: "COMBO", DurationDays: 30, TrafficBytes: 50 * gib, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := app.GenerateCards(ctx, 1, "127.0.0.1", GenerateCardsInput{PackageID: combo.ID, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	before, err := mock.GetClient(ctx, "user001")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := app.Preview(ctx, "user001", generated.Cards[0])
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.Confirm(ctx, preview.PreviewToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "SUCCESS" || result.OrderNo == "" || result.OrderToken == "" {
		t.Fatalf("unexpected redemption result: %#v", result)
	}
	after, err := mock.GetClient(ctx, "user001")
	if err != nil {
		t.Fatal(err)
	}
	if after.ExpiryMS != before.ExpiryMS+int64(30*24*time.Hour/time.Millisecond) {
		t.Fatalf("expiry = %d, want prior expiry plus 30 days", after.ExpiryMS)
	}
	if after.TotalBytes != before.TotalBytes+50*gib {
		t.Fatalf("total bytes = %d, want prior total plus package quota", after.TotalBytes)
	}
	if _, err := app.Preview(ctx, "user001", generated.Cards[0]); err == nil {
		t.Fatal("used card was accepted again")
	}
	var cardStatus string
	if err := db.QueryRow(`SELECT status FROM recharge_cards WHERE card_hash=?`, mustCardHash(t, app, generated.Cards[0])).Scan(&cardStatus); err != nil {
		t.Fatal(err)
	}
	if cardStatus != "USED" {
		t.Fatalf("card status = %s, want USED", cardStatus)
	}
	queried, err := app.PublicOrder(ctx, result.OrderNo, result.OrderToken)
	if err != nil || queried.Status != "SUCCESS" {
		t.Fatalf("authorized order query = %#v, %v", queried, err)
	}
	if _, err := app.PublicOrder(ctx, result.OrderNo, "wrong-token"); err == nil {
		t.Fatal("order was readable without the query token")
	}
}

func TestExpiredRenewalAndUnlimitedQuotaRejection(t *testing.T) {
	ctx := context.Background()
	app, _, mock := testApp(t)
	timePackage, err := app.SavePackage(ctx, 1, "127.0.0.1", 0, PackageInput{Name: "7 天", Type: "TIME", DurationDays: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := app.GenerateCards(ctx, 1, "127.0.0.1", GenerateCardsInput{PackageID: timePackage.ID, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := app.Preview(ctx, "expired-demo", generated.Cards[0])
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.Confirm(ctx, preview.PreviewToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "SUCCESS" {
		t.Fatalf("expired renewal status = %s, want SUCCESS", result.Status)
	}
	client, err := mock.GetClient(ctx, "expired-demo")
	if err != nil {
		t.Fatal(err)
	}
	if delta := time.Duration(client.ExpiryMS-time.Now().UTC().UnixMilli()) * time.Millisecond; delta < 7*24*time.Hour-time.Second || delta > 7*24*time.Hour+time.Second {
		t.Fatalf("expired client renewed for unexpected duration: %s", delta)
	}
	if _, err := app.Preview(ctx, "unlimited-demo", generated.Cards[1]); err == nil {
		t.Fatal("time card was accepted for unlimited-expiry client")
	}
	var status string
	if err := app.DB.QueryRow(`SELECT status FROM recharge_cards WHERE card_hash=?`, mustCardHash(t, app, generated.Cards[1])).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "UNUSED" {
		t.Fatalf("rejected card status = %s, want UNUSED", status)
	}
}

func TestConcurrentConfirmUsesCardOnce(t *testing.T) {
	ctx := context.Background()
	app, db, mock := testApp(t)
	pkg, err := app.SavePackage(ctx, 1, "127.0.0.1", 0, PackageInput{Name: "10 GiB", Type: "TRAFFIC", TrafficBytes: 10 * gib, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cards, err := app.GenerateCards(ctx, 1, "127.0.0.1", GenerateCardsInput{PackageID: pkg.ID, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := mock.GetClient(ctx, "user001")
	preview, err := app.Preview(ctx, "user001", cards.Cards[0])
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan OrderResult, 2)
	errorsFound := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := app.Confirm(ctx, preview.PreviewToken)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	close(errorsFound)
	successes := 0
	for result := range results {
		if result.Status == "SUCCESS" {
			successes++
		}
	}
	for range errorsFound {
	}
	if successes != 1 {
		t.Fatalf("successful confirmations = %d, want 1", successes)
	}
	after, _ := mock.GetClient(ctx, "user001")
	if after.TotalBytes != before.TotalBytes+10*gib {
		t.Fatalf("quota was modified more than once: total=%d", after.TotalBytes)
	}
	var orderCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM recharge_orders WHERE card_id=(SELECT id FROM recharge_cards WHERE card_hash=?)`, mustCardHash(t, app, cards.Cards[0])).Scan(&orderCount); err != nil {
		t.Fatal(err)
	}
	if orderCount != 1 {
		t.Fatalf("orders for card = %d, want 1", orderCount)
	}
}

func TestPanelPreflightFailureReleasesCardWithoutMutation(t *testing.T) {
	ctx := context.Background()
	app, db, mock := testApp(t)
	pkg, err := app.SavePackage(ctx, 1, "127.0.0.1", 0, PackageInput{Name: "30 天", Type: "TIME", DurationDays: 30, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cards, err := app.GenerateCards(ctx, 1, "127.0.0.1", GenerateCardsInput{PackageID: pkg.ID, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	before, err := mock.GetClient(ctx, "user001")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := app.Preview(ctx, "user001", cards.Cards[0])
	if err != nil {
		t.Fatal(err)
	}
	app.Panel = failingPreflightPanel{ClientAPI: mock}
	result, err := app.Confirm(ctx, preview.PreviewToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "FAILED" {
		t.Fatalf("order status = %s, want FAILED", result.Status)
	}
	after, err := mock.GetClient(ctx, "user001")
	if err != nil {
		t.Fatal(err)
	}
	if after.ExpiryMS != before.ExpiryMS || after.TotalBytes != before.TotalBytes {
		t.Fatalf("panel state changed despite failed preflight: before=%#v after=%#v", before, after)
	}
	var cardStatus string
	if err := db.QueryRow(`SELECT status FROM recharge_cards WHERE card_hash=?`, mustCardHash(t, app, cards.Cards[0])).Scan(&cardStatus); err != nil {
		t.Fatal(err)
	}
	if cardStatus != "UNUSED" {
		t.Fatalf("card status = %s, want UNUSED", cardStatus)
	}
}

type failingPreflightPanel struct {
	panel.ClientAPI
}

func (p failingPreflightPanel) CheckConnection(context.Context) (panel.Info, error) {
	return panel.Info{}, errors.New("OpenAPI unavailable")
}

func mustCardHash(t *testing.T, app *App, card string) string {
	t.Helper()
	hash, err := security.CardHash(app.Cfg.Security.CardHMACSecret, card)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
