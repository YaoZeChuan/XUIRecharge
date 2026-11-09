package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"xui-recharge/internal/security"
)

type PackageInput struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	DurationDays int    `json:"durationDays"`
	TrafficBytes int64  `json:"trafficBytes"`
	Description  string `json:"description"`
	Enabled      bool   `json:"enabled"`
	SortOrder    int    `json:"sortOrder"`
}

type GenerateCardsInput struct {
	PackageID   int64  `json:"packageId"`
	Count       int    `json:"count"`
	ExpiresAt   int64  `json:"expiresAt"`
	ClientEmail string `json:"clientEmail"`
}

type GeneratedCards struct {
	BatchNo string   `json:"batchNo"`
	Cards   []string `json:"cards"`
}

type Dashboard struct {
	ResetTotal   int64 `json:"resetTotal"`
	ResetSuccess int64 `json:"resetSuccess"`
	ResetReview  int64 `json:"resetReview"`
	TodayResets  int64 `json:"todayResets"`
}

type AuditItem struct {
	ID         int64  `json:"id"`
	AdminID    int64  `json:"adminId"`
	Action     string `json:"action"`
	TargetType string `json:"targetType"`
	TargetID   string `json:"targetId"`
	IP         string `json:"ip"`
	Details    string `json:"details"`
	CreatedAt  int64  `json:"createdAt"`
}

func (a *App) ListPackages(ctx context.Context, includeDisabled bool) ([]Package, error) {
	query := `SELECT id,name,type,duration_days,traffic_bytes,description,enabled,sort_order,created_at,updated_at FROM packages`
	if !includeDisabled {
		query += ` WHERE enabled=1`
	}
	query += ` ORDER BY sort_order,id`
	rows, err := a.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	packages := make([]Package, 0)
	for rows.Next() {
		pkg, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		packages = append(packages, pkg)
	}
	return packages, rows.Err()
}

func scanPackage(row interface{ Scan(...any) error }) (Package, error) {
	var pkg Package
	var enabled int
	err := row.Scan(&pkg.ID, &pkg.Name, &pkg.Type, &pkg.DurationDays, &pkg.TrafficBytes, &pkg.Description, &enabled, &pkg.SortOrder, &pkg.CreatedAt, &pkg.UpdatedAt)
	pkg.Enabled = enabled == 1
	return pkg, err
}

func (a *App) SavePackage(ctx context.Context, adminID int64, ip string, id int64, input PackageInput) (Package, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Type = strings.ToUpper(strings.TrimSpace(input.Type))
	input.Description = strings.TrimSpace(input.Description)
	if err := a.validatePackage(input); err != nil {
		return Package{}, err
	}
	now := a.nowMS()
	if id == 0 {
		result, err := a.DB.ExecContext(ctx, `INSERT INTO packages(name,type,duration_days,traffic_bytes,description,enabled,sort_order,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, input.Name, input.Type, input.DurationDays, input.TrafficBytes, input.Description, boolInt(input.Enabled), input.SortOrder, now, now)
		if err != nil {
			return Package{}, err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return Package{}, err
		}
		_ = a.writeAudit(ctx, adminID, "package.create", "package", fmt.Sprint(id), ip, input.Name)
	} else {
		result, err := a.DB.ExecContext(ctx, `UPDATE packages SET name=?,type=?,duration_days=?,traffic_bytes=?,description=?,enabled=?,sort_order=?,updated_at=? WHERE id=?`, input.Name, input.Type, input.DurationDays, input.TrafficBytes, input.Description, boolInt(input.Enabled), input.SortOrder, now, id)
		if err != nil {
			return Package{}, err
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			return Package{}, sql.ErrNoRows
		}
		_ = a.writeAudit(ctx, adminID, "package.update", "package", fmt.Sprint(id), ip, input.Name)
	}
	return a.getPackage(ctx, id)
}

func (a *App) validatePackage(input PackageInput) error {
	if input.Name == "" || len([]rune(input.Name)) > 80 || len(input.Description) > 500 {
		return errors.New("套餐名称需为 1 至 80 个字符，备注不能超过 500 个字符")
	}
	if input.DurationDays < 0 || input.TrafficBytes < 0 || input.DurationDays > a.Cfg.Recharge.MaxDaysPerOrder || input.TrafficBytes > a.Cfg.Recharge.MaxTrafficGBPerOrder*gib {
		return errors.New("套餐额度超出系统允许范围")
	}
	switch input.Type {
	case "TIME":
		if input.DurationDays == 0 || input.TrafficBytes != 0 {
			return errors.New("时长套餐必须只设置大于 0 的天数")
		}
	case "TRAFFIC":
		if input.DurationDays != 0 || input.TrafficBytes == 0 {
			return errors.New("流量套餐必须只设置大于 0 的字节数")
		}
	case "COMBO":
		if input.DurationDays == 0 || input.TrafficBytes == 0 {
			return errors.New("组合套餐必须同时设置天数和流量")
		}
	default:
		return errors.New("套餐类型无效")
	}
	return nil
}

func (a *App) getPackage(ctx context.Context, id int64) (Package, error) {
	return scanPackage(a.DB.QueryRowContext(ctx, `SELECT id,name,type,duration_days,traffic_bytes,description,enabled,sort_order,created_at,updated_at FROM packages WHERE id=?`, id))
}

func (a *App) DisablePackage(ctx context.Context, adminID int64, ip string, id int64) error {
	result, err := a.DB.ExecContext(ctx, `UPDATE packages SET enabled=0,updated_at=? WHERE id=?`, a.nowMS(), id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return a.writeAudit(ctx, adminID, "package.disable", "package", fmt.Sprint(id), ip, "")
}

func (a *App) GenerateCards(ctx context.Context, adminID int64, ip string, input GenerateCardsInput) (GeneratedCards, error) {
	if input.Count < 1 || input.Count > 500 {
		return GeneratedCards{}, errors.New("一次最多生成 500 张卡密")
	}
	if input.ClientEmail != "" && (len(input.ClientEmail) > 254 || strings.ContainsAny(input.ClientEmail, "\r\n")) {
		return GeneratedCards{}, errors.New("绑定客户端标识无效")
	}
	if input.ExpiresAt > 0 && input.ExpiresAt <= a.nowMS() {
		return GeneratedCards{}, errors.New("卡密有效期必须晚于当前时间")
	}
	pkg, err := a.getPackage(ctx, input.PackageID)
	if err != nil || !pkg.Enabled {
		return GeneratedCards{}, errors.New("请选择已启用的套餐")
	}
	if len(a.Cfg.Security.CardHMACSecret) < 32 {
		return GeneratedCards{}, errors.New("CARD_HMAC_SECRET must contain at least 32 bytes")
	}
	packageJSON, err := json.Marshal(pkg)
	if err != nil {
		return GeneratedCards{}, err
	}
	batchToken, _, err := security.NewToken()
	if err != nil {
		return GeneratedCards{}, err
	}
	batchNo := "B" + time.UnixMilli(a.nowMS()).UTC().Format("20060102") + "-" + strings.ToUpper(batchToken[:8])
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return GeneratedCards{}, err
	}
	defer tx.Rollback()
	cards := make([]string, 0, input.Count)
	now := a.nowMS()
	for i := 0; i < input.Count; i++ {
		card, err := security.GenerateCard()
		if err != nil {
			return GeneratedCards{}, err
		}
		hash, err := security.CardHash(a.Cfg.Security.CardHMACSecret, card)
		if err != nil {
			return GeneratedCards{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recharge_cards(card_hash,card_mask,package_id,package_snapshot,status,bound_client_email,expires_at,batch_no,created_at,updated_at) VALUES(?,?,?,?, 'UNUSED',?,?,?,?,?)`, hash, security.MaskCard(card), pkg.ID, string(packageJSON), input.ClientEmail, input.ExpiresAt, batchNo, now, now); err != nil {
			return GeneratedCards{}, fmt.Errorf("save generated card: %w", err)
		}
		cards = append(cards, card)
	}
	if err := tx.Commit(); err != nil {
		return GeneratedCards{}, err
	}
	_ = a.writeAudit(ctx, adminID, "card.generate", "card_batch", batchNo, ip, fmt.Sprintf("count=%d package_id=%d", input.Count, pkg.ID))
	return GeneratedCards{BatchNo: batchNo, Cards: cards}, nil
}

func (a *App) ListCards(ctx context.Context, status string, packageID int64, search string, limit, offset int) ([]CardListItem, int64, error) {
	where := []string{"1=1"}
	args := []any{}
	if status != "" {
		if !validCardStatus(status) {
			return nil, 0, errors.New("invalid card status")
		}
		where = append(where, "c.status=?")
		args = append(args, status)
	}
	if packageID > 0 {
		where = append(where, "c.package_id=?")
		args = append(args, packageID)
	}
	if strings.TrimSpace(search) != "" {
		search = strings.TrimSpace(search)
		if hashes, err := a.cardHashes(search); err == nil {
			cardMatch := make([]string, 0, len(hashes))
			for _, hash := range hashes {
				cardMatch = append(cardMatch, "c.card_hash=?")
				args = append(args, hash)
			}
			where = append(where, "("+strings.Join(cardMatch, " OR ")+" OR c.bound_client_email LIKE ? OR c.batch_no LIKE ?)")
			args = append(args, "%"+search+"%", "%"+search+"%")
		} else {
			where = append(where, "(c.bound_client_email LIKE ? OR c.batch_no LIKE ?)")
			args = append(args, "%"+search+"%", "%"+search+"%")
		}
	}
	filter := strings.Join(where, " AND ")
	var total int64
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM recharge_cards c WHERE `+filter, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := a.DB.QueryContext(ctx, `SELECT c.id,c.card_mask,p.name,c.status,c.bound_client_email,c.expires_at,c.used_at,c.created_at,c.batch_no FROM recharge_cards c JOIN packages p ON p.id=c.package_id WHERE `+filter+` ORDER BY c.created_at DESC,c.id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]CardListItem, 0)
	for rows.Next() {
		var item CardListItem
		if err := rows.Scan(&item.ID, &item.Mask, &item.PackageName, &item.Status, &item.BoundEmail, &item.ExpiresAt, &item.UsedAt, &item.CreatedAt, &item.BatchNo); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func validCardStatus(value string) bool {
	switch value {
	case "UNUSED", "PROCESSING", "USED", "DISABLED", "EXPIRED", "REVIEW":
		return true
	default:
		return false
	}
}

func (a *App) DisableCard(ctx context.Context, adminID int64, ip string, cardID int64) error {
	result, err := a.DB.ExecContext(ctx, `UPDATE recharge_cards SET status='DISABLED',updated_at=? WHERE id=? AND status='UNUSED'`, a.nowMS(), cardID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return errors.New("仅未使用的卡密可以禁用")
	}
	return a.writeAudit(ctx, adminID, "card.disable", "card", fmt.Sprint(cardID), ip, "")
}

func (a *App) Dashboard(ctx context.Context) (Dashboard, error) {
	var result Dashboard
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(status='SUCCESS'),0),COALESCE(SUM(status='REVIEW_REQUIRED'),0) FROM client_reset_orders`).Scan(&result.ResetTotal, &result.ResetSuccess, &result.ResetReview); err != nil {
		return result, err
	}
	loc, err := time.LoadLocation(a.Cfg.Server.Timezone)
	if err != nil {
		loc = time.UTC
	}
	localNow := a.Now().In(loc)
	startOfDay := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc).UTC().UnixMilli()
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_reset_orders WHERE created_at>=?`, startOfDay).Scan(&result.TodayResets); err != nil {
		return result, err
	}
	return result, nil
}

func (a *App) ListOrders(ctx context.Context, status, search string, limit, offset int) ([]OrderListItem, int64, error) {
	where := []string{"1=1"}
	args := []any{}
	if status != "" {
		where = append(where, "o.status=?")
		args = append(args, status)
	}
	if search = strings.TrimSpace(search); search != "" {
		where = append(where, "(o.order_no LIKE ? OR o.client_email LIKE ? OR c.card_mask LIKE ?)")
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern, pattern)
	}
	filter := strings.Join(where, " AND ")
	var total int64
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM recharge_orders o JOIN recharge_cards c ON c.id=o.card_id WHERE `+filter, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := a.DB.QueryContext(ctx, `SELECT o.order_no,o.status,o.client_email,c.card_mask,json_extract(c.package_snapshot,'$.name'),o.duration_days,o.traffic_bytes,o.error_message,o.created_at,o.updated_at FROM recharge_orders o JOIN recharge_cards c ON c.id=o.card_id WHERE `+filter+` ORDER BY o.created_at DESC,o.id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]OrderListItem, 0)
	for rows.Next() {
		var item OrderListItem
		if err := rows.Scan(&item.OrderNo, &item.Status, &item.ClientEmail, &item.CardMask, &item.PackageName, &item.DurationDays, &item.TrafficBytes, &item.ErrorMessage, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (a *App) AdminOrder(ctx context.Context, orderNo string) (OrderResult, []map[string]any, error) {
	result, err := a.orderResultByNo(ctx, orderNo, "")
	if err != nil {
		return OrderResult{}, nil, err
	}
	rows, err := a.DB.QueryContext(ctx, `SELECT event_type,event_status,details,created_at FROM recharge_order_events WHERE order_id=(SELECT id FROM recharge_orders WHERE order_no=?) ORDER BY id`, orderNo)
	if err != nil {
		return OrderResult{}, nil, err
	}
	defer rows.Close()
	events := make([]map[string]any, 0)
	for rows.Next() {
		var typ, status, details string
		var created int64
		if err := rows.Scan(&typ, &status, &details, &created); err != nil {
			return OrderResult{}, nil, err
		}
		events = append(events, map[string]any{"type": typ, "status": status, "details": details, "createdAt": created})
	}
	return result, events, rows.Err()
}

func (a *App) Reconcile(ctx context.Context, adminID int64, ip, orderNo, action, note string) error {
	if len(note) > 500 {
		return errors.New("核对说明不能超过 500 个字符")
	}
	var orderID, cardID int64
	var clientEmail, status, beforeJSON, expectedJSON string
	err := a.DB.QueryRowContext(ctx, `SELECT id,card_id,client_email,status,before_snapshot,expected_snapshot FROM recharge_orders WHERE order_no=?`, orderNo).Scan(&orderID, &cardID, &clientEmail, &status, &beforeJSON, &expectedJSON)
	if err != nil {
		return err
	}
	if status != "REVIEW_REQUIRED" {
		return errors.New("只有待人工核对的订单可以处理")
	}
	client, err := a.Panel.GetClient(ctx, clientEmail)
	if err != nil {
		return errors.New("无法读取面板客户端状态，订单仍保持待核对")
	}
	var before, expected Snapshot
	if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(expectedJSON), &expected); err != nil {
		return err
	}
	current := clientSnapshot(client)
	var newStatus, cardStatus string
	switch action {
	case "mark_done":
		if !snapshotMatches(expected, client) {
			return errors.New("当前面板额度与订单预期不一致，不能标记成功")
		}
		newStatus, cardStatus = "SUCCESS", "USED"
	case "mark_unexecuted":
		if !snapshotMatches(before, client) {
			return errors.New("当前面板额度已变化，不能释放卡密")
		}
		newStatus, cardStatus = "FAILED", "UNUSED"
	case "close":
		if strings.TrimSpace(note) == "" {
			return errors.New("关闭异常订单时必须填写处理说明")
		}
		newStatus, cardStatus = "CLOSED", "REVIEW"
	default:
		return errors.New("核对动作无效")
	}
	now := a.nowMS()
	afterJSON, _ := json.Marshal(current)
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE recharge_orders SET status=?,external_request_state='MANUALLY_RECONCILED',error_message=?,after_snapshot=?,finished_at=?,updated_at=? WHERE id=? AND status='REVIEW_REQUIRED'`, newStatus, strings.TrimSpace(note), string(afterJSON), now, now, orderID); err != nil {
		return err
	}
	if cardStatus == "USED" {
		_, err = tx.ExecContext(ctx, `UPDATE recharge_cards SET status='USED',used_at=?,updated_at=? WHERE id=?`, now, now, cardID)
	} else if cardStatus == "UNUSED" {
		_, err = tx.ExecContext(ctx, `UPDATE recharge_cards SET status='UNUSED',order_id=NULL,updated_at=? WHERE id=?`, now, cardID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE recharge_cards SET status='REVIEW',updated_at=? WHERE id=?`, now, cardID)
	}
	if err != nil {
		return err
	}
	if err := a.recordEvent(ctx, tx, orderID, "MANUAL_RECONCILE", newStatus, strings.TrimSpace(note)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(admin_id,action,target_type,target_id,ip,details,created_at) VALUES(?,?,?,?,?,?,?)`, adminID, "order.reconcile", "order", orderNo, ip, action+": "+strings.TrimSpace(note), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) ListAudit(ctx context.Context, limit, offset int) ([]AuditItem, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT id,COALESCE(admin_id,0),action,target_type,target_id,ip,details,created_at FROM audit_logs ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AuditItem, 0)
	for rows.Next() {
		var item AuditItem
		if err := rows.Scan(&item.ID, &item.AdminID, &item.Action, &item.TargetType, &item.TargetID, &item.IP, &item.Details, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
