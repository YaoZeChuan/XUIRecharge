package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"xui-recharge/internal/ldc"
	"xui-recharge/internal/panel"
)

var clientResetPlans = map[string]ClientResetPlan{
	"100g": {Code: "100g", Name: "100 GB", TrafficBytes: 100 * gib, PointsCost: 100, DurationMonths: 1},
	"200g": {Code: "200g", Name: "200 GB", TrafficBytes: 200 * gib, PointsCost: 200, DurationMonths: 1},
	"300g": {Code: "300g", Name: "300 GB", TrafficBytes: 300 * gib, PointsCost: 300, DurationMonths: 1},
}

var ErrClientResetRejected = errors.New("客户端重置请求无法执行")

func rejectClientReset(message string) error {
	return fmt.Errorf("%w: %s", ErrClientResetRejected, message)
}

type ClientResetPlan struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	TrafficBytes   int64  `json:"trafficBytes"`
	PointsCost     int    `json:"pointsCost"`
	DurationDays   int    `json:"durationDays,omitempty"`
	DurationMonths int    `json:"durationMonths,omitempty"`
}

type ClientResetResult struct {
	OrderNo        string          `json:"orderNo"`
	Status         string          `json:"status"`
	ClientEmail    string          `json:"-"`
	Plan           ClientResetPlan `json:"plan"`
	PaymentStatus  string          `json:"paymentStatus"`
	PaymentTradeNo string          `json:"paymentTradeNo,omitempty"`
	PaymentMoney   string          `json:"paymentMoney,omitempty"`
	PaymentURL     string          `json:"paymentUrl,omitempty"`
	Before         Snapshot        `json:"before"`
	Expected       Snapshot        `json:"expected"`
	After          Snapshot        `json:"after"`
	ErrorMessage   string          `json:"errorMessage,omitempty"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
}

type ClientResetListItem struct {
	OrderNo        string `json:"orderNo"`
	PaymentTradeNo string `json:"paymentTradeNo"`
	PaymentMoney   string `json:"paymentMoney"`
	ClientEmail    string `json:"clientEmail"`
	PlanName       string `json:"planName"`
	DurationDays   int    `json:"durationDays"`
	DurationMonths int    `json:"durationMonths"`
	PointsCost     int    `json:"pointsCost"`
	PaymentStatus  string `json:"paymentStatus"`
	Status         string `json:"status"`
	ErrorMessage   string `json:"errorMessage"`
	CreatedAt      int64  `json:"createdAt"`
}

func ClientResetPlans() []ClientResetPlan {
	return []ClientResetPlan{
		clientResetPlans["100g"],
		clientResetPlans["200g"],
		clientResetPlans["300g"],
	}
}

func (a *App) CreateClientResetCheckout(ctx context.Context, subscriptionID, planCode, requestKey string) (ClientResetResult, error) {
	if a.LDC == nil {
		return ClientResetResult{}, errors.New("LDC 支付尚未配置")
	}
	a.ldcMu.Lock()
	defer a.ldcMu.Unlock()

	subscriptionID, err := normalizeSubscriptionID(subscriptionID)
	planCode = strings.ToLower(strings.TrimSpace(planCode))
	requestKey = strings.TrimSpace(requestKey)
	if err != nil || requestKey == "" || len(requestKey) > 128 {
		return ClientResetResult{}, rejectClientReset("请求参数无效")
	}
	plan, ok := clientResetPlans[planCode]
	if !ok {
		return ClientResetResult{}, rejectClientReset("请选择有效的流量套餐")
	}
	client, err := a.Panel.GetClientBySubscriptionID(ctx, subscriptionID)
	if err != nil || client.Email == "" || len(client.InboundIDs) == 0 {
		return ClientResetResult{}, rejectClientReset("无法读取有效订阅账号，请重新查询订阅 ID")
	}
	email := client.Email
	var order ClientResetResult
	if existing, err := a.clientResetOrderByRequestKey(ctx, requestKey); err == nil {
		if !strings.EqualFold(existing.ClientEmail, email) || existing.Plan.Code != plan.Code {
			return ClientResetResult{}, rejectClientReset("请求编号已用于其他重置操作，请刷新后重试")
		}
		if existing.Status != "PAYMENT_PENDING" || existing.PaymentURL != "" {
			return existing, nil
		}
		order = existing
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ClientResetResult{}, rejectClientReset("读取重置订单失败")
	}
	if order.OrderNo == "" {
		var pendingOrderNo string
		err := a.DB.QueryRowContext(ctx, `SELECT order_no FROM client_reset_orders WHERE LOWER(client_email)=LOWER(?) AND (status IN ('PAYMENT_PENDING','PAYMENT_CONFIRMED','APPLYING','VERIFYING','REVIEW_REQUIRED') OR (payment_status='SUCCESS' AND status<>'SUCCESS')) ORDER BY created_at DESC LIMIT 1`, email).Scan(&pendingOrderNo)
		if err == nil {
			return ClientResetResult{}, rejectClientReset("该客户端已有待付款、执行中或待核对的订单，请先查询订单状态")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ClientResetResult{}, rejectClientReset("读取客户端重置状态失败")
		}

		if client.ExpiryMS < 0 {
			return ClientResetResult{}, rejectClientReset("账号处于特殊续期状态，暂不能执行重置")
		}
		if client.ExpiryMS == 0 {
			return ClientResetResult{}, rejectClientReset("无限期账号不能购买带期限的流量套餐")
		}
		if plan.TrafficBytes <= 0 || plan.TrafficBytes > math.MaxInt64 {
			return ClientResetResult{}, rejectClientReset("流量套餐超出系统支持范围")
		}
		if err := a.Panel.ValidateSetQuota(ctx, client, true, true); err != nil {
			return ClientResetResult{}, rejectClientReset("面板不支持安全设置套餐额度，暂不能收款: " + err.Error())
		}
		if err := a.Panel.ValidateResetTraffic(ctx); err != nil {
			return ClientResetResult{}, rejectClientReset("面板不支持流量重置或无法核验流量，暂不能收款: " + err.Error())
		}
		before := clientSnapshot(client)
		expected := before
		expected.TotalBytes = plan.TrafficBytes
		expected.UsedBytes = 0
		expected.RemainingBytes = plan.TrafficBytes
		expected.UploadBytes = 0
		expected.DownloadBytes = 0
		expected.HasTrafficBreakdown = true
		if expected.HasEnabled {
			expected.Enabled = true
		}
		orderNo, err := clientResetOrderNumber(a.Now())
		if err != nil {
			return ClientResetResult{}, rejectClientReset("无法生成重置订单")
		}
		beforeJSON, _ := json.Marshal(before)
		expectedJSON, _ := json.Marshal(expected)
		created := a.nowMS()
		inserted, err := a.DB.ExecContext(ctx, `INSERT INTO client_reset_orders(order_no,request_key,client_email,plan_code,plan_name,duration_days,duration_months,traffic_bytes,points_cost,payment_status,status,external_request_state,before_snapshot,expected_snapshot,created_at,updated_at) SELECT ?,?,?,?,?,?,?,?,?, 'PENDING','PAYMENT_PENDING','NOT_STARTED',?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM client_reset_orders WHERE LOWER(client_email)=LOWER(?) AND (status IN ('PAYMENT_PENDING','PAYMENT_CONFIRMED','APPLYING','VERIFYING','REVIEW_REQUIRED') OR (payment_status='SUCCESS' AND status<>'SUCCESS')))`, orderNo, requestKey, client.Email, plan.Code, plan.Name, plan.DurationDays, plan.DurationMonths, plan.TrafficBytes, plan.PointsCost, string(beforeJSON), string(expectedJSON), created, created, client.Email)
		if err != nil {
			if existing, lookupErr := a.clientResetOrderByRequestKey(ctx, requestKey); lookupErr == nil {
				if !strings.EqualFold(existing.ClientEmail, email) || existing.Plan.Code != plan.Code {
					return ClientResetResult{}, rejectClientReset("请求编号已用于其他重置操作，请刷新后重试")
				}
				return existing, nil
			}
			return ClientResetResult{}, rejectClientReset("保存重置订单失败")
		}
		if rows, rowsErr := inserted.RowsAffected(); rowsErr != nil {
			return ClientResetResult{}, rejectClientReset("读取重置订单状态失败")
		} else if rows != 1 {
			return ClientResetResult{}, rejectClientReset("该客户端已有待付款、执行中或待核对的订单，请先查询订单状态")
		}
		order, err = a.clientResetOrder(ctx, orderNo)
		if err != nil {
			return ClientResetResult{}, rejectClientReset("读取新建的支付订单失败")
		}
	}

	if order.Status != "PAYMENT_PENDING" || order.PaymentStatus != "PENDING" {
		return order, nil
	}
	if order.PaymentURL != "" {
		return order, nil
	}
	paymentURL, submitErr := a.LDC.CreatePayment(ctx, order.OrderNo, "客户端重置 "+order.Plan.Name, fmt.Sprintf("%d.00", order.Plan.PointsCost))
	if submitErr != nil {
		providerOrder, queryErr := a.LDC.QueryOrder(ctx, order.OrderNo)
		if queryErr == nil && providerOrder.Status == 0 {
			paymentURL = a.LDC.ResumePaymentURL(order.OrderNo)
		} else {
			return ClientResetResult{}, errors.New("LDC 支付页面创建失败，请稍后使用同一请求重试")
		}
	}
	if paymentURL == "" {
		return ClientResetResult{}, errors.New("LDC 未返回支付地址，请稍后使用同一请求重试")
	}
	if _, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_url=?,updated_at=? WHERE order_no=? AND status='PAYMENT_PENDING' AND payment_status='PENDING'`, paymentURL, a.nowMS(), order.OrderNo); err != nil {
		return ClientResetResult{}, errors.New("支付地址保存失败，请稍后查询同一订单")
	}
	return a.clientResetOrder(ctx, order.OrderNo)
}

func clientResetOrderNumber(now time.Time) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "C" + now.UTC().Format("20060102T150405") + "-" + strings.ToUpper(hex.EncodeToString(raw[:])), nil
}

func (a *App) ClientResetOrder(ctx context.Context, orderNo string) (ClientResetResult, error) {
	if len(orderNo) < 20 || len(orderNo) > 64 {
		return ClientResetResult{}, errors.New("重置订单不存在")
	}
	orderNo = strings.TrimSpace(orderNo)
	order, err := a.clientResetOrder(ctx, orderNo)
	if err != nil || a.LDC == nil || order.Status != "PAYMENT_PENDING" || order.PaymentStatus != "PENDING" {
		return order, err
	}
	checked := a.nowMS()
	threshold := checked - int64(a.Cfg.LDC.PollIntervalSeconds)*1000
	result, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_checked_at=? WHERE order_no=? AND status='PAYMENT_PENDING' AND payment_status='PENDING' AND (payment_checked_at=0 OR payment_checked_at<=?)`, checked, orderNo, threshold)
	if err != nil {
		return order, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return order, err
	}
	providerOrder, queryErr := a.LDC.QueryOrder(ctx, orderNo)
	if queryErr != nil {
		return order, nil
	}
	if providerOrder.Status == 1 {
		if err := a.ConfirmClientResetPayment(ctx, orderNo, providerOrder.Money, providerOrder.TradeNo); err != nil {
			latest, loadErr := a.clientResetOrder(ctx, orderNo)
			if loadErr == nil && latest.PaymentStatus == "SUCCESS" {
				return latest, nil
			}
			return order, err
		}
		return a.clientResetOrder(ctx, orderNo)
	}
	return a.clientResetOrder(ctx, orderNo)
}

func (a *App) ClientResetOrderForSubscriptionID(ctx context.Context, orderNo, subscriptionID string) (ClientResetResult, error) {
	subscriptionID, err := normalizeSubscriptionID(subscriptionID)
	if err != nil {
		return ClientResetResult{}, errors.New("无法查询订单")
	}
	client, err := a.Panel.GetClientBySubscriptionID(ctx, subscriptionID)
	if err != nil || strings.TrimSpace(client.Email) == "" {
		return ClientResetResult{}, errors.New("无法查询订单")
	}
	order, err := a.ClientResetOrder(ctx, orderNo)
	if err != nil || !strings.EqualFold(order.ClientEmail, client.Email) {
		return ClientResetResult{}, errors.New("无法查询订单")
	}
	return order, nil
}

// CancelClientResetOrderForSubscriptionID cancels only the local order. LDC's
// official API has no endpoint for closing an unpaid payment order, so any
// later payment is reconciled by the callback and payment polling worker.
func (a *App) CancelClientResetOrderForSubscriptionID(ctx context.Context, orderNo, subscriptionID string) (ClientResetResult, error) {
	subscriptionID, err := normalizeSubscriptionID(subscriptionID)
	if err != nil {
		return ClientResetResult{}, errors.New("无法取消订单")
	}
	client, err := a.Panel.GetClientBySubscriptionID(ctx, subscriptionID)
	if err != nil || strings.TrimSpace(client.Email) == "" {
		return ClientResetResult{}, errors.New("无法取消订单")
	}
	orderNo = strings.TrimSpace(orderNo)
	a.ldcMu.Lock()
	defer a.ldcMu.Unlock()

	order, err := a.clientResetOrder(ctx, orderNo)
	if err != nil || !strings.EqualFold(order.ClientEmail, client.Email) {
		return ClientResetResult{}, errors.New("无法取消订单")
	}
	if order.Status == "CANCELLED" && order.PaymentStatus == "PENDING" {
		return order, nil
	}
	if order.Status != "PAYMENT_PENDING" || order.PaymentStatus != "PENDING" {
		return order, nil
	}

	// Check once before cancelling to avoid racing a payment already visible to
	// LDC. If LDC is temporarily unavailable, the local cancellation can still
	// proceed; the worker keeps checking cancelled orders for late payments.
	if a.LDC != nil {
		providerOrder, queryErr := a.LDC.QueryOrder(ctx, orderNo)
		if queryErr == nil && providerOrder.Status == 1 {
			if confirmErr := a.ConfirmClientResetPayment(ctx, orderNo, providerOrder.Money, providerOrder.TradeNo); confirmErr != nil {
				latest, loadErr := a.clientResetOrder(ctx, orderNo)
				if loadErr == nil && latest.PaymentStatus != "PENDING" {
					return latest, nil
				}
				return ClientResetResult{}, confirmErr
			}
			return a.clientResetOrder(ctx, orderNo)
		}
	}

	message := "客户已取消本地订单。LDC 不提供关闭未付款订单的接口，旧支付页面仍可能完成付款；若之后收款，订单将转为待核对且不会自动充值。"
	result, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET status='CANCELLED',external_request_state='CANCELLED_BY_CLIENT',error_message=?,payment_checked_at=0,updated_at=? WHERE order_no=? AND status='PAYMENT_PENDING' AND payment_status='PENDING'`, message, a.nowMS(), orderNo)
	if err != nil {
		return ClientResetResult{}, errors.New("取消订单失败，请刷新后查询")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ClientResetResult{}, err
	}
	if changed != 1 {
		return a.clientResetOrder(ctx, orderNo)
	}
	return a.clientResetOrder(ctx, orderNo)
}

func (a *App) ConfirmClientResetPayment(ctx context.Context, orderNo, money, tradeNo string) error {
	order, err := a.clientResetOrder(ctx, orderNo)
	if err != nil {
		return errors.New("LDC 回调订单不存在")
	}
	actual, err := ldc.MoneyCents(money)
	if err != nil || actual != int64(order.Plan.PointsCost)*100 {
		_, _ = a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='AMOUNT_MISMATCH',payment_trade_no=?,payment_money=?,status='REVIEW_REQUIRED',error_message=?,updated_at=? WHERE order_no=? AND payment_status='PENDING'`, tradeNo, money, "LDC 支付金额与订单金额不一致，请管理员核对", a.nowMS(), orderNo)
		return errors.New("LDC 支付金额与订单不匹配")
	}
	if order.PaymentStatus == "SUCCESS" {
		return nil
	}
	if order.PaymentStatus != "PENDING" || (order.Status != "PAYMENT_PENDING" && order.Status != "CANCELLED") {
		return errors.New("LDC 回调对应的订单当前不可支付")
	}
	now := a.nowMS()
	newStatus := "PAYMENT_CONFIRMED"
	message := ""
	if order.Status == "CANCELLED" {
		newStatus = "REVIEW_REQUIRED"
		message = "客户取消本地订单后，LDC 仍确认收款；系统未自动充值，请管理员核对订单并决定手动确认或退款。"
	}
	result, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='SUCCESS',payment_trade_no=?,payment_money=?,status=?,error_message=?,payment_checked_at=?,updated_at=? WHERE order_no=? AND payment_status='PENDING' AND status=?`, tradeNo, money, newStatus, message, now, now, orderNo, order.Status)
	if err != nil {
		return fmt.Errorf("save LDC payment confirmation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		current, loadErr := a.clientResetOrder(ctx, orderNo)
		if loadErr == nil && current.PaymentStatus == "SUCCESS" {
			return nil
		}
		return errors.New("LDC 订单状态已变化，请稍后查询")
	}
	if newStatus == "PAYMENT_CONFIRMED" {
		a.schedulePaidClientReset(orderNo)
	}
	return nil
}

// ConfirmClientResetPaymentFromLDC treats the callback only as a notification
// that an order may have changed. Payment status and amount always come from a
// fresh server-to-server query to LDC.
func (a *App) ConfirmClientResetPaymentFromLDC(ctx context.Context, orderNo string) error {
	if a.LDC == nil {
		return errors.New("LDC 支付尚未配置")
	}
	order, err := a.clientResetOrder(ctx, orderNo)
	if err != nil {
		return errors.New("LDC 通知订单不存在")
	}
	if order.PaymentStatus == "SUCCESS" {
		return nil
	}
	if (order.Status != "PAYMENT_PENDING" && order.Status != "CANCELLED") || order.PaymentStatus != "PENDING" {
		return errors.New("LDC 通知对应的订单当前不可支付")
	}
	providerOrder, err := a.LDC.QueryOrder(ctx, orderNo)
	if err != nil {
		return fmt.Errorf("LDC 实时查单失败: %w", err)
	}
	if providerOrder.Status != 1 {
		return errors.New("LDC 查单尚未确认收款")
	}
	return a.ConfirmClientResetPayment(ctx, orderNo, providerOrder.Money, providerOrder.TradeNo)
}

func (a *App) schedulePaidClientReset(orderNo string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := a.ApplyPaidClientReset(ctx, orderNo)
		if err != nil {
			slog.Error("apply paid client reset", "order_no", orderNo, "error", err)
		} else if result.Status == "REVIEW_REQUIRED" || result.Status == "FAILED" {
			slog.Warn("paid client reset requires follow-up", "order_no", orderNo, "status", result.Status)
		}
	}()
}

func (a *App) ApplyPaidClientReset(ctx context.Context, orderNo string) (ClientResetResult, error) {
	a.ldcMu.Lock()
	defer a.ldcMu.Unlock()
	result, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET status='APPLYING',external_request_state='PREPARING',started_at=?,updated_at=? WHERE order_no=? AND status='PAYMENT_CONFIRMED' AND payment_status='SUCCESS'`, a.nowMS(), a.nowMS(), orderNo)
	if err != nil {
		return ClientResetResult{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ClientResetResult{}, err
	}
	if changed != 1 {
		current, loadErr := a.clientResetOrder(ctx, orderNo)
		if loadErr != nil {
			return ClientResetResult{}, loadErr
		}
		if current.PaymentStatus == "SUCCESS" {
			return current, nil
		}
		return ClientResetResult{}, errors.New("只有已支付订单可以执行客户端重置")
	}
	order, err := a.clientResetOrder(ctx, orderNo)
	if err != nil {
		return ClientResetResult{}, err
	}
	client, readErr := a.Panel.GetClient(ctx, order.ClientEmail)
	if readErr != nil || !strings.EqualFold(client.Email, order.ClientEmail) {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "PANEL_READ_FAILED", "已付款，但无法读取面板客户端；请管理员核对后处理", Snapshot{})
		return a.clientResetOrder(ctx, orderNo)
	}
	if !clientResetMatchesBefore(order.Before, client) {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "CLIENT_CHANGED_AFTER_PAYMENT", "已付款，但账号额度、期限或启用状态在付款期间发生变化；请管理员核对后处理", clientSnapshot(client))
		return a.clientResetOrder(ctx, orderNo)
	}
	if err := a.Panel.ValidateSetQuota(ctx, client, true, true); err != nil {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "NOT_SENT", "已付款，但面板额度设置能力当前不可用；请管理员核对后处理", clientSnapshot(client))
		return a.clientResetOrder(ctx, orderNo)
	}
	if err := a.Panel.ValidateResetTraffic(ctx); err != nil {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "NOT_SENT", "已付款，但面板流量重置能力当前不可用；请管理员核对后处理", clientSnapshot(client))
		return a.clientResetOrder(ctx, orderNo)
	}

	now := a.Now().UTC()
	expiry := now
	switch {
	case order.Plan.DurationDays > 0:
		expiry = now.AddDate(0, 0, order.Plan.DurationDays)
	case order.Plan.DurationMonths > 0:
		expiry = addCalendarMonths(now, order.Plan.DurationMonths)
	default:
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "NOT_SENT", "已付款，但订单套餐期限无效；请管理员核对后处理", clientSnapshot(client))
		return a.clientResetOrder(ctx, orderNo)
	}
	if expiry.UnixMilli() <= now.UnixMilli() {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "NOT_SENT", "已付款，但无法计算有效的续期时间；请管理员核对后处理", clientSnapshot(client))
		return a.clientResetOrder(ctx, orderNo)
	}
	expected := clientSnapshot(client)
	expected.ExpiryMS = expiry.UnixMilli()
	expected.TotalBytes = order.Plan.TrafficBytes
	expected.UsedBytes = 0
	expected.RemainingBytes = order.Plan.TrafficBytes
	expected.UploadBytes = 0
	expected.DownloadBytes = 0
	expected.HasTrafficBreakdown = true
	if expected.HasEnabled {
		expected.Enabled = true
	}
	expectedJSON, _ := json.Marshal(expected)
	started := a.nowMS()
	if _, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET expected_snapshot=?,external_request_state='REQUEST_MAY_BE_SENT',updated_at=? WHERE order_no=? AND status='APPLYING' AND payment_status='SUCCESS'`, string(expectedJSON), started, orderNo); err != nil {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "REQUEST_STATE_NOT_SAVED", "已付款，但无法记录面板操作状态；请管理员核对后处理", clientSnapshot(client))
		return a.clientResetOrder(ctx, orderNo)
	}

	setErr := a.Panel.SetQuota(ctx, client, expected.ExpiryMS, expected.TotalBytes, true, true)
	if errors.Is(setErr, panel.ErrWriteNotSent) {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "NOT_SENT", "已付款，但面板未接受额度更新请求；请联系管理员处理退款", clientSnapshot(client))
		return a.clientResetOrder(ctx, orderNo)
	}
	current, panelReadErr := a.Panel.GetClient(ctx, client.Email)
	if panel.IsDefinitiveWriteRejection(setErr) && panelReadErr == nil && clientResetMatchesBefore(clientSnapshot(client), current) {
		message := fmt.Sprintf("%s；面板状态与操作前一致，重置未执行；请联系管理员处理退款", safeError(setErr))
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "CONFIRMED_NOT_APPLIED", message, clientSnapshot(current))
		return a.clientResetOrder(ctx, orderNo)
	}
	if panelReadErr != nil || current.ExpiryMS != expected.ExpiryMS || current.TotalBytes != expected.TotalBytes {
		message := "已付款，但面板额度更新结果无法确认；请管理员核对"
		if setErr != nil {
			message = safeError(setErr) + "；已付款，面板额度更新结果无法确认，请管理员核对"
		}
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "OUTCOME_UNKNOWN", message, clientSnapshotIfAvailable(current, panelReadErr))
		return a.clientResetOrder(ctx, orderNo)
	}

	resetStarted := a.nowMS()
	if _, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET external_request_state='RESET_MAY_BE_SENT',updated_at=? WHERE order_no=? AND status='APPLYING'`, resetStarted, orderNo); err != nil {
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "RESET_NOT_SENT", "已付款且额度和期限已更新，但无法记录流量重置请求；请管理员核对", clientSnapshot(current))
		return a.clientResetOrder(ctx, orderNo)
	}
	resetErr := a.Panel.ResetTraffic(ctx, client.Email)
	afterClient, verifyErr := a.Panel.GetClient(ctx, client.Email)
	enabledMatches := !afterClient.HasEnabled || afterClient.Enabled
	if verifyErr == nil && afterClient.ExpiryMS == expected.ExpiryMS && afterClient.TotalBytes == expected.TotalBytes &&
		enabledMatches && ((resetErr == nil) || (afterClient.HasTrafficBreakdown && afterClient.UploadBytes == 0 && afterClient.DownloadBytes == 0)) {
		_ = a.finishClientReset(ctx, orderNo, "SUCCESS", "ACKNOWLEDGED_VERIFIED", "LDC 已收款，面板已确认流量重置", clientSnapshot(afterClient))
	} else {
		message := "已付款，但面板重置结果无法确认；请联系管理员核对，请勿重复提交"
		if resetErr != nil {
			message = safeError(resetErr) + "；已付款，请管理员核对，请勿重复提交"
		}
		if verifyErr != nil {
			message += "；复查面板状态失败"
		} else if afterClient.ExpiryMS != expected.ExpiryMS || afterClient.TotalBytes != expected.TotalBytes || !afterClient.HasTrafficBreakdown || afterClient.UploadBytes != 0 || afterClient.DownloadBytes != 0 {
			message += "；当前面板数据与预期不一致"
		}
		_ = a.finishClientReset(ctx, orderNo, "REVIEW_REQUIRED", "OUTCOME_UNKNOWN", message, clientSnapshotIfAvailable(afterClient, verifyErr))
	}
	return a.clientResetOrder(ctx, orderNo)
}

func (a *App) PollLDCPaymentOrders(ctx context.Context) error {
	if a.LDC == nil {
		return nil
	}
	var failures []error
	rows, err := a.DB.QueryContext(ctx, `SELECT order_no FROM client_reset_orders WHERE status='PAYMENT_CONFIRMED' AND payment_status='SUCCESS' ORDER BY created_at LIMIT 50`)
	if err != nil {
		return err
	}
	var confirmed []string
	for rows.Next() {
		var orderNo string
		if err := rows.Scan(&orderNo); err != nil {
			rows.Close()
			return err
		}
		confirmed = append(confirmed, orderNo)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, orderNo := range confirmed {
		if _, err := a.ApplyPaidClientReset(ctx, orderNo); err != nil {
			failures = append(failures, fmt.Errorf("apply paid client reset %s: %w", orderNo, err))
		}
	}

	cutoff := a.nowMS() - int64(a.Cfg.LDC.PollIntervalSeconds)*1000
	rows, err = a.DB.QueryContext(ctx, `SELECT order_no FROM client_reset_orders WHERE status IN ('PAYMENT_PENDING','CANCELLED') AND payment_status='PENDING' AND (payment_checked_at=0 OR payment_checked_at<=?) ORDER BY payment_checked_at,created_at LIMIT 50`, cutoff)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	var pending []string
	for rows.Next() {
		var orderNo string
		if err := rows.Scan(&orderNo); err != nil {
			rows.Close()
			return errors.Join(append(failures, err)...)
		}
		pending = append(pending, orderNo)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return errors.Join(append(failures, err)...)
	}
	rows.Close()
	for _, orderNo := range pending {
		checked := a.nowMS()
		_, _ = a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_checked_at=? WHERE order_no=? AND status IN ('PAYMENT_PENDING','CANCELLED') AND payment_status='PENDING'`, checked, orderNo)
		providerOrder, err := a.LDC.QueryOrder(ctx, orderNo)
		if err != nil {
			if !errors.Is(err, ldc.ErrOrderNotFound) {
				failures = append(failures, fmt.Errorf("query LDC order %s: %w", orderNo, err))
			}
			continue
		}
		if providerOrder.Status != 1 {
			continue
		}
		order, err := a.clientResetOrder(ctx, orderNo)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		actual, amountErr := ldc.MoneyCents(providerOrder.Money)
		if amountErr != nil || actual != int64(order.Plan.PointsCost)*100 {
			_, _ = a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='AMOUNT_MISMATCH',payment_trade_no=?,payment_money=?,status='REVIEW_REQUIRED',error_message=?,updated_at=? WHERE order_no=? AND payment_status='PENDING'`, providerOrder.TradeNo, providerOrder.Money, "LDC 查单金额与订单金额不一致，请管理员核对", a.nowMS(), orderNo)
			failures = append(failures, fmt.Errorf("LDC amount mismatch for %s", orderNo))
			continue
		}
		if err := a.ConfirmClientResetPayment(ctx, orderNo, providerOrder.Money, providerOrder.TradeNo); err != nil {
			failures = append(failures, fmt.Errorf("confirm LDC order %s: %w", orderNo, err))
		}
	}
	return errors.Join(failures...)
}

func (a *App) RunLDCPaymentWorker(ctx context.Context) {
	if a.LDC == nil {
		return
	}
	interval := time.Duration(a.Cfg.LDC.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		if err := a.PollLDCPaymentOrders(pollCtx); err != nil && ctx.Err() == nil {
			slog.Warn("LDC payment polling had errors", "error", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) RecoverIncompleteClientResetOrders(ctx context.Context) error {
	if _, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='REFUND_UNCERTAIN',error_message=?,updated_at=? WHERE payment_status='REFUNDING'`, "服务重启时 LDC 退款结果不确定，请管理员在 LDC 核对", a.nowMS()); err != nil {
		return err
	}
	message := "服务重启时重置订单尚未完成，系统未重发面板请求；请核对面板状态"
	_, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET status='REVIEW_REQUIRED',external_request_state='RECOVERED_AFTER_RESTART',error_message=?,finished_at=?,updated_at=? WHERE status IN ('RESERVED','APPLYING','VERIFYING')`, message, a.nowMS(), a.nowMS())
	return err
}

func (a *App) ListClientResetOrders(ctx context.Context, status, search string, limit, offset int) ([]ClientResetListItem, int, error) {
	where := " WHERE 1=1"
	args := make([]any, 0, 5)
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	if search = strings.TrimSpace(search); search != "" {
		where += " AND (order_no LIKE ? OR client_email LIKE ?)"
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern)
	}
	var total int
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_reset_orders`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := a.DB.QueryContext(ctx, `SELECT order_no,payment_trade_no,payment_money,client_email,plan_name,duration_days,duration_months,points_cost,payment_status,status,error_message,created_at FROM client_reset_orders`+where+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]ClientResetListItem, 0)
	for rows.Next() {
		var item ClientResetListItem
		if err := rows.Scan(&item.OrderNo, &item.PaymentTradeNo, &item.PaymentMoney, &item.ClientEmail, &item.PlanName, &item.DurationDays, &item.DurationMonths, &item.PointsCost, &item.PaymentStatus, &item.Status, &item.ErrorMessage, &item.CreatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (a *App) ConfirmClientResetNotApplied(ctx context.Context, adminID int64, ip, orderNo, note string) error {
	orderNo = strings.TrimSpace(orderNo)
	note = strings.TrimSpace(note)
	if len(note) == 0 || len(note) > 500 {
		return errors.New("请填写 1 至 500 个字符的核对说明")
	}
	var orderID int64
	var clientEmail, status, paymentStatus, beforeJSON, originalError string
	err := a.DB.QueryRowContext(ctx, `SELECT id,client_email,status,payment_status,before_snapshot,error_message FROM client_reset_orders WHERE order_no=?`, orderNo).Scan(&orderID, &clientEmail, &status, &paymentStatus, &beforeJSON, &originalError)
	if err != nil {
		return errors.New("重置订单不存在")
	}
	if status != "REVIEW_REQUIRED" {
		return errors.New("只有待核对的重置订单可以解除")
	}
	if paymentStatus == "SUCCESS" || paymentStatus == "AMOUNT_MISMATCH" {
		return errors.New("已付款订单请使用后台的全额退款操作")
	}
	if paymentStatus == "REFUNDING" || paymentStatus == "REFUND_UNCERTAIN" {
		return errors.New("退款状态处理中或待确认，请先完成退款核对")
	}
	var before Snapshot
	if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil || before.ExpiryMS <= 0 || before.TotalBytes <= 0 {
		return errors.New("订单缺少完整的操作前状态，不能安全解除")
	}
	client, err := a.Panel.GetClient(ctx, clientEmail)
	if err != nil || !strings.EqualFold(client.Email, clientEmail) {
		return errors.New("无法读取面板客户端，订单仍保持待核对")
	}
	if !clientResetMatchesBefore(before, client) {
		return errors.New("当前面板额度、期限或启用状态与操作前不同，不能解除；请先人工核实")
	}
	currentJSON, err := json.Marshal(clientSnapshot(client))
	if err != nil {
		return err
	}
	resolution := "管理员已核验面板状态与操作前一致，本次重置未执行，可重新提交。"
	if originalError != "" {
		resolution += " 原始错误：" + originalError
	}
	now := a.nowMS()
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE client_reset_orders SET status='FAILED',external_request_state='CONFIRMED_NOT_APPLIED',error_message=?,after_snapshot=?,finished_at=?,updated_at=? WHERE id=? AND status='REVIEW_REQUIRED'`, resolution, string(currentJSON), now, now, orderID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("订单状态已变化，请刷新后重试")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(admin_id,action,target_type,target_id,ip,details,created_at) VALUES(?,?,?,?,?,?,?)`, adminID, "client_reset.confirm_not_applied", "client_reset_order", orderNo, ip, note, now); err != nil {
		return err
	}
	return tx.Commit()
}

func clientResetMatchesBefore(before Snapshot, client panel.Client) bool {
	if before.ExpiryMS != client.ExpiryMS || before.TotalBytes != client.TotalBytes || before.HasEnabled != client.HasEnabled {
		return false
	}
	return !before.HasEnabled || before.Enabled == client.Enabled
}

func addCalendarMonths(from time.Time, months int) time.Time {
	year, month, day := from.Date()
	hour, minute, second := from.Clock()
	nextMonth := time.Date(year, month+time.Month(months), 1, hour, minute, second, from.Nanosecond(), from.Location())
	lastDay := nextMonth.AddDate(0, 1, -1).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(nextMonth.Year(), nextMonth.Month(), day, hour, minute, second, from.Nanosecond(), from.Location())
}

func (a *App) finishClientReset(ctx context.Context, orderNo, status, requestState, message string, after Snapshot) error {
	afterJSON := ""
	if after.HasEnabled || after.ExpiryMS != 0 || after.TotalBytes != 0 || after.UploadBytes != 0 || after.DownloadBytes != 0 || after.InboundCount > 0 {
		encoded, _ := json.Marshal(after)
		afterJSON = string(encoded)
	}
	now := a.nowMS()
	_, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET status=?,external_request_state=?,error_message=?,after_snapshot=?,finished_at=?,updated_at=? WHERE order_no=?`, status, requestState, message, afterJSON, now, now, orderNo)
	return err
}

func (a *App) clientResetOrderByRequestKey(ctx context.Context, requestKey string) (ClientResetResult, error) {
	var orderNo string
	err := a.DB.QueryRowContext(ctx, `SELECT order_no FROM client_reset_orders WHERE request_key=?`, requestKey).Scan(&orderNo)
	if err != nil {
		return ClientResetResult{}, err
	}
	return a.clientResetOrder(ctx, orderNo)
}

func (a *App) clientResetOrder(ctx context.Context, orderNo string) (ClientResetResult, error) {
	var result ClientResetResult
	var beforeJSON, expectedJSON, afterJSON string
	var planCode, planName string
	var trafficBytes int64
	var durationDays, durationMonths, pointsCost int
	err := a.DB.QueryRowContext(ctx, `SELECT order_no,status,client_email,plan_code,plan_name,duration_days,duration_months,traffic_bytes,points_cost,payment_status,payment_trade_no,payment_money,payment_url,before_snapshot,expected_snapshot,after_snapshot,error_message,created_at,updated_at FROM client_reset_orders WHERE order_no=?`, orderNo).Scan(&result.OrderNo, &result.Status, &result.ClientEmail, &planCode, &planName, &durationDays, &durationMonths, &trafficBytes, &pointsCost, &result.PaymentStatus, &result.PaymentTradeNo, &result.PaymentMoney, &result.PaymentURL, &beforeJSON, &expectedJSON, &afterJSON, &result.ErrorMessage, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return ClientResetResult{}, err
	}
	result.Plan = ClientResetPlan{Code: planCode, Name: planName, TrafficBytes: trafficBytes, PointsCost: pointsCost, DurationDays: durationDays, DurationMonths: durationMonths}
	if result.Status != "PAYMENT_PENDING" || result.PaymentStatus != "PENDING" {
		result.PaymentURL = ""
	}
	_ = json.Unmarshal([]byte(beforeJSON), &result.Before)
	_ = json.Unmarshal([]byte(expectedJSON), &result.Expected)
	_ = json.Unmarshal([]byte(afterJSON), &result.After)
	return result, nil
}
