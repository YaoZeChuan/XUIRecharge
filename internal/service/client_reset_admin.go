package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"xui-recharge/internal/ldc"
	"xui-recharge/internal/panel"
)

func (a *App) AdminConfirmClientResetPayment(ctx context.Context, adminID int64, ip, orderNo, tradeNo, note string) error {
	orderNo = strings.TrimSpace(orderNo)
	tradeNo = strings.TrimSpace(tradeNo)
	note = strings.TrimSpace(note)
	if tradeNo == "" || len(tradeNo) > 128 || len(note) == 0 || len(note) > 500 ||
		!strings.Contains(strings.ToLower(note), "ldc") || !strings.Contains(note, "收款") {
		return errors.New("请提供 LDC 交易号，并填写包含“LDC 收款”的核对说明")
	}
	order, err := a.clientResetOrder(ctx, orderNo)
	if err != nil || order.Status != "PAYMENT_PENDING" || order.PaymentStatus != "PENDING" {
		return errors.New("只有等待付款的订单可以手动确认收款")
	}
	money := fmt.Sprintf("%d.00", order.Plan.PointsCost)
	now := a.nowMS()
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='SUCCESS',payment_trade_no=?,payment_money=?,status='PAYMENT_CONFIRMED',payment_checked_at=?,updated_at=? WHERE order_no=? AND status='PAYMENT_PENDING' AND payment_status='PENDING'`, tradeNo, money, now, now, orderNo)
	if err != nil {
		return err
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
		return errors.New("订单状态已变化，请刷新后重试")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(admin_id,action,target_type,target_id,ip,details,created_at) VALUES(?,?,?,?,?,?,?)`, adminID, "client_reset.confirm_payment", "client_reset_order", orderNo, ip, note+"；LDC交易号："+tradeNo, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.schedulePaidClientReset(orderNo)
	return nil
}

func (a *App) AdminConfirmClientResetSuccess(ctx context.Context, adminID int64, ip, orderNo, note string) error {
	a.ldcMu.Lock()
	defer a.ldcMu.Unlock()
	note = strings.TrimSpace(note)
	if len(note) == 0 || len(note) > 500 {
		return errors.New("请填写 1 至 500 个字符的核对说明")
	}
	order, err := a.clientResetOrder(ctx, strings.TrimSpace(orderNo))
	if err != nil || order.Status != "REVIEW_REQUIRED" {
		return errors.New("只有待核对订单可以手动确认重置成功")
	}
	if order.PaymentStatus != "SUCCESS" {
		return errors.New("只有金额正确且已付款的订单可以确认重置成功")
	}
	client, err := a.Panel.GetClient(ctx, order.ClientEmail)
	if err != nil || !strings.EqualFold(client.Email, order.ClientEmail) {
		return errors.New("无法读取面板客户端，订单仍保持待核对")
	}
	if !clientResetMatchesExpected(order.Expected, client) {
		return errors.New("当前面板状态未达到订单预期，不能确认重置成功")
	}
	return a.finishAdminClientResetReview(ctx, adminID, ip, order, note, "client_reset.confirm_applied", "SUCCESS", "MANUALLY_CONFIRMED", "管理员已核验面板状态与订单预期一致，手动确认重置成功。", clientSnapshot(client), "SUCCESS", "SUCCESS")
}

func (a *App) AdminRefundClientReset(ctx context.Context, adminID int64, ip, orderNo, note string) error {
	note = strings.TrimSpace(note)
	if len(note) == 0 || len(note) > 500 {
		return errors.New("请填写 1 至 500 个字符的退款说明")
	}
	if a.LDC == nil {
		return errors.New("LDC 支付未配置，无法通过服务发起退款")
	}
	a.ldcMu.Lock()
	defer a.ldcMu.Unlock()

	order, err := a.clientResetOrder(ctx, strings.TrimSpace(orderNo))
	if err != nil {
		return errors.New("重置订单不存在")
	}
	if order.PaymentStatus != "SUCCESS" && order.PaymentStatus != "AMOUNT_MISMATCH" {
		return errors.New("订单当前没有可退款的已确认付款")
	}
	if order.PaymentTradeNo == "" {
		return errors.New("订单缺少 LDC 交易号，无法发起退款")
	}
	if _, err := ldc.MoneyCents(order.PaymentMoney); err != nil {
		return errors.New("订单缺少有效的实际付款金额，无法发起全额退款")
	}
	claim, err := a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='REFUNDING',updated_at=? WHERE order_no=? AND payment_status=?`, a.nowMS(), order.OrderNo, order.PaymentStatus)
	if err != nil {
		return err
	}
	claimed, err := claim.RowsAffected()
	if err != nil {
		return err
	}
	if claimed != 1 {
		return errors.New("订单支付状态已变化，请刷新后重试")
	}
	if err := a.LDC.Refund(ctx, order.PaymentTradeNo, order.PaymentMoney); err != nil {
		var uncertain *ldc.RefundOutcomeUnknownError
		if errors.As(err, &uncertain) {
			_, _ = a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='REFUND_UNCERTAIN',error_message=?,updated_at=? WHERE order_no=? AND payment_status='REFUNDING'`, "LDC 退款请求结果不确定；请在 LDC 核对后手动确认", a.nowMS(), order.OrderNo)
			_ = a.writeAudit(ctx, adminID, "client_reset.refund_uncertain", "client_reset_order", order.OrderNo, ip, note+"；"+safeError(err))
			return errors.New("LDC 退款结果不确定，请在 LDC 控制台核对退款状态后再确认")
		}
		_, _ = a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status=?,error_message=?,updated_at=? WHERE order_no=? AND payment_status='REFUNDING'`, order.PaymentStatus, "LDC 退款失败："+safeError(err), a.nowMS(), order.OrderNo)
		_ = a.writeAudit(ctx, adminID, "client_reset.refund_failed", "client_reset_order", order.OrderNo, ip, note+"；"+safeError(err))
		return fmt.Errorf("LDC 退款失败：%w", err)
	}

	current, currentErr := a.Panel.GetClient(ctx, order.ClientEmail)
	unchanged := currentErr == nil && strings.EqualFold(current.Email, order.ClientEmail) && clientResetMatchesBefore(order.Before, current)
	status := "REVIEW_REQUIRED"
	message := "LDC 全额退款成功；订单仍需管理员核对面板状态。"
	requestState := "REFUNDED_PANEL_REVIEW"
	if unchanged {
		status = "FAILED"
		message = "LDC 全额退款成功；面板状态与操作前一致，客户可以重新提交。"
		requestState = "REFUNDED_NOT_APPLIED"
	}
	if err := a.finishAdminClientResetReview(ctx, adminID, ip, order, note, "client_reset.refund", status, requestState, message, clientSnapshotIfAvailable(current, currentErr), "REFUNDING", "REFUNDED"); err != nil {
		_, _ = a.DB.ExecContext(ctx, `UPDATE client_reset_orders SET payment_status='REFUND_UNCERTAIN',error_message=?,updated_at=? WHERE order_no=? AND payment_status='REFUNDING'`, "LDC 已退款，但本地状态保存失败；请管理员在 LDC 核验后确认", a.nowMS(), order.OrderNo)
		return errors.New("LDC 已退款，但本地订单状态保存失败；请管理员核对退款流水和订单")
	}
	return nil
}

func (a *App) AdminConfirmClientResetRefunded(ctx context.Context, adminID int64, ip, orderNo, note string) error {
	a.ldcMu.Lock()
	defer a.ldcMu.Unlock()
	note = strings.TrimSpace(note)
	if len(note) == 0 || len(note) > 500 || !strings.Contains(strings.ToLower(note), "ldc") || !strings.Contains(note, "退款") {
		return errors.New("请先在 LDC 核验已全额退款，并填写包含“LDC 退款”的核对说明")
	}
	order, err := a.clientResetOrder(ctx, strings.TrimSpace(orderNo))
	if err != nil || order.PaymentStatus != "REFUND_UNCERTAIN" {
		return errors.New("只有退款结果不确定的订单可以手动确认退款")
	}
	client, panelErr := a.Panel.GetClient(ctx, order.ClientEmail)
	unchanged := panelErr == nil && strings.EqualFold(client.Email, order.ClientEmail) && clientResetMatchesBefore(order.Before, client)
	status := "REVIEW_REQUIRED"
	message := "管理员已在 LDC 核验退款；面板状态仍需进一步核对。"
	requestState := "REFUND_MANUALLY_CONFIRMED"
	if unchanged {
		status = "FAILED"
		message = "管理员已在 LDC 核验全额退款，并确认面板状态与操作前一致；客户可以重新提交。"
		requestState = "REFUNDED_NOT_APPLIED"
	}
	return a.finishAdminClientResetReview(ctx, adminID, ip, order, note, "client_reset.confirm_refund", status, requestState, message, clientSnapshotIfAvailable(client, panelErr), "REFUND_UNCERTAIN", "REFUNDED")
}

func (a *App) finishAdminClientResetReview(ctx context.Context, adminID int64, ip string, order ClientResetResult, note, action, status, requestState, message string, after Snapshot, expectedPaymentStatus, paymentStatus string) error {
	afterJSON := ""
	if after.HasEnabled || after.ExpiryMS != 0 || after.TotalBytes != 0 || after.InboundCount > 0 {
		encoded, _ := json.Marshal(after)
		afterJSON = string(encoded)
	}
	now := a.nowMS()
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE client_reset_orders SET status=?,payment_status=?,external_request_state=?,error_message=?,after_snapshot=?,finished_at=?,updated_at=? WHERE order_no=? AND status=? AND payment_status=?`, status, paymentStatus, requestState, message, afterJSON, now, now, order.OrderNo, order.Status, expectedPaymentStatus)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(admin_id,action,target_type,target_id,ip,details,created_at) VALUES(?,?,?,?,?,?,?)`, adminID, action, "client_reset_order", order.OrderNo, ip, note, now); err != nil {
		return err
	}
	return tx.Commit()
}

func clientResetMatchesExpected(expected Snapshot, client panel.Client) bool {
	if expected.ExpiryMS != client.ExpiryMS || expected.TotalBytes != client.TotalBytes || expected.HasEnabled != client.HasEnabled {
		return false
	}
	if expected.HasEnabled && expected.Enabled != client.Enabled {
		return false
	}
	return client.HasTrafficBreakdown && client.UploadBytes == 0 && client.DownloadBytes == 0
}
