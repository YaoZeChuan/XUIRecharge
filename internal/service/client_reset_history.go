package service

import (
	"context"
	"errors"
	"strings"
)

type ClientResetHistoryItem struct {
	OrderNo        string `json:"orderNo"`
	PlanName       string `json:"planName"`
	DurationDays   int    `json:"durationDays"`
	DurationMonths int    `json:"durationMonths"`
	TrafficBytes   int64  `json:"trafficBytes"`
	PointsCost     int    `json:"pointsCost"`
	PaymentMoney   string `json:"paymentMoney"`
	PaymentStatus  string `json:"paymentStatus"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"createdAt"`
}

type ClientResetHistory struct {
	Items        []ClientResetHistoryItem `json:"items"`
	Total        int                      `json:"total"`
	SuccessCount int                      `json:"successCount"`
	TotalPaid    int                      `json:"totalPaid"`
}

func (a *App) ClientResetHistory(ctx context.Context, subscriptionID string) (ClientResetHistory, error) {
	subscriptionID, err := normalizeSubscriptionID(subscriptionID)
	if err != nil {
		return ClientResetHistory{}, errors.New("订阅 ID 无效")
	}
	client, err := a.Panel.GetClientBySubscriptionID(ctx, subscriptionID)
	if err != nil || strings.TrimSpace(client.Email) == "" || len(client.InboundIDs) == 0 {
		return ClientResetHistory{}, ErrClientLookupRejected
	}
	var history ClientResetHistory
	err = a.DB.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status='SUCCESS' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN payment_status IN ('SUCCESS','REFUNDING','REFUND_UNCERTAIN') THEN points_cost ELSE 0 END),0)
		FROM client_reset_orders WHERE LOWER(client_email)=LOWER(?)`, client.Email).
		Scan(&history.Total, &history.SuccessCount, &history.TotalPaid)
	if err != nil {
		return ClientResetHistory{}, errors.New("读取账号订单统计失败")
	}
	rows, err := a.DB.QueryContext(ctx, `
		SELECT order_no,plan_name,duration_days,duration_months,traffic_bytes,points_cost,payment_money,payment_status,status,created_at
		FROM client_reset_orders WHERE LOWER(client_email)=LOWER(?) ORDER BY created_at DESC LIMIT 20`, client.Email)
	if err != nil {
		return ClientResetHistory{}, errors.New("读取账号订单记录失败")
	}
	defer rows.Close()
	history.Items = make([]ClientResetHistoryItem, 0, 20)
	for rows.Next() {
		var item ClientResetHistoryItem
		if err := rows.Scan(&item.OrderNo, &item.PlanName, &item.DurationDays, &item.DurationMonths, &item.TrafficBytes, &item.PointsCost, &item.PaymentMoney, &item.PaymentStatus, &item.Status, &item.CreatedAt); err != nil {
			return ClientResetHistory{}, errors.New("读取账号订单记录失败")
		}
		history.Items = append(history.Items, item)
	}
	if err := rows.Err(); err != nil {
		return ClientResetHistory{}, errors.New("读取账号订单记录失败")
	}
	return history, nil
}
