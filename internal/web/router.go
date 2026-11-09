package web

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"xui-recharge/internal/config"
	"xui-recharge/internal/security"
	"xui-recharge/internal/service"
	ui "xui-recharge/web"
)

type response struct {
	Success   bool   `json:"success"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	RequestID string `json:"requestId"`
}

type apiServer struct {
	app    *service.App
	cfg    config.Config
	logger *slog.Logger
}

func NewRouter(app *service.App, cfg config.Config, logger *slog.Logger) *gin.Engine {
	if cfg.AppEnv == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	_ = r.SetTrustedProxies(cfg.Server.TrustedProxies)
	r.Use(gin.Recovery(), requestContext(logger), securityHeaders(cfg), accessLog(logger))
	api := &apiServer{app: app, cfg: cfg, logger: logger}

	r.GET("/api/healthz", func(c *gin.Context) {
		ok := true
		if err := app.DB.PingContext(c.Request.Context()); err != nil {
			ok = false
		}
		if !ok {
			write(c, http.StatusServiceUnavailable, "UNHEALTHY", "service unavailable", nil)
			return
		}
		write(c, http.StatusOK, "OK", "运行正常", gin.H{"status": "ok"})
	})

	userPage, _ := ui.Files.ReadFile("user/index.html")
	adminPage, _ := ui.Files.ReadFile("admin/index.html")
	r.GET("/", func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", userPage) })
	r.GET("/admin", func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", adminPage) })
	r.GET("/admin/*path", func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", adminPage) })
	assets, _ := fs.Sub(ui.Files, "assets")
	r.StaticFS("/assets", http.FS(assets))

	public := r.Group("/api/public")
	public.Use(rateLimit(30, 5*time.Minute))
	public.GET("/config", api.publicConfig)
	public.POST("/clients/lookup", rateLimit(12, 5*time.Minute), api.lookupClient)
	public.POST("/client-reset/history", rateLimit(12, 5*time.Minute), api.clientResetHistory)
	public.GET("/client-reset/plans", api.clientResetPlans)
	public.POST("/client-reset", rateLimit(5, time.Minute), api.resetClient)
	public.POST("/client-reset/orders/:orderNo", rateLimit(30, time.Minute), api.clientResetOrder)
	public.POST("/client-reset/orders/:orderNo/cancel", rateLimit(12, 5*time.Minute), api.cancelClientResetOrder)
	r.GET("/api/public/ldc/notify", api.ldcNotify)

	adminAuth := r.Group("/api/admin/auth")
	adminAuth.Use(rateLimit(12, 5*time.Minute))
	adminAuth.POST("/login", api.login)

	admin := r.Group("/api/admin")
	admin.Use(api.requireAdmin())
	admin.POST("/auth/logout", api.logout)
	admin.GET("/auth/me", api.me)
	admin.POST("/auth/change-password", api.changePassword)
	admin.GET("/dashboard", api.dashboard)
	admin.GET("/client-resets", api.listClientResets)
	admin.POST("/client-resets/:orderNo/reconcile", api.reconcileClientReset)
	admin.POST("/client-resets/:orderNo/confirm-payment", api.confirmClientResetPayment)
	admin.POST("/client-resets/:orderNo/confirm-success", api.confirmClientResetSuccess)
	admin.POST("/client-resets/:orderNo/refund", api.refundClientReset)
	admin.POST("/client-resets/:orderNo/confirm-refund", api.confirmClientResetRefunded)
	admin.GET("/clients", api.listClients)
	admin.GET("/panel/status", api.panelStatus)
	admin.POST("/panel/test", api.panelStatus)
	admin.GET("/settings", api.settings)
	admin.PUT("/settings", api.updateSettings)
	admin.POST("/backup", api.backup)
	admin.GET("/audit-logs", api.auditLogs)

	return r
}

func (s *apiServer) publicConfig(c *gin.Context) {
	write(c, http.StatusOK, "OK", "获取成功", gin.H{"timezone": s.cfg.Server.Timezone, "title": "FLOW 流量充值中心"})
}

func (s *apiServer) lookupClient(c *gin.Context) {
	var input struct {
		SubscriptionID string `json:"subscriptionId"`
	}
	if !bind(c, &input) {
		return
	}
	result, err := s.app.LookupClient(c.Request.Context(), input.SubscriptionID)
	if err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "账号查询成功", result)
}

func (s *apiServer) clientResetPlans(c *gin.Context) {
	write(c, http.StatusOK, "OK", "获取成功", gin.H{"items": service.ClientResetPlans(), "durationMonths": 1, "paymentEnabled": s.app.LDC != nil})
}

func (s *apiServer) clientResetHistory(c *gin.Context) {
	var input struct {
		SubscriptionID string `json:"subscriptionId"`
	}
	if !bind(c, &input) {
		return
	}
	result, err := s.app.ClientResetHistory(c.Request.Context(), input.SubscriptionID)
	if err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "获取成功", result)
}

func (s *apiServer) resetClient(c *gin.Context) {
	var input struct {
		SubscriptionID string `json:"subscriptionId"`
		PlanCode       string `json:"planCode"`
		RequestKey     string `json:"requestKey"`
	}
	if !bind(c, &input) {
		return
	}
	result, err := s.app.CreateClientResetCheckout(c.Request.Context(), input.SubscriptionID, input.PlanCode, input.RequestKey)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, service.ErrClientResetRejected) {
			status = http.StatusBadRequest
		}
		writeError(c, err, status)
		return
	}
	write(c, http.StatusOK, "OK", "支付订单已创建", result)
}

func (s *apiServer) clientResetOrder(c *gin.Context) {
	var input struct {
		SubscriptionID string `json:"subscriptionId"`
	}
	if !bind(c, &input) {
		return
	}
	result, err := s.app.ClientResetOrderForSubscriptionID(c.Request.Context(), c.Param("orderNo"), input.SubscriptionID)
	if err != nil {
		write(c, http.StatusNotFound, "NOT_FOUND", "订单不存在", nil)
		return
	}
	write(c, http.StatusOK, "OK", "获取成功", result)
}

func (s *apiServer) cancelClientResetOrder(c *gin.Context) {
	var input struct {
		SubscriptionID string `json:"subscriptionId"`
	}
	if !bind(c, &input) {
		return
	}
	result, err := s.app.CancelClientResetOrderForSubscriptionID(c.Request.Context(), c.Param("orderNo"), input.SubscriptionID)
	if err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "订单状态已更新", result)
}

func (s *apiServer) ldcNotify(c *gin.Context) {
	if s.app.LDC == nil {
		c.String(http.StatusServiceUnavailable, "fail")
		return
	}
	orderNumbers := c.Request.URL.Query()["out_trade_no"]
	if len(orderNumbers) != 1 || len(strings.TrimSpace(orderNumbers[0])) < 20 || len(strings.TrimSpace(orderNumbers[0])) > 64 {
		s.logger.Warn("reject LDC payment notification without a valid order number")
		c.String(http.StatusBadRequest, "fail")
		return
	}
	orderNo := strings.TrimSpace(orderNumbers[0])
	err := s.app.ConfirmClientResetPaymentFromLDC(c.Request.Context(), orderNo)
	if err != nil {
		order, readErr := s.app.ClientResetOrder(c.Request.Context(), orderNo)
		if readErr != nil || order.PaymentStatus != "SUCCESS" {
			s.logger.Error("process LDC payment notification", "order_no", orderNo, "error", err)
			c.String(http.StatusInternalServerError, "fail")
			return
		}
	}
	c.String(http.StatusOK, "success")
}

func (s *apiServer) login(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !bind(c, &input) {
		return
	}
	result, err := s.app.Login(c.Request.Context(), input.Username, input.Password, c.ClientIP())
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, service.ErrLoginRateLimited) {
			status = http.StatusTooManyRequests
		}
		writeError(c, err, status)
		return
	}
	secure := s.cfg.AppEnv == "production"
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("admin_session", result.Session, s.cfg.Security.AdminSessionHours*3600, "/", "", secure, true)
	c.SetCookie("admin_csrf", result.CSRF, s.cfg.Security.AdminSessionHours*3600, "/", "", secure, false)
	write(c, http.StatusOK, "OK", "登录成功", gin.H{"admin": result.Admin, "csrfToken": result.CSRF, "expiresAt": result.ExpiresAt})
}

func (s *apiServer) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		session, _ := c.Cookie("admin_session")
		csrfCookie, _ := c.Cookie("admin_csrf")
		unsafe := c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions
		admin, err := s.app.AuthenticateSession(c.Request.Context(), session, csrfCookie, c.GetHeader("X-CSRF-Token"), unsafe)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, service.ErrCSRF) {
				status = http.StatusForbidden
			}
			writeError(c, err, status)
			c.Abort()
			return
		}
		c.Set("admin", admin)
		c.Next()
	}
}

func (s *apiServer) logout(c *gin.Context) {
	session, _ := c.Cookie("admin_session")
	if err := s.app.Logout(c.Request.Context(), session); err != nil {
		writeError(c, err, http.StatusInternalServerError)
		return
	}
	secure := s.cfg.AppEnv == "production"
	c.SetCookie("admin_session", "", -1, "/", "", secure, true)
	c.SetCookie("admin_csrf", "", -1, "/", "", secure, false)
	write(c, http.StatusOK, "OK", "已退出登录", nil)
}

func (s *apiServer) me(c *gin.Context) {
	admin, _ := c.Get("admin")
	write(c, http.StatusOK, "OK", "登录有效", gin.H{"admin": admin})
}

func (s *apiServer) changePassword(c *gin.Context) {
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !bind(c, &input) {
		return
	}
	admin := c.MustGet("admin").(service.Admin)
	if err := s.app.ChangePassword(c.Request.Context(), admin.ID, input.CurrentPassword, input.NewPassword); err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	_ = s.app.WriteAudit(c.Request.Context(), admin.ID, "admin.password.change", "admin", fmt.Sprint(admin.ID), c.ClientIP(), "")
	write(c, http.StatusOK, "OK", "密码已修改", nil)
}

func (s *apiServer) dashboard(c *gin.Context) {
	result, err := s.app.Dashboard(c.Request.Context())
	if err != nil {
		writeError(c, err, http.StatusInternalServerError)
		return
	}
	write(c, http.StatusOK, "OK", "获取成功", result)
}

func (s *apiServer) listClientResets(c *gin.Context) {
	limit, offset := pagination(c)
	items, total, err := s.app.ListClientResetOrders(c.Request.Context(), c.Query("status"), c.Query("search"), limit, offset)
	if err != nil {
		writeError(c, err, http.StatusInternalServerError)
		return
	}
	write(c, http.StatusOK, "OK", "获取成功", gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}

func (s *apiServer) reconcileClientReset(c *gin.Context) {
	var input struct {
		Note string `json:"note"`
	}
	if !bind(c, &input) {
		return
	}
	admin := c.MustGet("admin").(service.Admin)
	if err := s.app.ConfirmClientResetNotApplied(c.Request.Context(), admin.ID, c.ClientIP(), c.Param("orderNo"), input.Note); err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "已确认本次重置未执行，客户可以重新提交", nil)
}

func (s *apiServer) confirmClientResetPayment(c *gin.Context) {
	var input struct {
		TradeNo string `json:"tradeNo"`
		Note    string `json:"note"`
	}
	if !bind(c, &input) {
		return
	}
	admin := c.MustGet("admin").(service.Admin)
	if err := s.app.AdminConfirmClientResetPayment(c.Request.Context(), admin.ID, c.ClientIP(), c.Param("orderNo"), input.TradeNo, input.Note); err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "已确认 LDC 收款，重置任务已排队", nil)
}

func (s *apiServer) confirmClientResetSuccess(c *gin.Context) {
	var input struct {
		Note string `json:"note"`
	}
	if !bind(c, &input) {
		return
	}
	admin := c.MustGet("admin").(service.Admin)
	if err := s.app.AdminConfirmClientResetSuccess(c.Request.Context(), admin.ID, c.ClientIP(), c.Param("orderNo"), input.Note); err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "已手动确认重置成功", nil)
}

func (s *apiServer) refundClientReset(c *gin.Context) {
	var input struct {
		Note string `json:"note"`
	}
	if !bind(c, &input) {
		return
	}
	admin := c.MustGet("admin").(service.Admin)
	if err := s.app.AdminRefundClientReset(c.Request.Context(), admin.ID, c.ClientIP(), c.Param("orderNo"), input.Note); err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "LDC 全额退款已处理", nil)
}

func (s *apiServer) confirmClientResetRefunded(c *gin.Context) {
	var input struct {
		Note string `json:"note"`
	}
	if !bind(c, &input) {
		return
	}
	admin := c.MustGet("admin").(service.Admin)
	if err := s.app.AdminConfirmClientResetRefunded(c.Request.Context(), admin.ID, c.ClientIP(), c.Param("orderNo"), input.Note); err != nil {
		writeError(c, err, http.StatusBadRequest)
		return
	}
	write(c, http.StatusOK, "OK", "已确认退款，订单状态已更新", nil)
}

func (s *apiServer) listClients(c *gin.Context) {
	clients, err := s.app.Panel.ListClients(c.Request.Context())
	if err != nil {
		writeError(c, err, http.StatusBadGateway)
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	if limit < 1 || limit > 500 {
		limit = 200
	}
	items := make([]gin.H, 0, min(limit, len(clients)))
	for _, client := range clients {
		if len(items) == limit {
			break
		}
		used := client.UploadBytes + client.DownloadBytes
		items = append(items, gin.H{"email": client.Email, "enabled": client.Enabled, "expiryTime": client.ExpiryMS, "totalBytes": client.TotalBytes, "usedBytes": used, "remainingBytes": max(int64(0), client.TotalBytes-used), "inboundIds": client.InboundIDs, "nodeSync": "unknown"})
	}
	write(c, http.StatusOK, "OK", "获取成功", gin.H{"items": items, "total": len(clients)})
}

func (s *apiServer) panelStatus(c *gin.Context) {
	info, err := s.app.PanelStatus(c.Request.Context())
	if err != nil {
		write(c, http.StatusBadGateway, "PANEL_UNAVAILABLE", "面板连接失败，请检查地址、令牌和 API 能力", gin.H{"connected": false})
		return
	}
	write(c, http.StatusOK, "OK", "面板连接正常", info)
}

func (s *apiServer) settings(c *gin.Context) {
	info, panelErr := s.app.PanelStatus(c.Request.Context())
	write(c, http.StatusOK, "OK", "获取成功", gin.H{"panelMode": s.cfg.Panel.Mode, "tokenConfigured": s.cfg.PanelToken() != "", "connected": panelErr == nil, "version": info.Version, "timezone": s.cfg.Server.Timezone, "appEnv": s.cfg.AppEnv})
}

func (s *apiServer) updateSettings(c *gin.Context) {
	write(c, http.StatusNotImplemented, "NOT_SUPPORTED", "面板连接参数通过服务器配置和环境变量管理", nil)
}

func (s *apiServer) backup(c *gin.Context) {
	if err := c.Request.Context().Err(); err != nil {
		writeError(c, err, http.StatusRequestTimeout)
		return
	}
	dir := filepath.Join(filepath.Dir(s.cfg.Database.Path), "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		writeError(c, err, http.StatusInternalServerError)
		return
	}
	name := "recharge-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
	path := filepath.Join(dir, name)
	quoted := strings.ReplaceAll(filepath.ToSlash(path), "'", "''")
	if _, err := s.app.DB.ExecContext(c.Request.Context(), "VACUUM INTO '"+quoted+"'"); err != nil {
		writeError(c, err, http.StatusInternalServerError)
		return
	}
	_ = os.Chmod(path, 0600)
	admin := c.MustGet("admin").(service.Admin)
	_ = s.app.WriteAudit(c.Request.Context(), admin.ID, "database.backup", "database", name, c.ClientIP(), "")
	c.FileAttachment(path, name)
}

func (s *apiServer) auditLogs(c *gin.Context) {
	limit, offset := pagination(c)
	items, err := s.app.ListAudit(c.Request.Context(), limit, offset)
	if err != nil {
		writeError(c, err, http.StatusInternalServerError)
		return
	}
	write(c, http.StatusOK, "OK", "获取成功", gin.H{"items": items})
}

func bind(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
	if err := c.ShouldBindJSON(target); err != nil {
		write(c, http.StatusBadRequest, "BAD_REQUEST", "请求参数无效", nil)
		return false
	}
	return true
}

func pagination(c *gin.Context) (int, int) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 || offset > 1_000_000 {
		offset = 0
	}
	return limit, offset
}

func write(c *gin.Context, status int, code, message string, data any) {
	c.JSON(status, response{Success: status >= 200 && status < 300, Code: code, Message: message, Data: data, RequestID: c.GetString("requestID")})
}

func writeError(c *gin.Context, err error, status int) {
	message := err.Error()
	if errors.Is(err, service.ErrUnauthorized) {
		status, message = http.StatusUnauthorized, "管理员登录已失效"
	} else if errors.Is(err, service.ErrInvalidCredentials) {
		status, message = http.StatusUnauthorized, "用户名或密码错误"
	} else if errors.Is(err, service.ErrLoginRateLimited) {
		status, message = http.StatusTooManyRequests, "登录失败次数过多，请稍后再试"
	} else if errors.Is(err, service.ErrCSRF) {
		status, message = http.StatusForbidden, "请求校验失败，请刷新页面后重试"
	}
	if status >= 500 {
		c.GetString("requestID")
		message = "服务器暂时无法处理请求"
	}
	write(c, status, http.StatusText(status), message, nil)
}

func requestContext(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _, err := securityTokenID()
		if err != nil {
			id = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		c.Set("requestID", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		logger.Info("http request", "method", c.Request.Method, "path", c.Request.URL.Path, "status", c.Writer.Status(), "latency_ms", time.Since(started).Milliseconds(), "request_id", c.GetString("requestID"))
	}
}

func securityHeaders(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "same-origin")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		if cfg.AppEnv == "production" {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	}
}

type rateBucket struct {
	count int
	reset time.Time
}

func rateLimit(limit int, window time.Duration) gin.HandlerFunc {
	var mu sync.Mutex
	buckets := map[string]rateBucket{}
	return func(c *gin.Context) {
		key := c.ClientIP()
		now := time.Now()
		mu.Lock()
		bucket := buckets[key]
		if now.After(bucket.reset) {
			bucket = rateBucket{reset: now.Add(window)}
		}
		bucket.count++
		buckets[key] = bucket
		if len(buckets) > 4096 {
			for ip, entry := range buckets {
				if now.After(entry.reset) {
					delete(buckets, ip)
				}
			}
		}
		mu.Unlock()
		if bucket.count > limit {
			write(c, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁，请稍后再试", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

func securityTokenID() (string, string, error) {
	token, _, err := security.NewToken()
	if err != nil {
		return "", "", err
	}
	return token[:16], "", nil
}
