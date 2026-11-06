// Package round runs one benchmark round: pick a trade, ask every aggregator
// at the same moment, simulate every answer at the block they were asked at
// (T0), and the same transactions again at the head block staleDelay later
// (T30).
package round

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	mrand "math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/sim"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/store"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// Runner runs rounds on one chain.
type Runner struct {
	Config      *config.Config
	Chain       config.Chain
	Sim         *sim.Simulator
	Slots       *slots.Client
	Aggregators []aggregator.Aggregator // all of the config's, on every chain
	Region      string
	Version     string // of the collector
}

// trade is what a round sells.
type trade struct {
	in, out     config.Token
	bucket      string
	amount      *big.Int
	slippageBps int
}

// entry is one aggregator's part of a round.
type entry struct {
	q     store.Quote
	quote *aggregator.Quote // nil unless the status is ok
	swap  sim.Swap
	t0Ok  bool // the T0 simulation succeeded, so T30 runs
}

// Run runs one round. An error means the round was dropped because something
// on our side failed (RPC, slots API); no aggregator is judged by such a round.
func (r *Runner) Run(ctx context.Context) (*store.Round, error) {
	t := r.pick()
	taker := randomAddress()
	started := time.Now()

	// T0 is the block the aggregators are asked at.
	b0, err := r.Sim.Pin(ctx)
	if err != nil {
		return nil, fmt.Errorf("pinning T0: %w", err)
	}
	t0At := time.Now()
	req := aggregator.Request{
		ChainID: r.Chain.ID, TokenIn: t.in.Address, TokenOut: t.out.Address,
		TokenInDecimals: t.in.Decimals, TokenOutDecimals: t.out.Decimals,
		Amount: t.amount.String(), Taker: taker, SlippageBps: t.slippageBps,
		GasPrice: b0.GasPrice.String(), IntegratorID: r.Config.IntegratorID,
	}
	entries := r.quoteAll(ctx, req)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var quoted []*entry
	for _, e := range entries {
		if e.quote != nil {
			quoted = append(quoted, e)
		}
	}
	if err := r.prepareSwaps(ctx, b0, quoted, t, taker); err != nil {
		return nil, err
	}
	if err := each(quoted, func(e *entry) error { return r.simulateT0(ctx, b0, e) }); err != nil {
		return nil, fmt.Errorf("simulating at T0: %w", err)
	}

	// T30: the same transactions with the same gas limits, as if sent staleDelay after T0.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Until(t0At.Add(r.Config.StaleDelay.Duration))):
	}
	b30, err := r.Sim.Pin(ctx)
	if err != nil {
		return nil, fmt.Errorf("pinning T30: %w", err)
	}
	err = each(quoted, func(e *entry) error {
		if !e.t0Ok {
			return nil
		}
		res, err := r.Sim.Run(ctx, b30, e.swap, e.q.GasLimit)
		if err != nil {
			return err
		}
		e.q.T30 = &store.Simulation{Success: res.Success, AmountOut: res.AmountOut, Gas: res.Gas, Revert: res.Revert}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("simulating at T30: %w", err)
	}

	out := &store.Round{
		Region: r.Region, ChainID: r.Chain.ID, TokenIn: strings.ToLower(t.in.Address), TokenOut: strings.ToLower(t.out.Address),
		TokenInNative: t.in.CountsAsNative(), TokenOutNative: t.out.CountsAsNative(),
		AmountIn: t.amount, SizeBucket: t.bucket, SlippageBps: t.slippageBps, Taker: taker, StartedAt: started,
		T0Block: b0.Number, T0GasPrice: b0.GasPrice, T30Block: b30.Number, T30GasPrice: b30.GasPrice,
		MethodologyVersion: r.Config.MethodologyVersion, CollectorVersion: r.Version,
	}
	for _, e := range entries {
		out.Quotes = append(out.Quotes, e.q)
	}
	return out, nil
}

// quoteAll asks every aggregator at once and waits for all of them.
func (r *Runner) quoteAll(ctx context.Context, req aggregator.Request) []*entry {
	entries := make([]*entry, len(r.Aggregators))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, agg := range r.Aggregators {
		entries[i] = &entry{q: store.Quote{Aggregator: agg.ID()}}
		wg.Go(func() {
			<-start // nobody gets a head start
			r.quote(ctx, agg, req, entries[i])
		})
	}
	close(start)
	wg.Wait()
	return entries
}

func (r *Runner) quote(ctx context.Context, agg aggregator.Aggregator, req aggregator.Request, e *entry) {
	timeout := r.Config.QuoteTimeout.Duration
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var sw upstream.Stopwatch // the time the API took, without our connection setup
	q, err := agg.Quote(upstream.WithStopwatch(qctx, &sw), req)
	e.q.Latency = sw.Elapsed()

	var ae *aggregator.Error
	switch {
	case err == nil:
		if verr := q.Validate(req); verr != nil {
			e.q.Status, e.q.Error = string(aggregator.CodeInvalid), verr.Error()
		} else {
			e.q.Status, e.quote = "ok", q
		}
		e.q.QuotedOut, _ = new(big.Int).SetString(q.AmountOut, 10)
		e.q.TxTo, e.q.TxData, e.q.Spender, e.q.Raw = strings.ToLower(q.Tx.To), q.Tx.Data, strings.ToLower(q.Spender), q.Raw
		e.q.TxValue, _ = new(big.Int).SetString(q.Tx.Value, 10)
	case errors.As(err, &ae):
		e.q.Status, e.q.Error = string(ae.Code), ae.Message
	case qctx.Err() == context.DeadlineExceeded:
		e.q.Status, e.q.Error, e.q.Latency = "timeout", fmt.Sprintf("no answer within %s", timeout), timeout
	default:
		e.q.Status, e.q.Error = string(aggregator.CodeError), err.Error()
	}
}

// prepareSwaps turns the quotes into simulation inputs.
func (r *Runner) prepareSwaps(ctx context.Context, b0 sim.Block, quoted []*entry, t trade, taker string) error {
	sale := Sale{ChainID: r.Chain.ID, TokenIn: t.in.Address, TokenOut: t.out.Address, AmountIn: t.amount, Taker: taker}
	txs := make([]Tx, len(quoted))
	for i, e := range quoted {
		txs[i] = Tx{To: e.quote.Tx.To, Data: e.quote.Tx.Data, Value: e.quote.Tx.Value, Spender: e.quote.Spender}
	}
	swaps, err := Swaps(ctx, r.Sim, r.Slots, b0, sale, txs)
	if err != nil {
		return err
	}
	for i, e := range quoted {
		e.swap = swaps[i]
	}
	return nil
}

// Sale is what every transaction of a round sells.
type Sale struct {
	ChainID           uint64
	TokenIn, TokenOut string
	AmountIn          *big.Int
	Taker             string
}

// Tx is an aggregator's transaction: hex data, decimal value, and the spender
// an ERC-20 input is approved to.
type Tx struct{ To, Data, Value, Spender string }

// Swaps turns a round's transactions into simulation inputs: the taker holds
// exactly AmountIn of TokenIn, approved to each transaction's spender, and its
// TokenOut balance before every swap is the one at b0. It fetches the storage
// keys once per spender and that balance once. resim builds its inputs with
// it too, so a re-simulation starts from the same state as the round.
func Swaps(ctx context.Context, s *sim.Simulator, sl *slots.Client, b0 sim.Block, sale Sale, txs []Tx) ([]sim.Swap, error) {
	if len(txs) == 0 {
		return nil, nil
	}
	var outBefore *big.Int
	if !aggregator.IsNative(sale.TokenOut) {
		var err error
		if outBefore, err = s.Balance(ctx, b0, sale.TokenOut, sale.Taker); err != nil {
			return nil, err
		}
	}
	keys := map[string]slots.Keys{}
	swaps := make([]sim.Swap, len(txs))
	for i, tx := range txs {
		var k slots.Keys
		if !aggregator.IsNative(sale.TokenIn) {
			var ok bool
			if k, ok = keys[tx.Spender]; !ok {
				var err error
				if k, err = sl.Get(ctx, sale.ChainID, sale.TokenIn, sale.Taker, tx.Spender); err != nil {
					return nil, fmt.Errorf("slots of %s: %w", sale.TokenIn, err)
				}
				keys[tx.Spender] = k
			}
		}
		data, err := hex.DecodeString(strings.TrimPrefix(tx.Data, "0x"))
		if err != nil {
			return nil, fmt.Errorf("tx data: %w", err)
		}
		value, ok := new(big.Int).SetString(tx.Value, 10)
		if !ok {
			return nil, fmt.Errorf("tx value %q", tx.Value)
		}
		swaps[i] = sim.Swap{
			Taker: sale.Taker, TokenIn: sale.TokenIn, TokenOut: sale.TokenOut, AmountIn: sale.AmountIn, Slots: k,
			To: tx.To, Data: data, Value: value, OutBefore: outBefore,
		}
	}
	return swaps, nil
}

// simulateT0 runs the swap with the gas limit a wallet would send it with.
func (r *Runner) simulateT0(ctx context.Context, b sim.Block, e *entry) error {
	limit, res, err := r.Sim.Measure(ctx, b, e.swap)
	if err != nil {
		return err
	}
	e.q.GasLimit = limit
	e.q.T0 = &store.Simulation{Success: res.Success, AmountOut: res.AmountOut, Gas: res.Gas, Revert: res.Revert}
	e.t0Ok = res.Success
	return nil
}

// pick draws the round's trade: a pair and a size bucket by their weight, a
// direction, and the amount of that bucket × a log-uniform factor in
// [0.8, 1.25], so that no trade can be prepared for.
func (r *Runner) pick() trade {
	total := 0
	for _, p := range r.Chain.Pairs {
		for _, w := range p.Sizes {
			total += w
		}
	}
	n := mrand.IntN(total)
	var pair config.Pair
	var bucket string
	for _, p := range r.Chain.Pairs {
		for b, w := range p.Sizes {
			if n -= w; n < 0 && bucket == "" {
				pair, bucket = p, b
			}
		}
	}
	in, out := r.Chain.Tokens[pair.A], r.Chain.Tokens[pair.B]
	if mrand.IntN(2) == 1 {
		in, out = out, in
	}

	factor := math.Exp(math.Log(0.8) + mrand.Float64()*(math.Log(1.25)-math.Log(0.8)))
	amount := new(big.Int).Mul(in.Amount(bucket), big.NewInt(int64(math.Round(factor*1e6))))
	amount.Div(amount, big.NewInt(1e6))

	slippage := r.Config.SlippageBps.Default
	if in.Stable && out.Stable {
		slippage = r.Config.SlippageBps.Stable
	}
	return trade{in: in, out: out, bucket: bucket, amount: amount, slippageBps: slippage}
}

// randomAddress is a fresh taker: an address nobody controls or has used.
func randomAddress() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return "0x" + hex.EncodeToString(b)
}

// each runs f for every entry at once and returns the errors.
func each(entries []*entry, f func(*entry) error) error {
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Go(func() { errs[i] = f(e) })
	}
	wg.Wait()
	return errors.Join(errs...)
}
