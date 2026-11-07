// Command distribution derives what the benchmark trades from what users
// trade: the weight of each chain, and on each chain the pairs, their size
// buckets and the weight of each (pair, size).
//
//   - Chains: DefiLlama's 30-day aggregator volume of each configured chain.
//   - Pairs and sizes: the swaps the LI.FI and KyberSwap routers logged on the
//     chain over the last days (-days), read with eth_getLogs, valued in USD
//     from the trades themselves (the config's stablecoins at $1), and
//     weighted by volume. The rules are in derive.go and METHODOLOGY.md.
//
// It keeps the config's chains, the names and flags of the tokens it already
// has, and everything outside the chains, and prints the new config (-write:
// writes it back). Each chain records the blocks it was derived from;
// -reproduce reads those blocks again instead of the latest ones; -chain
// derives one chain only.
//
// Environment: RPC_<chainId> of every configured chain (with eth_getLogs over
// thousands of blocks), SLOTS_API_URL to check that new tokens can be simulated.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

func main() {
	configPath := flag.String("config", "config/benchmark.json", "benchmark config")
	days := flag.Float64("days", 7, "how many days of router events to read")
	write := flag.Bool("write", false, "write the new config back instead of printing it")
	reproduce := flag.Bool("reproduce", false, "read the blocks each chain records instead of the latest ones")
	only := flag.Uint64("chain", 0, "derive only this chain id and keep the others as they are")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	out, err := run(ctx, *configPath, *days, *reproduce, *only)
	if err == nil && *write {
		err = os.WriteFile(*configPath, out, 0o644)
	} else if err == nil {
		_, err = os.Stdout.Write(out)
	}
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string, days float64, reproduce bool, only uint64) ([]byte, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	slotsURL := os.Getenv("SLOTS_API_URL")
	if slotsURL == "" {
		return nil, errors.New("SLOTS_API_URL is required")
	}
	hc := upstream.NewHTTPClient()
	slotsAPI := slots.New(upstream.New(hc, slotsURL, http.Header{"Referer": {upstream.Referer}}))
	volumes, err := defiLlama(ctx, upstream.New(hc, "https://api.llama.fi", nil))
	if err != nil {
		return nil, fmt.Errorf("DefiLlama: %w", err)
	}

	chains := slices.Clone(cfg.Chains)
	errs := make([]error, len(chains))
	var wg sync.WaitGroup
	for i := range chains {
		if only != 0 && chains[i].ID != only {
			continue
		}
		wg.Go(func() {
			ch := &chains[i]
			if errs[i] = deriveChain(ctx, ch, days, reproduce, slotsAPI); errs[i] != nil {
				errs[i] = fmt.Errorf("chain %d (%s): %w", ch.ID, ch.Name, errs[i])
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// Chain weights: basis points of the configured chains' aggregator volume.
	total := 0.0
	for _, ch := range chains {
		v, ok := volumes[ch.DefiLlama]
		if ch.DefiLlama == "" || !ok || v <= 0 {
			return nil, fmt.Errorf("chain %d (%s): no DefiLlama volume for %q", ch.ID, ch.Name, ch.DefiLlama)
		}
		total += v
	}
	for i := range chains {
		v := volumes[chains[i].DefiLlama]
		chains[i].Weight = max(int(math.Round(10_000*v/total)), 1)
		if chains[i].Derived == nil {
			return nil, fmt.Errorf("chain %d (%s) was never derived", chains[i].ID, chains[i].Name)
		}
		chains[i].Derived.DefiLlamaVolume30d = int64(v)
		slog.Info("chain", "name", chains[i].Name, "weight", chains[i].Weight, "volume30d", int64(v))
	}
	return format(cfg, chains)
}

// deriveChain replaces the chain's tokens and pairs with the derived ones.
func deriveChain(ctx context.Context, ch *config.Chain, days float64, reproduce bool, slotsAPI *slots.Client) error {
	url := os.Getenv("RPC_" + strconv.FormatUint(ch.ID, 10))
	if url == "" {
		return fmt.Errorf("RPC_%d is not set", ch.ID)
	}
	c := rpc.New(url, upstream.NewHTTPClient())
	var from, to uint64
	if reproduce {
		if ch.Derived == nil {
			return errors.New("the config records no blocks to reproduce")
		}
		from, to = ch.Derived.FromBlock, ch.Derived.ToBlock
	} else {
		var head string
		if err := c.Call(ctx, &head, "eth_blockNumber"); err != nil {
			return err
		}
		h, err := strconv.ParseUint(strings.TrimPrefix(head, "0x"), 16, 64)
		if err != nil {
			return fmt.Errorf("block number %q", head)
		}
		since := time.Now().Add(-time.Duration(days * float64(24*time.Hour)))
		if from, err = blockAt(ctx, c, h, since); err != nil {
			return err
		}
		to = h
	}
	ts, err := trades(ctx, c, ch.Name, from, to)
	if err != nil {
		return err
	}
	// The anchors stay what they were: the config's stablecoins the first time.
	var anchors []string
	if ch.Derived != nil {
		anchors = ch.Derived.Anchors
		if reproduce && ch.Derived.Trades != len(ts) {
			slog.Warn("the node returned other events than the config was derived from", "chain", ch.Name,
				"trades", len(ts), "recorded", ch.Derived.Trades)
		}
	}
	if len(anchors) == 0 {
		for name, t := range ch.Tokens {
			if t.Stable {
				anchors = append(anchors, name)
			}
		}
		slices.Sort(anchors)
	}
	resolve := func(a string) (meta, error) {
		sym, dec, err := tokenMeta(ctx, c, a)
		if err != nil {
			return meta{}, err
		}
		if a != strings.ToLower(config.NativeToken) {
			if _, err := slotsAPI.Get(ctx, ch.ID, a, "0x000000000000000000000000000000000000dEaD", "0x000000000000000000000000000000000000bEEF"); err != nil {
				return meta{}, err
			}
		}
		return meta{sym, dec}, nil
	}
	tokens, pairs, rep, err := derive(ts, *ch, anchors, resolve)
	for _, s := range rep.skipped {
		slog.Info("left out", "chain", ch.Name, "pair", s)
	}
	if err != nil {
		return err
	}
	slog.Info("derived", "chain", ch.Name, "blocks", fmt.Sprintf("%d-%d", from, to), "trades", rep.trades, "valued", rep.valued,
		"volume", fmt.Sprintf("$%.0f", rep.volume), "pairs", len(pairs), "covered", fmt.Sprintf("%.0f%%", 100*rep.covered))
	slices.SortStableFunc(pairs, func(x, y config.Pair) int { return weight(y) - weight(x) })
	ch.Tokens, ch.Pairs, ch.Derived = tokens, pairs, &config.Derived{FromBlock: from, ToBlock: to, Trades: len(ts), Anchors: anchors}
	return nil
}

// defiLlama returns each chain's aggregator volume of the last 30 days, in USD,
// by DefiLlama's chain name.
func defiLlama(ctx context.Context, up *upstream.Client) (map[string]float64, error) {
	body, err := up.Get(ctx, "/overview/aggregators?excludeTotalDataChart=true&excludeTotalDataChartBreakdown=true")
	if err != nil {
		return nil, err
	}
	var r struct {
		Protocols []struct {
			Breakdown30d map[string]map[string]float64 `json:"breakdown30d"`
		} `json:"protocols"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, p := range r.Protocols {
		for chain, byName := range p.Breakdown30d {
			for _, v := range byName {
				out[chain] += v
			}
		}
	}
	return out, nil
}

// format writes the config with the derived chains: one line per token and
// per pair, sizes in bucket order. It refuses to write a config that does
// not read back as exactly these chains.
func format(cfg *config.Config, chains []config.Chain) ([]byte, error) {
	top := *cfg
	top.Chains = nil
	head, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for i, ch := range chains {
		sep := ","
		if i == len(chains)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "    {\n      \"id\": %d,\n      \"name\": %q,\n      \"maxTxGas\": %d,\n      \"blockTime\": %q,\n      \"weight\": %d,\n      \"defillama\": %q,\n",
			ch.ID, ch.Name, ch.MaxTxGas, ch.BlockTime.String(), ch.Weight, ch.DefiLlama)
		d := ch.Derived
		anchors, _ := json.Marshal(d.Anchors)
		fmt.Fprintf(&b, "      \"derived\": { \"fromBlock\": %d, \"toBlock\": %d, \"trades\": %d, \"anchors\": %s, \"defillamaVolume30d\": %d },\n",
			d.FromBlock, d.ToBlock, d.Trades, anchors, d.DefiLlamaVolume30d)
		b.WriteString("      \"tokens\": {\n")
		names := make([]string, 0, len(ch.Tokens))
		for n := range ch.Tokens {
			names = append(names, n)
		}
		slices.SortFunc(names, func(x, y string) int { return strings.Compare(strings.ToLower(x), strings.ToLower(y)) })
		for j, n := range names {
			t := ch.Tokens[n]
			fmt.Fprintf(&b, "        %q: { \"address\": %q, \"decimals\": %d", n, t.Address, t.Decimals)
			if t.Stable {
				b.WriteString(`, "stable": true`)
			}
			if t.WrapsNative {
				b.WriteString(`, "wrapsNative": true`)
			}
			b.WriteString(`, "sizes": ` + sizes(t.Sizes, strconv.Quote) + " }")
			b.WriteString(map[bool]string{true: "\n", false: ",\n"}[j == len(names)-1])
		}
		b.WriteString("      },\n      \"pairs\": [\n")

		for j, p := range ch.Pairs {
			fmt.Fprintf(&b, "        { \"a\": %q, \"b\": %q, \"sizes\": %s }", p.A, p.B, sizes(p.Sizes, strconv.Itoa))
			b.WriteString(map[bool]string{true: "\n", false: ",\n"}[j == len(ch.Pairs)-1])
		}
		b.WriteString("      ]\n    }" + sep + "\n")
	}
	out := bytes.Replace(head, []byte(`"chains": null`), []byte("\"chains\": [\n"+b.String()+"  ]"), 1)
	out = append(out, '\n')

	back, err := config.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("the derived config is not valid: %w", err)
	}
	if !reflect.DeepEqual(back.Chains, chains) {
		return nil, errors.New("the written config does not read back as the derived one")
	}
	back.Chains = nil
	if !reflect.DeepEqual(*back, top) {
		return nil, errors.New("the written config changes more than the chains")
	}
	return out, nil
}

func weight(p config.Pair) int {
	w := 0
	for _, v := range p.Sizes {
		w += v
	}
	return w
}

// sizes writes a sizes map in bucket order.
func sizes[V any](m map[string]V, value func(V) string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int { return int(config.BucketValue(x) - config.BucketValue(y)) })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strconv.Quote(k) + ": " + value(m[k])
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}
