// Command collector runs benchmark rounds and writes them to Postgres.
//
// Environment:
//
//	REGION         label of this collector's rows, e.g. eu-central
//	DATABASE_URL   Postgres connection string
//	SLOTS_API_URL  base of the token slots API; {SLOTS_API_URL}/{chainId}?token=…
//	RPC_<chainId>  RPC of every configured chain, e.g. RPC_8453
//	ZEROX_API_KEY, LIFI_API_KEY, ENSO_API_KEY
//	               API keys of the aggregators that need one, when the config lists them
//
// It exits when the database cannot be written: a round is never kept anywhere else.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/round"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/sim"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/store"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// version is the collector's git commit, set at build time:
// go build -ldflags "-X main.version=$(git rev-parse HEAD)"
var version = "dev"

func main() {
	configPath := flag.String("config", "config/benchmark.json", "benchmark config")
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
	region, dbURL, slotsURL := os.Getenv("REGION"), os.Getenv("DATABASE_URL"), os.Getenv("SLOTS_API_URL")
	if region == "" || dbURL == "" || slotsURL == "" {
		return errors.New("REGION, DATABASE_URL and SLOTS_API_URL are required")
	}
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	hc := upstream.NewHTTPClient()
	asked, err := askedAggregators(cfg, aggregators(hc, cfg.IntegratorID, os.Getenv), os.Getenv)
	if err != nil {
		return err
	}
	slotsAPI := slots.New(upstream.New(hc, slotsURL, http.Header{"Referer": {upstream.Referer}}))
	var runners []*round.Runner
	for _, ch := range cfg.Chains {
		r, err := setUpChain(ctx, cfg, ch, slotsAPI)
		if err != nil {
			return fmt.Errorf("chain %d (%s): %w", ch.ID, ch.Name, err)
		}
		r.Aggregators, r.Region, r.Version = asked, region, version
		runners = append(runners, r)
	}

	// One round start at a time, on a chain picked by its weight.
	every := time.Duration(float64(time.Minute) / cfg.RoundsPerMinute)
	slog.Info("collecting", "region", region, "version", version, "chains", len(runners), "every", every)
	failed := make(chan error, 1)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		go collect(ctx, pickChain(runners), db, failed)
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			return err
		case <-tick.C:
		}
	}
}

// askedAggregators are the config's aggregators. Every one is asked on every
// chain: one that does not serve a chain fails there, like any other failure.
func askedAggregators(cfg *config.Config, all map[string]aggregator.Aggregator, env func(string) string) ([]aggregator.Aggregator, error) {
	var out []aggregator.Aggregator
	for _, id := range cfg.Aggregators {
		if name, ok := keyEnv[id]; ok && env(name) == "" {
			return nil, fmt.Errorf("aggregator %q needs its API key in %s (or remove it from the config's aggregators)", id, name)
		}
		a, ok := all[id]
		if !ok {
			return nil, fmt.Errorf("aggregator %q does not exist", id)
		}
		out = append(out, a)
	}
	return out, nil
}

// pickChain draws a chain with probability weight / sum of weights.
func pickChain(runners []*round.Runner) *round.Runner {
	total := 0
	for _, r := range runners {
		total += r.Chain.Weight
	}
	n := mrand.IntN(total)
	for _, r := range runners {
		if n -= r.Chain.Weight; n < 0 {
			return r
		}
	}
	panic("unreachable")
}

// setUpChain checks everything a chain's rounds depend on before the first one.
func setUpChain(ctx context.Context, cfg *config.Config, ch config.Chain, slotsAPI *slots.Client) (*round.Runner, error) {
	url := os.Getenv("RPC_" + strconv.FormatUint(ch.ID, 10))
	if url == "" {
		return nil, fmt.Errorf("RPC_%d is not set", ch.ID)
	}
	s := sim.New(rpc.New(url, upstream.NewHTTPClient()), ch, cfg.GasLimitMultiplier)
	if err := s.CheckNode(ctx); err != nil {
		return nil, fmt.Errorf("RPC: %w", err)
	}
	r := &round.Runner{Config: cfg, Chain: ch, Sim: s, Slots: slotsAPI}
	for name, t := range ch.Tokens {
		if t.Native() {
			continue
		}
		if _, err := slotsAPI.Get(ctx, ch.ID, t.Address, "0x000000000000000000000000000000000000dEaD", "0x000000000000000000000000000000000000bEEF"); err != nil {
			return nil, fmt.Errorf("token %s: %w", name, err)
		}
	}
	return r, nil
}

// collect runs one round and stores it. A failed write ends the collector.
func collect(ctx context.Context, r *round.Runner, db *store.Store, failed chan<- error) {
	rd, err := r.Run(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("round dropped", "chain", r.Chain.ID, "err", err)
		}
		return
	}
	id, err := db.Insert(ctx, rd)
	if err != nil {
		if ctx.Err() == nil {
			select {
			case failed <- fmt.Errorf("database write: %w", err):
			default:
			}
		}
		return
	}
	var statuses []string
	for _, q := range rd.Quotes {
		s := q.Aggregator + "=" + q.Status
		if q.T0 != nil && !q.T0.Success {
			s += "(reverted)"
		}
		statuses = append(statuses, s)
	}
	slog.Info("round", "id", id, "chain", rd.ChainID, "in", rd.TokenIn, "out", rd.TokenOut, "amount", rd.AmountIn,
		"block", rd.T0Block, "quotes", strings.Join(statuses, " "))
}
