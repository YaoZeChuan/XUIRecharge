package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"xui-recharge/internal/panel"
	"xui-recharge/internal/security"
)

var ErrRedemptionRejected = errors.New("卡密或客户端信息无法验证，请检查后重试")
var ErrPreviewExpired = errors.New("预览已过期，请重新输入卡密")

const previewLifetime = 10 * time.Minute

func (a *App) Preview(ctx context.Context, email, cardValue string) (PreviewResult, error) {
	if len(a.Cfg.Security.CardHMACSecret) < 32 || len(a.Cfg.Security.SessionSecret) < 32 {
		return PreviewResult{}, errors.New("CARD_HMAC_SECRET and SESSION_SECRET must contain at least 32 bytes")
	}
	email = strings.TrimSpace(email)
	if email == "" || len(email) > 254 {
		return PreviewResult{}, ErrRedemptionRejected
	}
	cardHashes, err := a.cardHashes(cardValue)
	if err != nil {
		return PreviewResult{}, ErrRedemptionRejected
	}
	var cardID int64
	var status, packageData, boundEmail string
	var cardExpires int64
	placeholders := strings.TrimRight(strings.Repeat("?,", len(cardHashes)), ",")
	args := make([]any, len(cardHashes))
	for i, hash := range cardHashes {
		args[i] = hash
	}
	err = a.DB.QueryRowContext(ctx, `SELECT id,status,package_snapshot,bound_client_email,expires_at FROM recharge_cards WHERE card_hash IN (`+placeholders+")", args...).Scan(&cardID, &status, &packageData, &boundEmail, &cardExpires)
	if err != nil || status != "UNUSED" || (cardExpires > 0 && cardExpires <= a.nowMS()) || (boundEmail != "" && !strings.EqualFold(boundEmail, email)) {
		if err == nil && status == "UNUSED" && cardExpires > 0 && cardExpires <= a.nowMS() {
			_, _ = a.DB.ExecContext(ctx, `UPDATE recharge_cards SET status='EXPIRED',updated_at=? WHERE id=? AND status='UNUSED'`, a.nowMS(), cardID)
		}
		return PreviewResult{}, ErrRedemptionRejected
	}
	pkg, err := a.packageFromJSON(packageData)
	if err != nil {
		return PreviewResult{}, errors.New("卡密套餐数据异常，请联系管理员")
	}
	client, err := a.Panel.GetClient(ctx, email)
	if err != nil || !strings.EqualFold(client.Email, email) || len(client.InboundIDs) == 0 {
		return PreviewResult{}, ErrRedemptionRejected
	}
	if pkg.DurationDays > 0 && client.ExpiryMS < 0 {
		return PreviewResult{}, errors.New("目标账号处于面板特殊续期状态，暂不能安全充值；卡密未消耗")
	}
	if (pkg.DurationDays > 0 && client.ExpiryMS == 0) || (pkg.TrafficBytes > 0 && client.TotalBytes == 0) {
		return PreviewResult{}, errors.New("目标账号的对应额度为无限制，无法使用此套餐；卡密未消耗")
	}
	if pkg.DurationDays > 0 && client.ExpiryMS <= a.nowMS() && !a.Cfg.Recharge.AllowExpiredRenewal {
		return PreviewResult{}, errors.New("当前不允许为已过期账号续期；卡密未消耗")
	}
	if !reasonableInt64Add(client.TotalBytes, pkg.TrafficBytes) {
		return PreviewResult{}, errors.New("目标账号流量额度超出系统支持范围；卡密未消耗")
	}
	before := clientSnapshot(client)
	expected := expectedSnapshot(before, pkg, a.Now())
	if expected.ExpiryMS < 0 || expected.TotalBytes < 0 {
		return PreviewResult{}, errors.New("充值后额度超出系统支持范围；卡密未消耗")
	}
	previewToken, _, err := security.NewToken()
	if err != nil {
		return PreviewResult{}, err
	}
	created := a.nowMS()
	expires := created + int64(previewLifetime/time.Millisecond)
	beforeJSON, _ := json.Marshal(before)
	_, err = a.DB.ExecContext(ctx, `INSERT INTO recharge_previews(token_hash,card_id,client_email,client_snapshot,expires_at,created_at) VALUES(?,?,?,?,?,?)`, security.HashTokenHMAC(a.Cfg.Security.SessionSecret, previewToken), cardID, email, string(beforeJSON), expires, created)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("save recharge preview: %w", err)
	}
	_, _ = a.DB.ExecContext(ctx, `DELETE FROM recharge_previews WHERE expires_at<? OR consumed_at>0`, created-int64(24*time.Hour/time.Millisecond))
	return PreviewResult{PreviewToken: previewToken, ExpiresAt: expires, ClientEmail: client.Email, Package: pkg, Before: before, Expected: expected, NodeSync: "unknown"}, nil
}

func (a *App) Confirm(ctx context.Context, previewToken string) (OrderResult, error) {
	if len(a.Cfg.Security.SessionSecret) < 32 {
		return OrderResult{}, errors.New("SESSION_SECRET must contain at least 32 bytes")
	}
	if len(previewToken) < 32 || len(previewToken) > 128 {
		return OrderResult{}, ErrPreviewExpired
	}
	accessToken, _, err := security.NewToken()
	if err != nil {
		return OrderResult{}, err
	}
	orderNo, err := orderNumber()
	if err != nil {
		return OrderResult{}, err
	}
	tokenHash := security.HashTokenHMAC(a.Cfg.Security.SessionSecret, previewToken)
	now := a.nowMS()
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return OrderResult{}, err
	}
	defer tx.Rollback()
	var previewID, cardID int64
	var email, savedSnapshot string
	var previewExpires, consumed, cardExpires int64
	var status, packageData string
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.card_id,p.client_email,p.client_snapshot,p.expires_at,p.consumed_at,c.status,c.package_snapshot,c.expires_at FROM recharge_previews p JOIN recharge_cards c ON c.id=p.card_id WHERE p.token_hash=?`, tokenHash).Scan(&previewID, &cardID, &email, &savedSnapshot, &previewExpires, &consumed, &status, &packageData, &cardExpires)
	if err != nil || consumed != 0 || previewExpires <= now || status != "UNUSED" {
		return OrderResult{}, ErrPreviewExpired
	}
	if cardExpires > 0 && cardExpires <= now {
		_, _ = tx.ExecContext(ctx, `UPDATE recharge_cards SET status='EXPIRED',updated_at=? WHERE id=? AND status='UNUSED'`, now, cardID)
		_, _ = tx.ExecContext(ctx, `UPDATE recharge_previews SET consumed_at=? WHERE id=?`, now, previewID)
		if err := tx.Commit(); err != nil {
			return OrderResult{}, err
		}
		return OrderResult{}, ErrPreviewExpired
	}
	pkg, err := a.packageFromJSON(packageData)
	if err != nil {
		return OrderResult{}, errors.New("卡密套餐数据异常，请联系管理员")
	}
	var before Snapshot
	if err := json.Unmarshal([]byte(savedSnapshot), &before); err != nil {
		return OrderResult{}, errors.New("充值预览数据异常")
	}
	accessHash := security.HashToken(accessToken)
	_, err = tx.ExecContext(ctx, `INSERT INTO recharge_orders(order_no,card_id,order_access_hash,client_email,status,duration_days,traffic_bytes,before_snapshot,created_at,updated_at) VALUES(?,?,?,?,'RESERVED',?,?,?,?,?)`, orderNo, cardID, accessHash, email, pkg.DurationDays, pkg.TrafficBytes, savedSnapshot, now, now)
	if err != nil {
		return OrderResult{}, fmt.Errorf("reserve recharge order: %w", err)
	}
	var orderID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM recharge_orders WHERE order_no=?`, orderNo).Scan(&orderID); err != nil {
		return OrderResult{}, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE recharge_cards SET status='PROCESSING',order_id=?,updated_at=? WHERE id=? AND status='UNUSED'`, orderID, now, cardID)
	if err != nil {
		return OrderResult{}, err
	}
	changed, _ := updated.RowsAffected()
	if changed != 1 {
		return OrderResult{}, ErrPreviewExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recharge_previews SET consumed_at=? WHERE id=? AND consumed_at=0`, now, previewID); err != nil {
		return OrderResult{}, err
	}
	if err := a.recordEvent(ctx, tx, orderID, "CARD_RESERVED", "RESERVED", "卡密已锁定，等待面板核验"); err != nil {
		return OrderResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OrderResult{}, err
	}

	client, err := a.Panel.GetClient(ctx, email)
	if err != nil {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "无法读取当前面板状态；卡密已释放", Snapshot{})
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	current := clientSnapshot(client)
	if !sameEntitlement(before, current) {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "账号额度在预览后发生变化，请重新预览；卡密已释放", current)
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	if (pkg.DurationDays > 0 && client.ExpiryMS == 0) || (pkg.DurationDays > 0 && client.ExpiryMS < 0) || (pkg.TrafficBytes > 0 && client.TotalBytes == 0) || (pkg.DurationDays > 0 && client.ExpiryMS <= a.nowMS() && !a.Cfg.Recharge.AllowExpiredRenewal) {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "账号状态不再符合套餐条件；卡密已释放", current)
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	if !reasonableInt64Add(client.TotalBytes, pkg.TrafficBytes) {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "流量额度超出系统支持范围；卡密已释放", current)
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	if _, err := a.Panel.CheckConnection(ctx); err != nil {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "面板连接或 API Schema 未通过核验；卡密已释放", current)
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	if pkg.DurationDays > 0 && client.ExpiryMS <= a.nowMS() {
		if err := a.Panel.ValidateSetQuota(ctx, client, true, pkg.TrafficBytes > 0); err != nil {
			_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "面板未提供可安全续期的完整客户端数据；卡密已释放", current)
			return a.loadOrderResult(ctx, orderNo, accessToken)
		}
	}
	expected := expectedSnapshot(current, pkg, a.Now())
	if expected.ExpiryMS < 0 || expected.TotalBytes < 0 {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "充值后额度超出系统支持范围；卡密已释放", current)
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	beforeJSON, _ := json.Marshal(current)
	expectedJSON, _ := json.Marshal(expected)
	started := a.nowMS()
	if _, err := a.DB.ExecContext(ctx, `UPDATE recharge_orders SET status='APPLYING',before_snapshot=?,expected_snapshot=?,external_request_state='REQUEST_MAY_BE_SENT',started_at=?,updated_at=? WHERE id=? AND status='RESERVED'`, string(beforeJSON), string(expectedJSON), started, started, orderID); err != nil {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "REVIEW_REQUIRED", "REQUEST_MAY_BE_SENT", "订单状态无法持久化，需人工核对", Snapshot{})
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	_, _ = a.DB.ExecContext(ctx, `INSERT INTO recharge_order_events(order_id,event_type,event_status,details,created_at) VALUES(?,?,?,?,?)`, orderID, "PANEL_REQUEST", "REQUEST_MAY_BE_SENT", "请求即将发送；发生重启或超时时不得自动重试", started)

	var applyErr error
	if pkg.DurationDays > 0 && client.ExpiryMS <= started {
		setTotal := pkg.TrafficBytes > 0
		applyErr = a.Panel.SetQuota(ctx, client, expected.ExpiryMS, expected.TotalBytes, true, setTotal)
	} else {
		applyErr = a.Panel.Adjust(ctx, email, pkg.DurationDays, pkg.TrafficBytes)
	}
	if errors.Is(applyErr, panel.ErrWriteNotSent) {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "FAILED", "NOT_STARTED", "面板额度修改接口未通过安全校验，未发送请求；卡密已释放", current)
		return a.loadOrderResult(ctx, orderNo, accessToken)
	}
	afterClient, verifyErr := a.Panel.GetClient(ctx, email)
	if verifyErr == nil && snapshotMatches(expected, afterClient) && applyErr == nil {
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "SUCCESS", "ACKNOWLEDGED_VERIFIED", "主面板额度已核验；远程节点同步状态未知", clientSnapshot(afterClient))
	} else {
		detail := "面板修改结果无法确认，请管理员核对后再处理"
		if applyErr != nil {
			detail = safeError(applyErr)
		}
		if verifyErr != nil {
			detail += "；复查面板状态失败"
		} else if !snapshotMatches(expected, afterClient) {
			detail += "；当前额度与预期不一致"
		}
		_ = a.finishOrder(ctx, orderID, cardID, orderNo, "REVIEW_REQUIRED", "OUTCOME_UNKNOWN", detail, clientSnapshotIfAvailable(afterClient, verifyErr))
	}
	return a.loadOrderResult(ctx, orderNo, accessToken)
}

func (a *App) finishOrder(ctx context.Context, orderID, cardID int64, orderNo, status, requestState, message string, after Snapshot) error {
	afterJSON := ""
	if after.HasEnabled || after.ExpiryMS != 0 || after.TotalBytes != 0 || after.InboundCount > 0 || after.UploadBytes != 0 || after.DownloadBytes != 0 {
		encoded, _ := json.Marshal(after)
		afterJSON = string(encoded)
	}
	now := a.nowMS()
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE recharge_orders SET status=?,external_request_state=?,error_message=?,after_snapshot=?,finished_at=?,updated_at=? WHERE id=?`, status, requestState, message, afterJSON, now, now, orderID); err != nil {
		return err
	}
	switch status {
	case "SUCCESS":
		if _, err := tx.ExecContext(ctx, `UPDATE recharge_cards SET status='USED',used_at=?,updated_at=? WHERE id=?`, now, now, cardID); err != nil {
			return err
		}
	case "FAILED":
		if _, err := tx.ExecContext(ctx, `UPDATE recharge_cards SET status='UNUSED',order_id=NULL,updated_at=? WHERE id=? AND status='PROCESSING'`, now, cardID); err != nil {
			return err
		}
	case "REVIEW_REQUIRED":
		if _, err := tx.ExecContext(ctx, `UPDATE recharge_cards SET status='REVIEW',updated_at=? WHERE id=?`, now, cardID); err != nil {
			return err
		}
	}
	if err := a.recordEvent(ctx, tx, orderID, "ORDER_FINALIZED", status, message); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) PublicOrder(ctx context.Context, orderNo, accessToken string) (OrderResult, error) {
	return a.loadOrderResult(ctx, orderNo, accessToken)
}

func (a *App) loadOrderResult(ctx context.Context, orderNo, accessToken string) (OrderResult, error) {
	if accessToken == "" || len(a.Cfg.Security.SessionSecret) < 32 {
		return OrderResult{}, ErrUnauthorized
	}
	var expectedHash string
	if err := a.DB.QueryRowContext(ctx, `SELECT order_access_hash FROM recharge_orders WHERE order_no=?`, orderNo).Scan(&expectedHash); err != nil {
		return OrderResult{}, ErrUnauthorized
	}
	if !security.VerifyTokenHash(expectedHash, accessToken) {
		return OrderResult{}, ErrUnauthorized
	}
	return a.orderResultByNo(ctx, orderNo, accessToken)
}

func (a *App) cardHashes(value string) ([]string, error) {
	primary, err := security.CardHash(a.Cfg.Security.CardHMACSecret, value)
	if err != nil {
		return nil, err
	}
	hashes := []string{primary}
	if len(a.Cfg.Security.CardHMACPreviousSecret) >= 32 {
		previous, err := security.CardHash(a.Cfg.Security.CardHMACPreviousSecret, value)
		if err == nil && previous != primary {
			hashes = append(hashes, previous)
		}
	}
	return hashes, nil
}

func (a *App) orderResultByNo(ctx context.Context, orderNo, accessToken string) (OrderResult, error) {
	var result OrderResult
	var packageData, beforeJSON, expectedJSON, afterJSON string
	var errorMessage string
	var updated int64
	err := a.DB.QueryRowContext(ctx, `SELECT o.order_no,o.status,o.client_email,c.package_snapshot,o.before_snapshot,o.expected_snapshot,o.after_snapshot,o.error_message,o.created_at,o.updated_at FROM recharge_orders o JOIN recharge_cards c ON c.id=o.card_id WHERE o.order_no=?`, orderNo).Scan(&result.OrderNo, &result.Status, &result.ClientEmail, &packageData, &beforeJSON, &expectedJSON, &afterJSON, &errorMessage, &result.CreatedAt, &updated)
	if err != nil {
		return OrderResult{}, err
	}
	result.OrderToken = accessToken
	result.ErrorMessage = errorMessage
	result.UpdatedAt = updated
	result.NodeSync = "unknown"
	result.Package, err = a.packageFromJSON(packageData)
	if err != nil {
		return OrderResult{}, err
	}
	_ = json.Unmarshal([]byte(beforeJSON), &result.Before)
	_ = json.Unmarshal([]byte(expectedJSON), &result.Expected)
	_ = json.Unmarshal([]byte(afterJSON), &result.After)
	return result, nil
}

func clientSnapshotIfAvailable(client panel.Client, err error) Snapshot {
	if err != nil {
		return Snapshot{}
	}
	return clientSnapshot(client)
}

func sameEntitlement(before, current Snapshot) bool {
	left := append([]int64(nil), before.InboundIDs...)
	right := append([]int64(nil), current.InboundIDs...)
	slices.Sort(left)
	slices.Sort(right)
	return before.ExpiryMS == current.ExpiryMS && before.TotalBytes == current.TotalBytes && before.Enabled == current.Enabled && before.HasEnabled == current.HasEnabled && slices.Equal(left, right)
}

func safeError(err error) string {
	message := strings.TrimSpace(err.Error())
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 260 {
		message = message[:260]
	}
	if message == "" {
		return "面板修改结果未知"
	}
	return "面板操作未能确认：" + message
}

func orderStatusError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRedemptionRejected
	}
	return err
}
