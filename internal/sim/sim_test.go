package sim

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
)

// fakeNode answers JSON-RPC calls with handlers[method].
func fakeNode(t *testing.T, handlers map[string]func(params []json.RawMessage) (any, *rpc.Error)) *rpc.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		h, ok := handlers[req.Method]
		if !ok {
			t.Errorf("unexpected call %s", req.Method)
			return
		}
		res, rerr := h(req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = res
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return rpc.New(srv.URL, srv.Client())
}

// proxyReturn ABI-encodes what SimulationProxy.simulate returns.
func proxyReturn(success bool, amountOut, gasUsed int64, returnData []byte) string {
	ok := int64(0)
	if success {
		ok = 1
	}
	b := slices.Concat(word(big.NewInt(ok)), word(big.NewInt(amountOut)), word(big.NewInt(gasUsed)), word(big.NewInt(128)), bytesTail(returnData))
	return "0x" + hex.EncodeToString(b)
}

var base = config.Chain{ID: 8453, MaxTxGas: 16_777_216, BlockTime: config.Duration{Duration: 2 * time.Second}}

const (
	taker = "0xAbCd000000000000000000000000000000000001"
	usdc  = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
)

func erc20Swap() Swap {
	return Swap{
		Taker: taker, TokenIn: usdc, TokenOut: config.NativeToken, AmountIn: big.NewInt(1_000_000),
		Slots: slots.Keys{Balance: "0x" + strings.Repeat("11", 32), Allowance: "0x" + strings.Repeat("22", 32)},
		To:    "0x6131b5fae19ea4f9d964eac0408e4408b66337b5", Data: []byte{0xe2, 0x1f, 0xd0, 0xe9, 0, 0}, Value: new(big.Int),
	}
}

var block = Block{Number: 0x100, BaseFee: big.NewInt(1000), GasPrice: big.NewInt(1500)}

func TestRunERC20(t *testing.T) {
	var params []json.RawMessage
	node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
		"eth_call": func(p []json.RawMessage) (any, *rpc.Error) {
			params = p
			return proxyReturn(true, 990, 150_000, nil), nil
		},
	})
	r, err := New(node, base, 1.5).Run(context.Background(), block, erc20Swap(), 300_000)
	if err != nil {
		t.Fatal(err)
	}
	// 150,000 measured − 2,600 cold access + 21,000 + 4 non-zero and 2 zero calldata bytes.
	if !r.Success || r.AmountOut.Int64() != 990 || r.Gas != 150_000-2_600+21_000+4*16+2*4 {
		t.Errorf("result = %+v", r)
	}

	// Every field the simulation sets, as the node sees it.
	var call map[string]string
	var blockParam string
	var overrides map[string]map[string]any
	for i, v := range []any{&call, &blockParam, &overrides} {
		if err := json.Unmarshal(params[i], v); err != nil {
			t.Fatal(err)
		}
	}
	callGas := int64(300_000 + 300_000/63 + proxyGas)
	if call["from"] != strings.ToLower(taker) || call["to"] != strings.ToLower(taker) || call["value"] != "0x0" ||
		call["gasPrice"] != "0x5dc" || call["gas"] != hexBig(big.NewInt(callGas)) {
		t.Errorf("call = %v", call)
	}
	wantData := "0x42b5ce9e" + // simulate(to, tokenOut, data)
		"0000000000000000000000006131b5fae19ea4f9d964eac0408e4408b66337b5" +
		"000000000000000000000000eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" +
		"0000000000000000000000000000000000000000000000000000000000000060" +
		"0000000000000000000000000000000000000000000000000000000000000006" +
		"e21fd0e900000000000000000000000000000000000000000000000000000000"
	if call["data"] != wantData {
		t.Errorf("data = %s\nwant   %s", call["data"], wantData)
	}
	if blockParam != "0x100" || len(params) != 3 {
		t.Errorf("block = %s, %d params: a block without its time is simulated in itself", blockParam, len(params))
	}
	takerOv := overrides[strings.ToLower(taker)]
	if takerOv["code"] != proxyCode || takerOv["balance"] != hexBig(big.NewInt(callGas*1500)) {
		t.Errorf("taker override = balance %v", takerOv["balance"])
	}
	amount := "0x" + strings.Repeat("0", 59) + "f4240" // 1,000,000: exactly what is sold
	diff := overrides[strings.ToLower(usdc)]["stateDiff"].(map[string]any)
	if len(diff) != 2 || diff["0x"+strings.Repeat("11", 32)] != amount || diff["0x"+strings.Repeat("22", 32)] != amount {
		t.Errorf("token override = %v", diff)
	}
}

// Selling the native coin for an ERC-20: the call carries the value, and the
// output is the balance after the swap less OutBefore, which the round read
// once. Run itself makes exactly one call.
func TestRunNative(t *testing.T) {
	var call map[string]string
	var overrides map[string]map[string]any
	calls := 0
	node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
		"eth_call": func(p []json.RawMessage) (any, *rpc.Error) {
			calls++
			_ = json.Unmarshal(p[0], &call)
			_ = json.Unmarshal(p[2], &overrides)
			return proxyReturn(true, 5, 100_000, nil), nil // the balance after
		},
	})
	sw := erc20Swap()
	sw.TokenIn, sw.TokenOut, sw.Value, sw.Data, sw.OutBefore = config.NativeToken, usdc, big.NewInt(7e17), nil, big.NewInt(2)
	r, err := New(node, base, 1.5).Run(context.Background(), block, sw, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	callGas := int64(200_000 + 200_000/63 + proxyGas + proxyValueGas)
	funding := new(big.Int).Add(big.NewInt(7e17), big.NewInt(callGas*1500))
	if call["value"] != hexBig(big.NewInt(7e17)) || overrides[strings.ToLower(taker)]["balance"] != hexBig(funding) || len(overrides) != 1 {
		t.Errorf("call = %v, overrides = %v", call, overrides)
	}
	if calls != 1 || r.AmountOut.Int64() != 3 {
		t.Errorf("%d calls, amountOut = %v; want 1 call, 5 - 2", calls, r.AmountOut)
	}
	if r.Gas != 100_000-2_600-6_700+21_000 {
		t.Errorf("gas = %d", r.Gas)
	}
}

func TestBalance(t *testing.T) {
	node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
		"eth_call": func(p []json.RawMessage) (any, *rpc.Error) {
			var c map[string]string
			_ = json.Unmarshal(p[0], &c)
			if c["to"] != usdc || c["data"] != "0x70a08231"+hex.EncodeToString(addressWord(taker)) || string(p[1]) != `"0x100"` || len(p) != 2 {
				t.Errorf("balanceOf call = %s", p)
			}
			return "0x" + hex.EncodeToString(word(big.NewInt(7))), nil
		},
	})
	if v, err := New(node, base, 1.5).Balance(context.Background(), block, usdc, taker); err != nil || v.Int64() != 7 {
		t.Errorf("balance = %v, %v", v, err)
	}
}

func TestRunFailures(t *testing.T) {
	errorString := "08c379a0" + "0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000013" +
		hex.EncodeToString([]byte("Too little received")) + strings.Repeat("00", 13)
	revertData, _ := hex.DecodeString(errorString)
	for name, tc := range map[string]struct {
		result   any
		rpcErr   *rpc.Error
		wantErr  bool
		wantText string
	}{
		"swap reverts":      {result: proxyReturn(false, 0, 80_000, revertData), wantText: "Too little received"},
		"panic":             {result: proxyReturn(false, 0, 1, append([]byte{0x4e, 0x48, 0x7b, 0x71}, word(big.NewInt(0x11))...)), wantText: "panic 0x11"},
		"custom error":      {result: proxyReturn(false, 0, 1, []byte{0xde, 0xad, 0xbe, 0xef}), wantText: "0xdeadbeef"},
		"proxy out of gas":  {rpcErr: &rpc.Error{Code: -32000, Message: "out of gas"}, wantText: "out of gas"},
		"node lost state":   {rpcErr: &rpc.Error{Code: -32000, Message: "missing trie node abc"}, wantErr: true},
		"override ignored":  {result: "0x", wantErr: true},
		"node rejects call": {rpcErr: &rpc.Error{Code: -32602, Message: "invalid argument"}, wantErr: true},
	} {
		node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
			"eth_call": func([]json.RawMessage) (any, *rpc.Error) { return tc.result, tc.rpcErr },
		})
		r, err := New(node, base, 1.5).Run(context.Background(), block, erc20Swap(), 300_000)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: no error, result %+v", name, r)
			}
			continue
		}
		if err != nil || r.Success || r.Revert != tc.wantText {
			t.Errorf("%s: result %+v, err %v; want revert %q", name, r, err, tc.wantText)
		}
	}
}

// One eth_feeHistory call names the latest block, its base fee and the tips.
func TestPin(t *testing.T) {
	calls := 0
	node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
		"eth_feeHistory": func(p []json.RawMessage) (any, *rpc.Error) {
			calls++
			if string(p[0]) != `"0x14"` || string(p[1]) != `"latest"` || string(p[2]) != `[50]` {
				t.Errorf("feeHistory params = %s", p)
			}
			return map[string]any{
				"oldestBlock":   "0x26",                                                 // blocks 38 to 42
				"baseFeePerGas": []string{"0x1", "0x1", "0x1", "0x1", "0x3e8", "0x400"}, // the last is block 43's
				"gasUsedRatio":  []float64{0.5, 0.5, 0.5, 0.5, 0.5},
				"reward":        [][]string{{"0x5"}, {"0x1"}, {"0x64"}, {"0x2"}, {"0x3"}},
			}, nil
		},
		"eth_getBlockByNumber": header,
	})
	b, err := New(node, base, 1.5).Pin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || b.Number != 42 || b.Time != 0x6900 || b.BaseFee.Int64() != 1000 || b.GasPrice.Int64() != 1003 { // median tip of 1, 2, 3, 5, 100
		t.Errorf("%d calls, block = %+v", calls, b)
	}
}

// Without EIP-1559 the base fee is 0 and the "tips" are the gas prices paid.
func TestPinLegacyChain(t *testing.T) {
	node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
		"eth_feeHistory": func([]json.RawMessage) (any, *rpc.Error) {
			return map[string]any{"oldestBlock": "0x2a", "gasUsedRatio": []float64{0.5}, "reward": [][]string{{"0x77"}}}, nil
		},
		"eth_getBlockByNumber": header,
	})
	b, err := New(node, base, 1.5).Pin(context.Background())
	if err != nil || b.Number != 42 || b.BaseFee.Sign() != 0 || b.GasPrice.Int64() != 0x77 {
		t.Errorf("block = %+v, %v", b, err)
	}
}

// header answers eth_getBlockByNumber for block 42 (0x2a), at time 0x6900.
func header(p []json.RawMessage) (any, *rpc.Error) {
	var n string
	if err := json.Unmarshal(p[0], &n); err != nil || n != "0x2a" {
		return nil, &rpc.Error{Code: -32000, Message: "header not found"}
	}
	return map[string]string{"number": "0x2a", "timestamp": "0x6900"}, nil
}

// A swap runs in the block after the pinned one: its number + 1, its time + the
// block time.
func TestRunNextBlock(t *testing.T) {
	var next map[string]string
	node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
		"eth_call": func(p []json.RawMessage) (any, *rpc.Error) {
			if len(p) != 4 || json.Unmarshal(p[3], &next) != nil {
				return nil, &rpc.Error{Code: -32602, Message: "no block overrides"}
			}
			return proxyReturn(true, 990, 150_000, nil), nil
		},
	})
	b := block
	b.Time = 0x6900
	if _, err := New(node, base, 1.5).Run(context.Background(), b, erc20Swap(), 300_000); err != nil {
		t.Fatal(err)
	}
	if len(next) != 2 || next["number"] != "0x101" || next["time"] != "0x6902" {
		t.Errorf("block overrides = %v", next)
	}
}

func TestNextBlockSeconds(t *testing.T) {
	for _, tc := range []struct {
		blockTime time.Duration
		want      uint64
	}{{12 * time.Second, 12}, {1500 * time.Millisecond, 2}, {time.Second, 1}, {200 * time.Millisecond, 1}} {
		if got := nextBlockSeconds(tc.blockTime); got != tc.want {
			t.Errorf("nextBlockSeconds(%v) = %d, want %d", tc.blockTime, got, tc.want)
		}
	}
}

func TestGasLimit(t *testing.T) {
	if got := gasLimit(200_001, 1.5, 16_777_216); got != 300_002 {
		t.Errorf("gasLimit = %d", got)
	}
	if got := gasLimit(15_000_000, 1.5, 16_777_216); got != 16_777_216 {
		t.Errorf("gasLimit above the cap = %d", got)
	}
}

func TestMeasure(t *testing.T) {
	for _, probeOk := range []bool{true, false} {
		var gas []string
		node := fakeNode(t, map[string]func([]json.RawMessage) (any, *rpc.Error){
			"eth_call": func(p []json.RawMessage) (any, *rpc.Error) {
				var call map[string]string
				_ = json.Unmarshal(p[0], &call)
				gas = append(gas, call["gas"])
				return proxyReturn(probeOk, 5, 100_000, nil), nil
			},
		})
		limit, r, err := New(node, base, 1.5).Measure(context.Background(), block, erc20Swap())
		if err != nil {
			t.Fatal(err)
		}
		if !probeOk {
			if limit != 16_777_216 || r.Success || !slices.Equal(gas, []string{"0x1000000"}) {
				t.Errorf("failed probe: limit %d, %+v, gas %v", limit, r, gas)
			}
			continue
		}
		// txGas = 100,000 − 2,600 + 21,000 + 72 calldata gas = 118,472; × 1.5 = 177,708.
		// The measured run's eth_call adds 177,708/63 and the proxy's 15,000: 195,528 = 0x2fbc8.
		if limit != 177_708 || !r.Success || !slices.Equal(gas, []string{"0x1000000", "0x2fbc8"}) {
			t.Errorf("limit %d, %+v, gas %v", limit, r, gas)
		}
	}
}

// TestProxyBytecode checks that the simulation runs what contracts/SimulationProxy.sol compiles to.
func TestProxyBytecode(t *testing.T) {
	built, err := os.ReadFile("../../contracts/SimulationProxy.bin-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if "0x"+strings.TrimSpace(string(built)) != proxyCode {
		t.Error("proxyCode is not contracts/SimulationProxy.bin-runtime: run contracts/build.sh and update proxy.go")
	}
}
