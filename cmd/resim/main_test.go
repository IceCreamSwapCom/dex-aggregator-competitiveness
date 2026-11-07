package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// node is a fake chain: the taker holds 10 of tokenOut before the swap, every
// swap leaves it with 1010 at block 0x10 and reverts at 0x20. Every block's
// timestamp is 0x1000. It records the block overrides of the swaps.
type node struct {
	mu   sync.Mutex
	next []string // number@time, "@" for none
}

func (n *node) serve(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "eth_getBlockByNumber" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]string{"timestamp": "0x1000"}})
			return
		}
		var call map[string]string
		var block string
		_ = json.Unmarshal(req.Params[0], &call)
		_ = json.Unmarshal(req.Params[1], &block)
		word := func(v int64) []byte { return big.NewInt(v).FillBytes(make([]byte, 32)) }
		var result []byte
		switch {
		case strings.HasPrefix(call["data"], "0x70a08231"): // balanceOf
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x" + hex.EncodeToString(word(10))})
			return
		case block == "0x10":
			result = slices.Concat(word(1), word(1010), word(120_000), word(128), word(0))
		default:
			result = slices.Concat(word(0), word(0), word(120_000), word(128), word(0))
		}
		var next map[string]string
		if len(req.Params) > 3 {
			_ = json.Unmarshal(req.Params[3], &next)
		}
		n.mu.Lock()
		n.next = append(n.next, next["number"]+"@"+next["time"])
		n.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x" + hex.EncodeToString(result)})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestResimulate(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"methodologyVersion":"test","roundsPerMinute":1,"quoteTimeout":"5s","staleDelay":"30s",
		"gasLimitMultiplier":1.5,"slippageBps":{"default":50,"stable":10},"aggregators":["x"],
		"chains":[{"id":8453,"name":"Base","maxTxGas":16777216,"blockTime":"2s","weight":1,
		"tokens":{"ETH":{"address":"0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE","decimals":18,"sizes":{"1k":"0.3"}},
		          "USDC":{"address":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","decimals":6,"sizes":{"1k":"1000"}}},
		"pairs":[{"a":"ETH","b":"USDC","sizes":{"1k":1}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	slotsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"balance_slot_overwrite":"1","approval_slot_overwrite":"2"}`))
	}))
	t.Cleanup(slotsAPI.Close)
	sl := slots.New(upstream.New(slotsAPI.Client(), slotsAPI.URL, nil))
	chain := &node{}
	rpcURL := chain.serve(t)

	// What the collector stored for the same chain: 1000 out, 120,000 measured gas
	// − 2,600 + 21,000 + 64 calldata gas = 138,464, a limit of 1.5 × that.
	stored := func() *api.Round {
		return &api.Round{
			ID: "r1", ChainID: 8453, TokenIn: "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", TokenOut: "0xfde4c96c8593536e31f229ea8f37b2ada2699bb2",
			AmountIn: "1000000", Taker: "0x00000000000000000000000000000000000a0001", T0Block: 0x10, T0GasPrice: "7", T30Block: 0x20,
			T30GasPrice: "8", MethodologyVersion: "test",
			Quotes: []api.Quote{
				{Aggregator: "a", Status: "ok", TxTo: "0x6131b5fae19ea4f9d964eac0408e4408b66337b5", TxData: "0x12345678", TxValue: "0",
					Spender: "0x6131b5fae19ea4f9d964eac0408e4408b66337b5", GasLimit: 207_696,
					T0:  &api.Simulation{Success: true, AmountOut: "1000", Gas: 138_464},
					T30: &api.Simulation{Success: false, AmountOut: "0", Gas: 138_464, Revert: "reverted without data"}},
				{Aggregator: "b", Status: "no_route"},
			},
		}
	}

	var out bytes.Buffer
	diffs, err := resimulate(context.Background(), &out, cfg, stored(), sl, func(uint64) string { return rpcURL })
	if err != nil || diffs != 0 {
		t.Fatalf("diffs %d, %v\n%s", diffs, err, out.String())
	}
	if strings.Count(out.String(), " same ") != 2 {
		t.Errorf("output:\n%s", out.String())
	}
	// Probe and measured run at T0, the run at T30: each in the block after its own, 2 s later.
	if want := []string{"0x11@0x1002", "0x11@0x1002", "0x21@0x1002"}; !slices.Equal(chain.next, want) {
		t.Errorf("block overrides %v, want %v", chain.next, want)
	}

	// A round of methodology 0.2 ran in its pinned blocks, and is re-simulated there.
	chain.next = nil
	rd := stored()
	rd.MethodologyVersion = "0.2"
	if diffs, err := resimulate(context.Background(), &out, cfg, rd, sl, func(uint64) string { return rpcURL }); err != nil || diffs != 0 {
		t.Fatalf("0.2: diffs %d, %v", diffs, err)
	}
	if want := []string{"@", "@", "@"}; !slices.Equal(chain.next, want) {
		t.Errorf("0.2: block overrides %v", chain.next)
	}

	rd = stored()
	rd.Quotes[0].T0.AmountOut = "1001" // the collector stored something else
	out.Reset()
	if diffs, err := resimulate(context.Background(), &out, cfg, rd, sl, func(uint64) string { return rpcURL }); err != nil || diffs != 1 {
		t.Fatalf("diffs %d, %v\n%s", diffs, err, out.String())
	}
	if !strings.Contains(out.String(), "DIFF  stored ok out 1001") || !strings.Contains(out.String(), "now    ok out 1000") {
		t.Errorf("output:\n%s", out.String())
	}

	rd = stored()
	rd.ChainID = 1
	if _, err := resimulate(context.Background(), &out, cfg, rd, sl, func(uint64) string { return rpcURL }); err == nil {
		t.Error("a chain outside the config was re-simulated")
	}
}
