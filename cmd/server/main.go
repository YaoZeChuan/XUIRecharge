package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xui-recharge/internal/config"
	"xui-recharge/internal/database"
	"xui-recharge/internal/panel"
	"xui-recharge/internal/service"
	apiweb "xui-recharge/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}
	if len(cfg.Security.SessionSecret) < 32 {
		logger.Error("security secrets are missing or too short", "required", "SESSION_SECRET must contain at least 32 bytes")
		os.Exit(1)
	}
	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	var panelClient panel.ClientAPI
	if cfg.Panel.Mode == "mock" {
		if cfg.AppEnv == "production" {
			logger.Error("mock panel mode is prohibited in production")
			os.Exit(1)
		}
		panelClient = panel.NewMock()
		logger.Warn("mock panel enabled; use only for development and testing")
	} else {
		client, err := panel.NewAPI(cfg.Panel.BaseURL, cfg.PanelToken(), time.Duration(cfg.Panel.TimeoutSeconds)*time.Second, cfg.Panel.TLSVerify, cfg.Panel.SessionCookie)
		if err != nil {
			logger.Error("configure 3x-ui API", "error", err)
			os.Exit(1)
		}
		panelClient = client
		if cfg.PanelToken() == "" {
			logger.Warn("PANEL_API_TOKEN is not configured; panel operations will be unavailable")
		}
	}

	app := service.New(db, panelClient, cfg)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := app.EnsureInitialAdmin(ctx); err != nil {
		cancel()
		logger.Error("initialize administrator", "error", err)
		os.Exit(1)
	}
	if err := app.RecoverIncompleteOrders(ctx); err != nil {
		cancel()
		logger.Error("recover incomplete orders", "error", err)
		os.Exit(1)
	}
	if err := app.RecoverIncompleteClientResetOrders(ctx); err != nil {
		cancel()
		logger.Error("recover incomplete client reset orders", "error", err)
		os.Exit(1)
	}
	cancel()
	if app.LDC != nil {
		go app.RunLDCPaymentWorker(workerCtx)
		logger.Info("LDC payment polling enabled", "interval_seconds", cfg.LDC.PollIntervalSeconds)
	}

	handler := apiweb.NewRouter(app, cfg, logger)
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("server started", "address", server.Addr, "panel_mode", cfg.Panel.Mode)
		serverErrors <- server.ListenAndServe()
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	case <-stop:
		stopWorker()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown failed", "error", err)
		}
	}
}
