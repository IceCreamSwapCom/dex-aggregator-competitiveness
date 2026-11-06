// Command resim re-simulates stored rounds and prints where the result differs
// from what the collector stored. It is a standalone audit tool: it reads a
// round from the round API or from a JSON file in the same form, and needs no
// database.
//
//	resim [-config config/benchmark.json] [-api https://…] ROUND...
//
// ROUND is a round id, fetched from {api}/api/v1/rounds/{id}, or a .json file.
//
// Environment:
//
//	RPC_<chainId>  RPC of the round's chain; an archive node for rounds older than its state window
//	SLOTS_API_URL  base of the token slots API
//
// It exits with 0 when every simulation is reproduced exactly, 1 when one
// differs and 2 when a round could not be re-simulated.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/round"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/sim"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

func main() {
	configPath := flag.String("config", "config/benchmark.json", "benchmark config: the chains' gas cap and the gas limit multiplier")
	apiURL := flag.String("api", "http://localhost:8080", "round API to fetch round ids from")
	flag.Usage = func() {
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "usage: resim [flags] ROUND_ID|FILE.json...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	slotsURL := os.Getenv("SLOTS_API_URL")
	if slotsURL == "" {
		fmt.Fprintln(os.Stderr, "SLOTS_API_URL is required")
		os.Exit(2)
	}
	hc := upstream.NewHTTPClient()
	sl := slots.New(upstream.New(hc, slotsURL, http.Header{"Referer": {upstream.Referer}}))
	roundAPI := upstream.New(hc, *apiURL, nil)

	ctx := context.Background()
	exit := 0
	for _, arg := range flag.Args() {
		rd, err := load(ctx, roundAPI, arg)
		if err == nil {
			var diffs int
			diffs, err = resimulate(ctx, os.Stdout, cfg, rd, sl, func(chainID uint64) string {
				return os.Getenv("RPC_" + strconv.FormatUint(chainID, 10))
			})
			if diffs > 0 {
				exit = max(exit, 1)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", arg, err)
			exit = 2
		}
	}
	os.Exit(exit)
}

// load reads a round from a JSON file, or by id from the round API.
func load(ctx context.Context, roundAPI *upstream.Client, arg string) (*api.Round, error) {
	var body []byte
	var err error
	if strings.HasSuffix(arg, ".json") {
		body, err = os.ReadFile(arg)
	} else {
		body, err = roundAPI.Get(ctx, "/api/v1/rounds/"+url.PathEscape(arg))
	}
	if err != nil {
		return nil, err
	}
	var rd api.Round
	if err := json.Unmarshal(body, &rd); err != nil {
		return nil, fmt.Errorf("not a round: %w", err)
	}
	return &rd, nil
}

// resimulate runs every stored simulation of the round again, the way the
// collector ran it, prints one line per simulation and returns how many differ.
// At T0 it finds the gas limit again; T30 runs with the stored limit, as the
// collector's did.
func resimulate(ctx context.Context, out io.Writer, cfg *config.Config, rd *api.Round, sl *slots.Client, rpcURL func(uint64) string) (int, error) {
	var ch *config.Chain
	for i := range cfg.Chains {
		if cfg.Chains[i].ID == rd.ChainID {
			ch = &cfg.Chains[i]
		}
	}
	if ch == nil {
		return 0, fmt.Errorf("chain %d is not in the config", rd.ChainID)
	}
	u := rpcURL(rd.ChainID)
	if u == "" {
		return 0, fmt.Errorf("RPC_%d is not set", rd.ChainID)
	}
	s := sim.New(rpc.New(u, upstream.NewHTTPClient()), *ch, cfg.GasLimitMultiplier)
	amountIn, ok1 := new(big.Int).SetString(rd.AmountIn, 10)
	price0, ok2 := new(big.Int).SetString(rd.T0GasPrice, 10)
	price30, ok3 := new(big.Int).SetString(rd.T30GasPrice, 10)
	if !ok1 || !ok2 || !ok3 {
		return 0, errors.New("amountIn or a gas price is not a decimal number")
	}
	b0 := sim.Block{Number: rd.T0Block, GasPrice: price0}
	b30 := sim.Block{Number: rd.T30Block, GasPrice: price30}
	printf := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	printf("round %s: chain %d, %s %s → %s, T0 block %d, T30 block %d\n",
		rd.ID, rd.ChainID, rd.AmountIn, rd.TokenIn, rd.TokenOut, rd.T0Block, rd.T30Block)
	if rd.MethodologyVersion != cfg.MethodologyVersion {
		printf("  note: the round is methodology %s, the config %s\n", rd.MethodologyVersion, cfg.MethodologyVersion)
	}
	var quotes []api.Quote
	var txs []round.Tx
	for _, q := range rd.Quotes {
		if q.T0 != nil {
			quotes = append(quotes, q)
			txs = append(txs, round.Tx{To: q.TxTo, Data: q.TxData, Value: q.TxValue, Spender: q.Spender})
		}
	}
	sale := round.Sale{ChainID: rd.ChainID, TokenIn: rd.TokenIn, TokenOut: rd.TokenOut, AmountIn: amountIn, Taker: rd.Taker}
	swaps, err := round.Swaps(ctx, s, sl, b0, sale, txs)
	if err != nil {
		return 0, err
	}

	diffs := 0
	report := func(aggregator, at string, stored, now api.Simulation, storedLimit, nowLimit uint64) {
		verdict := "same"
		if stored != now || storedLimit != nowLimit {
			verdict, diffs = "DIFF", diffs+1
		}
		printf("  %-12s %-3s %s  stored %s", aggregator, at, verdict, describe(stored, storedLimit))
		if verdict == "DIFF" {
			printf("\n  %-12s %-3s       now    %s", "", "", describe(now, nowLimit))
		}
		printf("\n")
	}
	for i, q := range quotes {
		limit, r0, err := s.Measure(ctx, b0, swaps[i])
		if err != nil {
			return diffs, fmt.Errorf("%s at T0: %w", q.Aggregator, err)
		}
		report(q.Aggregator, "T0", *q.T0, stored(r0), q.GasLimit, limit)
		if q.T30 == nil {
			continue
		}
		r30, err := s.Run(ctx, b30, swaps[i], q.GasLimit)
		if err != nil {
			return diffs, fmt.Errorf("%s at T30: %w", q.Aggregator, err)
		}
		report(q.Aggregator, "T30", *q.T30, stored(r30), 0, 0)
	}
	return diffs, nil
}

// stored is a result as the collector stores it.
func stored(r sim.Result) api.Simulation {
	s := api.Simulation{Success: r.Success, Gas: r.Gas, Revert: r.Revert}
	if r.AmountOut != nil {
		s.AmountOut = r.AmountOut.String()
	}
	return s
}

func describe(s api.Simulation, limit uint64) string {
	d := "reverted " + strconv.Quote(s.Revert)
	if s.Success {
		d = "ok out " + s.AmountOut
	}
	if s.Gas != 0 {
		d += fmt.Sprintf(", gas %d", s.Gas)
	}
	if limit != 0 {
		d += fmt.Sprintf(", limit %d", limit)
	}
	return d
}
