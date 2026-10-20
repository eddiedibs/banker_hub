// Command bankerhub serves transaction history collected from Binance,
// BNC and (later) PayPal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eddiedibs/banker_hub/internal/config"
	"github.com/eddiedibs/banker_hub/internal/httpapi"
	"github.com/eddiedibs/banker_hub/internal/provider"
	"github.com/eddiedibs/banker_hub/internal/provider/binance"
	"github.com/eddiedibs/banker_hub/internal/provider/bnc"
	"github.com/eddiedibs/banker_hub/internal/provider/paypal"
	"github.com/eddiedibs/banker_hub/internal/store"
	"github.com/eddiedibs/banker_hub/internal/syncer"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	envFile := flag.String("env", ".env", "env file to load; process environment variables take precedence")
	flag.Parse()
	if err := config.LoadDotEnv(*envFile); err != nil {
		return fmt.Errorf("load %s: %w", *envFile, err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	providers := []provider.Provider{paypal.New()}
	if cfg.Binance.Enabled() {
		providers = append(providers, binance.New(cfg.Binance.BaseURL,
			cfg.Binance.APIKey.Reveal(), cfg.Binance.APISecret.Reveal()))
	} else {
		log.Warn("binance disabled: BINANCE_API_KEY / BINANCE_API_SECRET not set")
	}
	if cfg.BNC.Enabled() {
		providers = append(providers, bnc.New(cfg.BNC.BaseURL, bnc.Credentials{
			Card:     cfg.BNC.Card.Reveal(),
			UserID:   cfg.BNC.UserID.Reveal(),
			Password: cfg.BNC.Password.Reveal(),
		}, bnc.WithDebugDir(os.Getenv("BNC_DEBUG_DIR"))))
		if d := os.Getenv("BNC_DEBUG_DIR"); d != "" {
			log.Warn("bnc debug dump on: response bodies with account data are written to disk", "dir", d)
		}
	} else {
		log.Warn("bnc disabled: BNC_CARD / BNC_USER_ID / BNC_PASSWORD not set")
	}

	sy := syncer.New(st, cfg.SyncLookback, log, providers...)
	// After every start, BNC re-reads the whole current month once.
	sy.StartFrom(bnc.Name, bnc.MonthStart)
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		sy.Loop(ctx, cfg.SyncInterval)
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewHandler(st, sy, log, cfg.APIToken.Reveal()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// POST /sync waits for providers; BNC alone can take a minute.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "providers", sy.Names())
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	// Let a running sync finish its cancellation before the store closes.
	stop()
	<-syncDone
	return err
}
