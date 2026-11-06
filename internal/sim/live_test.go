//go:build live

package sim

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"slices"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// TestLiveTransferGas measures a plain USDC transfer on Base, whose gas is
// known, to check the gas accounting against a real node:
//
//	RPC_8453=https://mainnet.base.org SLOTS_API_URL=https://api.nordstern.finance/slots go test -tags live ./internal/sim
func TestLiveTransferGas(t *testing.T) {
	url, slotsURL := os.Getenv("RPC_8453"), os.Getenv("SLOTS_API_URL")
	if url == "" || slotsURL == "" {
		t.Skip("RPC_8453 and SLOTS_API_URL are required")
	}
	ctx := context.Background()
	hc := upstream.NewHTTPClient()
	s := New(rpc.New(url, hc), config.Chain{ID: 8453, MaxTxGas: 16_777_216}, 1.5)
	if err := s.CheckNode(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := s.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const sender, receiver = "0x7e57000000000000000000000000000000000a01", "0x7e57000000000000000000000000000000000a02"
	keys, err := slots.New(upstream.New(hc, slotsURL, http.Header{"Referer": {upstream.Referer}})).
		Get(ctx, 8453, usdc, sender, usdc)
	if err != nil {
		t.Fatal(err)
	}
	// transfer(receiver, 1): the receiver's balance goes from 0 to 1, the usual cost of paying a new address.
	data := slices.Concat([]byte{0xa9, 0x05, 0x9c, 0xbb}, addressWord(receiver), word(big.NewInt(1)))
	sw := Swap{Taker: sender, TokenIn: usdc, TokenOut: usdc, AmountIn: big.NewInt(1_000_000), Slots: keys, To: usdc, Data: data, Value: new(big.Int)}
	r, err := s.Run(ctx, b, sw, s.maxTxGas)
	if err != nil {
		t.Fatal(err)
	}
	// The node's own estimate for the same transfer, sent by the sender as a plain
	// account. An estimate is the smallest gas limit that works, which is a little
	// above the gas used (EIP-2200 wants 2,300 gas left at every SSTORE).
	var est string
	call := map[string]any{"from": sender, "to": usdc, "data": "0x" + hex.EncodeToString(data)}
	ov := map[string]any{usdc: map[string]any{"stateDiff": map[string]string{keys.Balance: "0x" + fmt.Sprintf("%064x", 1_000_000)}}}
	if err := rpc.New(url, hc).Call(ctx, &est, "eth_estimateGas", call, hexBig(new(big.Int).SetUint64(b.Number)), ov); err != nil {
		t.Fatal(err)
	}
	estimate, _ := parseHexBig(est)
	t.Logf("block %d, gas price %s wei, transfer gas %d (node estimate %s)", b.Number, b.GasPrice, r.Gas, estimate)
	if !r.Success || r.Gas > estimate.Uint64() || r.Gas+3_000 < estimate.Uint64() {
		t.Errorf("transfer: %+v, node estimate %s", r, estimate)
	}
}
