package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"xui-recharge/internal/ldc"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Panel    PanelConfig    `yaml:"panel"`
	Recharge RechargeConfig `yaml:"recharge"`
	LDC      LDCConfig      `yaml:"ldc"`
	Security SecurityConfig `yaml:"security"`
	AppEnv   string         `yaml:"-"`
}

type ServerConfig struct {
	Host           string   `yaml:"host"`
	Port           int      `yaml:"port"`
	Timezone       string   `yaml:"timezone"`
	TrustedProxies []string `yaml:"trusted_proxies"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type PanelConfig struct {
	Mode           string `yaml:"mode"`
	BaseURL        string `yaml:"base_url"`
	APITokenEnv    string `yaml:"api_token_env"`
	SessionCookie  string `yaml:"-"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	TLSVerify      bool   `yaml:"tls_verify"`
}

type RechargeConfig struct {
	AllowExpiredRenewal  bool  `yaml:"allow_expired_renewal"`
	MaxDaysPerOrder      int   `yaml:"max_days_per_order"`
	MaxTrafficGBPerOrder int64 `yaml:"max_traffic_gb_per_order"`
}

type LDCConfig struct {
	BaseURL             string `yaml:"base_url"`
	PublicBaseURL       string `yaml:"public_base_url"`
	ClientID            string `yaml:"-"`
	ClientSecret        string `yaml:"-"`
	Ed25519PrivateKey   string `yaml:"-"`
	PollIntervalSeconds int    `yaml:"poll_interval_seconds"`
}

type SecurityConfig struct {
	MaxFailedAttempts      int    `yaml:"max_failed_attempts"`
	AdminSessionHours      int    `yaml:"admin_session_hours"`
	CardHMACSecret         []byte `yaml:"-"`
	CardHMACPreviousSecret []byte `yaml:"-"`
	SessionSecret          []byte `yaml:"-"`
	AdminUsername          string `yaml:"-"`
	AdminInitialPassword   string `yaml:"-"`
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}
	cfg := Config{
		Server:   ServerConfig{Host: "0.0.0.0", Port: 8080, Timezone: "Asia/Shanghai"},
		Database: DatabaseConfig{Path: "./data/recharge.db"},
		Panel:    PanelConfig{Mode: "real", APITokenEnv: "PANEL_API_TOKEN", TimeoutSeconds: 10, TLSVerify: true},
		Recharge: RechargeConfig{AllowExpiredRenewal: true, MaxDaysPerOrder: 3650, MaxTrafficGBPerOrder: 10000},
		LDC:      LDCConfig{BaseURL: "https://credit.linux.do", PollIntervalSeconds: 30},
		Security: SecurityConfig{MaxFailedAttempts: 5, AdminSessionHours: 12},
	}

	configPath := strings.TrimSpace(os.Getenv("CONFIG_FILE"))
	if configPath == "" {
		configPath = "configs/config.yaml"
	}
	if data, err := os.ReadFile(configPath); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", configPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config %s: %w", configPath, err)
	}

	setString(&cfg.Server.Host, "SERVER_HOST")
	setInt(&cfg.Server.Port, "SERVER_PORT")
	setString(&cfg.Server.Timezone, "SERVER_TIMEZONE")
	setString(&cfg.Database.Path, "DATABASE_PATH")
	setString(&cfg.Panel.Mode, "PANEL_MODE")
	setString(&cfg.Panel.BaseURL, "PANEL_BASE_URL")
	setString(&cfg.Panel.APITokenEnv, "PANEL_API_TOKEN_ENV")
	cfg.Panel.SessionCookie = strings.TrimSpace(os.Getenv("PANEL_COOKIE"))
	setInt(&cfg.Panel.TimeoutSeconds, "PANEL_TIMEOUT_SECONDS")
	setBool(&cfg.Panel.TLSVerify, "PANEL_TLS_VERIFY")
	setBool(&cfg.Recharge.AllowExpiredRenewal, "ALLOW_EXPIRED_RENEWAL")
	setInt(&cfg.Recharge.MaxDaysPerOrder, "MAX_DAYS_PER_ORDER")
	setInt64(&cfg.Recharge.MaxTrafficGBPerOrder, "MAX_TRAFFIC_GB_PER_ORDER")
	setString(&cfg.LDC.BaseURL, "LDC_BASE_URL")
	setString(&cfg.LDC.PublicBaseURL, "PUBLIC_BASE_URL")
	setInt(&cfg.LDC.PollIntervalSeconds, "LDC_POLL_INTERVAL_SECONDS")
	setInt(&cfg.Security.MaxFailedAttempts, "MAX_FAILED_ATTEMPTS")
	setInt(&cfg.Security.AdminSessionHours, "ADMIN_SESSION_HOURS")

	cfg.AppEnv = strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	cfg.Security.CardHMACSecret = []byte(os.Getenv("CARD_HMAC_SECRET"))
	cfg.Security.CardHMACPreviousSecret = []byte(os.Getenv("CARD_HMAC_SECRET_PREVIOUS"))
	cfg.Security.SessionSecret = []byte(os.Getenv("SESSION_SECRET"))
	cfg.Security.AdminUsername = strings.TrimSpace(os.Getenv("ADMIN_USERNAME"))
	cfg.Security.AdminInitialPassword = os.Getenv("ADMIN_INITIAL_PASSWORD")
	cfg.LDC.ClientID = strings.TrimSpace(os.Getenv("LDC_CLIENT_ID"))
	cfg.LDC.ClientSecret = os.Getenv("LDC_CLIENT_SECRET")
	cfg.LDC.Ed25519PrivateKey = os.Getenv("LDC_ED25519_PRIVATE_KEY")

	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return Config{}, errors.New("server.port must be between 1 and 65535")
	}
	if cfg.Database.Path == "" {
		return Config{}, errors.New("database.path is required")
	}
	if cfg.Panel.Mode != "real" && cfg.Panel.Mode != "mock" {
		return Config{}, errors.New("panel.mode must be real or mock")
	}
	if cfg.Panel.TimeoutSeconds < 1 || cfg.Panel.TimeoutSeconds > 120 {
		return Config{}, errors.New("panel.timeout_seconds must be between 1 and 120")
	}
	if cfg.Recharge.MaxDaysPerOrder < 1 || cfg.Recharge.MaxTrafficGBPerOrder < 1 {
		return Config{}, errors.New("recharge order limits must be positive")
	}
	if cfg.LDC.PollIntervalSeconds < 10 || cfg.LDC.PollIntervalSeconds > 3600 {
		return Config{}, errors.New("ldc.poll_interval_seconds must be between 10 and 3600")
	}
	if (cfg.LDC.ClientID != "" || cfg.LDC.ClientSecret != "" || cfg.LDC.Ed25519PrivateKey != "" || cfg.LDC.PublicBaseURL != "") && !cfg.LDC.Enabled() {
		return Config{}, errors.New("LDC_CLIENT_ID, LDC_CLIENT_SECRET, LDC_ED25519_PRIVATE_KEY, and PUBLIC_BASE_URL must all be configured to enable LDC payments")
	}
	if cfg.LDC.Enabled() {
		if err := validateHTTPSBaseURL(cfg.LDC.BaseURL); err != nil {
			return Config{}, fmt.Errorf("invalid ldc.base_url: %w", err)
		}
		ldcBase, _ := url.Parse(cfg.LDC.BaseURL)
		ldcPath := strings.TrimRight(ldcBase.Path, "/")
		if ldcPath != "" && ldcPath != "/epay" {
			return Config{}, errors.New("ldc.base_url must be the gateway origin or end in /epay")
		}
		if err := validateHTTPSBaseURL(cfg.LDC.PublicBaseURL); err != nil {
			return Config{}, fmt.Errorf("invalid PUBLIC_BASE_URL: %w", err)
		}
		if err := ldc.ValidateEd25519PrivateKey(cfg.LDC.Ed25519PrivateKey); err != nil {
			return Config{}, fmt.Errorf("invalid LDC_ED25519_PRIVATE_KEY: %w", err)
		}
	}
	if _, err := time.LoadLocation(cfg.Server.Timezone); err != nil {
		return Config{}, fmt.Errorf("invalid server.timezone %q: %w", cfg.Server.Timezone, err)
	}
	if cfg.AppEnv == "production" {
		if !cfg.Panel.TLSVerify {
			return Config{}, errors.New("panel.tls_verify must remain enabled in production")
		}
		if len(cfg.Security.SessionSecret) < 32 {
			return Config{}, errors.New("SESSION_SECRET must contain at least 32 bytes in production")
		}
	}
	return cfg, nil
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open environment file: %w", err)
	}
	defer file.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	for {
		n, readErr := file.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > 64*1024 {
				return errors.New(".env file exceeds 64 KiB")
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read environment file: %w", readErr)
			}
			break
		}
	}
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("invalid .env line; expected KEY=VALUE")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if key == "" {
			return errors.New(".env contains an empty variable name")
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("load .env variable %q: %w", key, err)
			}
		}
	}
	return nil
}

func (c Config) PanelToken() string {
	if c.Panel.APITokenEnv == "" {
		return ""
	}
	return os.Getenv(c.Panel.APITokenEnv)
}

func (c LDCConfig) Enabled() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.Ed25519PrivateKey != "" && c.PublicBaseURL != ""
}

func validateHTTPSBaseURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be an https URL without user info, query, or fragment")
	}
	return nil
}

func setString(target *string, key string) {
	if value, ok := os.LookupEnv(key); ok {
		*target = strings.TrimSpace(value)
	}
}

func setInt(target *int, key string) {
	if value, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(value); err == nil {
			*target = n
		}
	}
}

func setInt64(target *int64, key string) {
	if value, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			*target = n
		}
	}
}

func setBool(target *bool, key string) {
	if value, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(value); err == nil {
			*target = b
		}
	}
}
