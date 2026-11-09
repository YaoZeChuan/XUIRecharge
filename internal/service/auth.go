package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"xui-recharge/internal/security"
)

type Admin struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type SessionTokens struct {
	Session   string
	CSRF      string
	ExpiresAt int64
	Admin     Admin
}

var ErrInvalidCredentials = errors.New("用户名或密码错误")
var ErrLoginRateLimited = errors.New("登录失败次数过多，请稍后再试")
var ErrUnauthorized = errors.New("管理员登录已失效")
var ErrCSRF = errors.New("请求校验失败，请刷新页面后重试")

func (a *App) Login(ctx context.Context, username, password, ip string) (SessionTokens, error) {
	ip = normalizeIP(ip)
	now := a.nowMS()
	var failures int
	var blockedUntil int64
	err := a.DB.QueryRowContext(ctx, `SELECT failures,blocked_until FROM login_attempts WHERE ip=?`, ip).Scan(&failures, &blockedUntil)
	if err == nil && blockedUntil > now {
		return SessionTokens{}, ErrLoginRateLimited
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SessionTokens{}, fmt.Errorf("read login attempt state: %w", err)
	}

	var admin Admin
	var passwordHash string
	err = a.DB.QueryRowContext(ctx, `SELECT id,username,password_hash FROM admin_users WHERE username=?`, strings.TrimSpace(username)).Scan(&admin.ID, &admin.Username, &passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		_ = security.VerifyPassword(a.dummyPasswordHash, password)
		passwordHash = ""
	} else if err != nil {
		return SessionTokens{}, fmt.Errorf("load administrator account: %w", err)
	}
	if passwordHash == "" || !security.VerifyPassword(passwordHash, password) {
		failures++
		blocked := int64(0)
		if failures >= a.Cfg.Security.MaxFailedAttempts {
			blocked = now + int64(15*time.Minute/time.Millisecond)
			failures = 0
		}
		_, writeErr := a.DB.ExecContext(ctx, `INSERT INTO login_attempts(ip,failures,blocked_until,updated_at) VALUES(?,?,?,?) ON CONFLICT(ip) DO UPDATE SET failures=excluded.failures,blocked_until=excluded.blocked_until,updated_at=excluded.updated_at`, ip, failures, blocked, now)
		if writeErr != nil {
			return SessionTokens{}, fmt.Errorf("record login failure: %w", writeErr)
		}
		return SessionTokens{}, ErrInvalidCredentials
	}
	if _, err := a.DB.ExecContext(ctx, `DELETE FROM login_attempts WHERE ip=?`, ip); err != nil {
		return SessionTokens{}, fmt.Errorf("clear login failures: %w", err)
	}
	session, sessionHash, err := security.NewToken()
	if err != nil {
		return SessionTokens{}, err
	}
	csrf, _, err := security.NewToken()
	if err != nil {
		return SessionTokens{}, err
	}
	if len(a.Cfg.Security.SessionSecret) < 32 {
		return SessionTokens{}, errors.New("SESSION_SECRET must contain at least 32 bytes")
	}
	sessionHash = security.HashTokenHMAC(a.Cfg.Security.SessionSecret, session)
	csrfHash := security.HashTokenHMAC(a.Cfg.Security.SessionSecret, csrf)
	expires := now + int64(a.Cfg.Security.AdminSessionHours)*int64(time.Hour/time.Millisecond)
	_, err = a.DB.ExecContext(ctx, `INSERT INTO admin_sessions(token_hash,csrf_hash,admin_id,expires_at,created_at) VALUES(?,?,?,?,?)`, sessionHash, csrfHash, admin.ID, expires, now)
	if err != nil {
		return SessionTokens{}, fmt.Errorf("create administrator session: %w", err)
	}
	return SessionTokens{Session: session, CSRF: csrf, ExpiresAt: expires, Admin: admin}, nil
}

func (a *App) AuthenticateSession(ctx context.Context, sessionToken, csrfCookie, csrfHeader string, unsafe bool) (Admin, error) {
	if sessionToken == "" || len(a.Cfg.Security.SessionSecret) < 32 {
		return Admin{}, ErrUnauthorized
	}
	tokenHash := security.HashTokenHMAC(a.Cfg.Security.SessionSecret, sessionToken)
	var admin Admin
	var storedCSRF string
	var expires int64
	err := a.DB.QueryRowContext(ctx, `SELECT u.id,u.username,s.csrf_hash,s.expires_at FROM admin_sessions s JOIN admin_users u ON u.id=s.admin_id WHERE s.token_hash=?`, tokenHash).Scan(&admin.ID, &admin.Username, &storedCSRF, &expires)
	if err != nil || expires <= a.nowMS() {
		if err == nil {
			_, _ = a.DB.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash=?`, tokenHash)
		}
		return Admin{}, ErrUnauthorized
	}
	if unsafe {
		if csrfCookie == "" || csrfHeader == "" || csrfCookie != csrfHeader {
			return Admin{}, ErrCSRF
		}
		got := security.HashTokenHMAC(a.Cfg.Security.SessionSecret, csrfHeader)
		if !security.VerifyHashValue(storedCSRF, got) {
			return Admin{}, ErrCSRF
		}
	}
	return admin, nil
}

func (a *App) Logout(ctx context.Context, sessionToken string) error {
	if sessionToken == "" {
		return nil
	}
	if len(a.Cfg.Security.SessionSecret) < 32 {
		return nil
	}
	_, err := a.DB.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash=?`, security.HashTokenHMAC(a.Cfg.Security.SessionSecret, sessionToken))
	return err
}

func (a *App) ChangePassword(ctx context.Context, adminID int64, currentPassword, newPassword string) error {
	var oldHash string
	if err := a.DB.QueryRowContext(ctx, `SELECT password_hash FROM admin_users WHERE id=?`, adminID).Scan(&oldHash); err != nil {
		return ErrUnauthorized
	}
	if !security.VerifyPassword(oldHash, currentPassword) {
		return ErrInvalidCredentials
	}
	newHash, err := security.HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE admin_users SET password_hash=?,updated_at=? WHERE id=?`, newHash, a.nowMS(), adminID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE admin_id=?`, adminID); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizeIP(ip string) string {
	if strings.TrimSpace(ip) == "" || len(ip) > 64 {
		return "unknown"
	}
	return strings.TrimSpace(ip)
}
