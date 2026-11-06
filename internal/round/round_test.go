package round

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/sim"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

const (
	usdc   = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	router = "0x6131b5fae19ea4f9d964eac0408e4408b66337b5"
)

// fakeAgg answers with quote(req).
type fakeAgg struct {
	id    string
	quote func(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error)
}

func (f fakeAgg) ID() string { return f.id }
func (f fakeAgg) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	return f.quote(ctx, req)
}

func swapQuote(req aggregator.Request) *aggregator.Quote {
	q := &aggregator.Quote{AmountOut: "1000", Tx: aggregator.Tx{To: router, Data: "0x12345678", Value: "0"}, Spender: router, Raw: []byte(`{}`)}
	if aggregator.IsNative(req.TokenIn) {
		q.Tx.Value, q.Spender = req.Amount, ""
	}
	return q
}

// node is a fake chain: the head moves from block 0x10 to 0x20 between the
// two pins, and every swap delivers 990 at T0 and reverts at T30.
type node struct {
	mu      sync.Mutex
	pins    int
	calls   []map[string]any // swap eth_calls: gas and block
	methods map[string]int   // every call by kind, to hold the RPC budget
	fail    string           // method that answers with an error the node, not the swap, caused
}

func (n *node) serve(t *testing.T) *rpc.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.methods == nil {
			n.methods = map[string]int{}
		}
		n.methods[req.Method]++
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case n.fail:
			resp["error"] = map[string]any{"code": -32000, "message": "header not found"}
		case "eth_feeHistory": // one block: the head, 0x10 at the first pin and 0x20 at the second
			n.pins++
			resp["result"] = map[string]any{"oldestBlock": "0x" + strconv.Itoa(n.pins*10), "baseFeePerGas": []string{"0x64", "0x64"},
				"gasUsedRatio": []float64{0.5}, "reward": [][]string{{"0x2"}}}
		case "eth_call":
			var call map[string]string
			var block string
			_ = json.Unmarshal(req.Params[0], &call)
			_ = json.Unmarshal(req.Params[1], &block)
			if strings.HasPrefix(call["data"], "0x70a08231") { // the taker's balance before a swap
				n.methods["balanceOf"]++
				resp["result"] = "0x" + hex.EncodeToString(make([]byte, 32))
				break
			}
			n.calls = append(n.calls, map[string]any{"gas": call["gas"], "block": block})
			resp["result"] = proxyReturn(block == "0x10", 990, 120_000)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return rpc.New(srv.URL, srv.Client())
}

func proxyReturn(success bool, out, gas int64) string {
	w := func(v int64) []byte { return big.NewInt(v).FillBytes(make([]byte, 32)) }
	ok := int64(0)
	if success {
		ok = 1
	}
	return "0x" + hex.EncodeToString(slices.Concat(w(ok), w(out), w(gas), w(128), w(0)))
}

func runner(t *testing.T, n *node, aggs ...aggregator.Aggregator) *Runner {
	t.Helper()
	cfg, err := config.Parse([]byte(`{"methodologyVersion":"test","roundsPerMinute":1,"quoteTimeout":"200ms","staleDelay":"50ms",
		"gasLimitMultiplier":1.5,"slippageBps":{"default":50,"stable":10},"integratorId":"dexbench","aggregators":["x"],
		"chains":[{"id":8453,"name":"Base","maxTxGas":16777216,"weight":1,
		"tokens":{"ETH":{"address":"0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE","decimals":18,"sizes":{"1k":"0.3"}},
		          "USDC":{"address":"` + usdc + `","decimals":6,"stable":true,"sizes":{"1k":"1000"}}},
		"pairs":[{"a":"ETH","b":"USDC","sizes":{"1k":1}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	slotsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"balance_slot_overwrite":"1","approval_slot_overwrite":"2"}`))
	}))
	t.Cleanup(slotsAPI.Close)
	return &Runner{
		Config: cfg, Chain: cfg.Chains[0], Sim: sim.New(n.serve(t), cfg.Chains[0], cfg.GasLimitMultiplier), Slots: slots.New(upstream.New(slotsAPI.Client(), slotsAPI.URL, nil)),
		Aggregators: aggs, Region: "test", Version: "dev",
	}
}

func TestRound(t *testing.T) {
	var asked sync.Map // aggregator -> request
	agg := func(id string, f func(context.Context, aggregator.Request) (*aggregator.Quote, error)) aggregator.Aggregator {
		return fakeAgg{id: id, quote: func(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
			asked.Store(id, req)
			return f(ctx, req)
		}}
	}
	n := &node{}
	r := runner(t, n,
		agg("good", func(_ context.Context, req aggregator.Request) (*aggregator.Quote, error) { return swapQuote(req), nil }),
		agg("noroute", func(context.Context, aggregator.Request) (*aggregator.Quote, error) {
			return nil, aggregator.Errorf(aggregator.CodeNoRoute, "no route found")
		}),
		agg("slow", func(ctx context.Context, _ aggregator.Request) (*aggregator.Quote, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		agg("badvalue", func(_ context.Context, req aggregator.Request) (*aggregator.Quote, error) {
			q := swapQuote(req)
			q.Tx.Value = "1" + q.Tx.Value // never exactly what is sold
			return q, nil
		}),
	)
	rd, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rd.T0Block != 0x10 || rd.T30Block != 0x20 || rd.T0GasPrice.Int64() != 102 {
		t.Errorf("blocks %d / %d, gas price %v", rd.T0Block, rd.T30Block, rd.T0GasPrice)
	}

	// Every aggregator got the same request, with the T0 gas price.
	var first aggregator.Request
	asked.Range(func(_, v any) bool {
		req := v.(aggregator.Request)
		if first.Taker == "" {
			first = req
		} else if req != first {
			t.Errorf("requests differ: %+v vs %+v", req, first)
		}
		return true
	})
	if first.GasPrice != "102" || first.SlippageBps != 50 || first.IntegratorID != "dexbench" {
		t.Errorf("request = %+v", first)
	}

	status := map[string]string{}
	for _, q := range rd.Quotes {
		status[q.Aggregator] = q.Status
	}
	if status["good"] != "ok" || status["noroute"] != "no_route" || status["slow"] != "timeout" || status["badvalue"] != "invalid" {
		t.Errorf("statuses = %v", status)
	}

	good := rd.Quotes[0]
	// Measured − the proxy's CALL overhead + 21,000 + the 4-byte calldata.
	wantGas := uint64(120_000 - 2_600 + 21_000 + 4*16)
	if aggregator.IsNative(rd.TokenIn) {
		wantGas -= 6_700 // the value transfer the transaction's 21,000 covers
	}
	if good.T0 == nil || !good.T0.Success || good.T0.AmountOut.Int64() != 990 || good.T0.Gas != wantGas {
		t.Fatalf("T0 = %+v", good.T0)
	}
	if want := uint64(math.Ceil(1.5 * float64(wantGas))); good.GasLimit != want {
		t.Errorf("gas limit = %d, want %d", good.GasLimit, want)
	}
	if good.T30 == nil || good.T30.Success {
		t.Errorf("T30 = %+v", good.T30)
	}
	// Probe and measured run at T0, then one run at T30 with the same gas as the measured run.
	if len(n.calls) != 3 || n.calls[0]["block"] != "0x10" || n.calls[1]["block"] != "0x10" || n.calls[2]["block"] != "0x20" ||
		n.calls[0]["gas"] != "0x1000000" || n.calls[1]["gas"] != n.calls[2]["gas"] {
		t.Errorf("simulations = %v", n.calls)
	}
	for _, q := range rd.Quotes[1:] {
		if q.T0 != nil || q.T30 != nil {
			t.Errorf("%s was simulated", q.Aggregator)
		}
	}

	// The RPC budget of a round: one pin per block, the output balance once, and per
	// usable quote a probe, the measured run and the T30 run.
	want := map[string]int{"eth_feeHistory": 2, "eth_call": 3}
	if !aggregator.IsNative(rd.TokenOut) {
		want["eth_call"], want["balanceOf"] = 4, 1 // balanceOf is one of the eth_calls
	}
	if !maps.Equal(n.methods, want) {
		t.Errorf("RPC calls = %v, want %v", n.methods, want)
	}
}

func TestNodeFailureDropsRound(t *testing.T) {
	ok := fakeAgg{id: "good", quote: func(_ context.Context, req aggregator.Request) (*aggregator.Quote, error) { return swapQuote(req), nil }}
	for _, method := range []string{"eth_feeHistory", "eth_call"} {
		if _, err := runner(t, &node{fail: method}, ok).Run(context.Background()); err == nil {
			t.Errorf("%s failing: the round was not dropped", method)
		}
	}
}

func TestPick(t *testing.T) {
	r := runner(t, &node{})
	seen := map[string]bool{}
	for range 200 {
		tr := r.pick()
		seen[tr.in.Address] = true
		base := tr.in.Amount(tr.bucket)
		lo := new(big.Int).Div(new(big.Int).Mul(base, big.NewInt(8)), big.NewInt(10))
		hi := new(big.Int).Div(new(big.Int).Mul(base, big.NewInt(125)), big.NewInt(100))
		if tr.amount.Cmp(lo) < 0 || tr.amount.Cmp(hi) > 0 {
			t.Fatalf("amount %s outside [0.8, 1.25] × %s", tr.amount, base)
		}
		if tr.slippageBps != 50 { // ETH/USDC is not a stable pair
			t.Fatalf("slippage %d", tr.slippageBps)
		}
	}
	if len(seen) != 2 {
		t.Errorf("only one direction was picked in 200 rounds")
	}
}

func TestPickWeights(t *testing.T) {
	r := runner(t, &node{})
	ch := r.Chain
	ch.Tokens = map[string]config.Token{}
	for name, tk := range r.Chain.Tokens {
		tk.Sizes = map[string]string{"1k": tk.Sizes["1k"], "10k": map[string]string{"ETH": "3", "USDC": "10000"}[name]}
		ch.Tokens[name] = tk
	}
	ch.Pairs = []config.Pair{{A: "ETH", B: "USDC", Sizes: map[string]int{"1k": 1, "10k": 3}}}
	r.Chain = ch
	n := 0
	for range 4000 {
		if r.pick().bucket == "10k" {
			n++
		}
	}
	if n < 2800 || n > 3200 { // 3000 expected
		t.Errorf("10k picked %d times in 4000, want about 3000", n)
	}
}
