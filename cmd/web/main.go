// Command web serves the dashboard and the JSON API from the collectors'
// Postgres. It never writes a round; it (re)creates the metrics view at start
// and refreshes it every 5 minutes.
//
// Environment:
//
//	DATABASE_URL   Postgres connection string, the collectors' database
//	ADDR           listen address, default :8080
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/stats"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/store"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/web"
)

const refreshEvery = 5 * time.Minute

func main() {
	configPath := flag.String("config", "config/benchmark.json", "benchmark config: chain and token names")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *configPath); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	dbURL, addr := os.Getenv("DATABASE_URL"), os.Getenv("ADDR")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if addr == "" {
		addr = ":8080"
	}
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	st := stats.New(db.Pool())
	if err := st.Create(ctx); err != nil {
		return err
	}
	srv := web.New(cfg, db, st)
	srv.Refreshed(time.Now())
	go func() {
		tick := time.NewTicker(refreshEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := st.Refresh(ctx); err != nil {
					slog.Error("refreshing the metrics", "err", err)
					continue
				}
				srv.Refreshed(time.Now())
			}
		}
	}()

	hs := &http.Server{
		Addr: addr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	slog.Info("serving", "addr", addr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
