package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
)

// trade is one swap a router logged. Tokens are lower case, the native coin
// as config.NativeToken; amounts are base units.
type trade struct {
	in, out, receiver   string
	amountIn, amountOut float64
}

// router is an aggregator contract that logs every swap with both tokens and
// both amounts. Both are deployed at the same address on every chain they
// serve. LI.FI is read first: it also routes through KyberSwap, and of two
// events in one transaction the outer one is the user's trade.
type router struct {
	name, address, topic string
	decode               func(words [][]byte) (trade, bool)
}

var routers = []router{
	// LiFiGenericSwapCompleted(bytes32 indexed transactionId, string integrator, string referrer,
	//   address receiver, address fromAssetId, address toAssetId, uint256 fromAmount, uint256 toAmount)
	{"lifi", "0x1231deb6f5749ef6ce6943a275a1d3e7486f4eae",
		"0x38eee76fd911eabac79da7af16053e809be0e12c8637f156e77e1af309b99537",
		func(w [][]byte) (trade, bool) {
			if len(w) < 7 {
				return trade{}, false
			}
			return trade{receiver: address(w[2]), in: address(w[3]), out: address(w[4]), amountIn: amount(w[5]), amountOut: amount(w[6])}, true
		}},
	// Swapped(address sender, address srcToken, address dstToken, address dstReceiver,
	//   uint256 spentAmount, uint256 returnAmount) of MetaAggregationRouterV2
	{"kyberswap", "0x6131b5fae19ea4f9d964eac0408e4408b66337b5",
		"0xd6d4f5681c246c9f42c203e287975af1601f8df8035a9251f79aab5c8f09e2f8",
		func(w [][]byte) (trade, bool) {
			if len(w) < 6 {
				return trade{}, false
			}
			return trade{in: address(w[1]), out: address(w[2]), receiver: address(w[3]), amountIn: amount(w[4]), amountOut: amount(w[5])}, true
		}},
}

// address reads an ABI word as an address; the zero address, which LI.FI uses
// for the native coin, becomes config.NativeToken.
func address(w []byte) string {
	a := "0x" + hex.EncodeToString(w[12:])
	if a == "0x0000000000000000000000000000000000000000" {
		return strings.ToLower(config.NativeToken)
	}
	return a
}

func amount(w []byte) float64 {
	f, _ := new(big.Float).SetInt(new(big.Int).SetBytes(w)).Float64()
	return f
}

// trades reads every swap the routers logged in [from, to], one per transaction.
func trades(ctx context.Context, c *rpc.Client, chain string, from, to uint64) ([]trade, error) {
	var out []trade
	seen := map[string]bool{}
	for _, r := range routers {
		var code string
		if err := c.Call(ctx, &code, "eth_getCode", r.address, "latest"); err != nil {
			return nil, err
		}
		if len(code) <= 2 {
			slog.Info("router not deployed", "chain", chain, "router", r.name)
			continue
		}
		logs, err := getLogs(ctx, c, chain+"/"+r.name, r.address, r.topic, from, to)
		if err != nil {
			return nil, fmt.Errorf("%s events: %w", r.name, err)
		}
		n := 0
		for _, l := range logs {
			if l.Removed || seen[l.TxHash] {
				continue
			}
			t, ok := r.decode(words(l.Data))
			if !ok || t.amountIn <= 0 || t.amountOut <= 0 || t.in == t.out {
				continue
			}
			seen[l.TxHash] = true
			out = append(out, t)
			n++
		}
		slog.Info("read", "chain", chain, "router", r.name, "events", len(logs), "trades", n)
	}
	return out, nil
}

type logEntry struct {
	TxHash  string `json:"transactionHash"`
	Data    string `json:"data"`
	Removed bool   `json:"removed"`
}

func words(data string) [][]byte {
	b, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil {
		return nil
	}
	var w [][]byte
	for i := 0; i+32 <= len(b); i += 32 {
		w = append(w, b[i:i+32])
	}
	return w
}

// getLogs reads one event of one contract over [from, to] in ranges the node
// accepts: a failed range is halved and retried, a good one grows again.
func getLogs(ctx context.Context, c *rpc.Client, label, address, topic string, from, to uint64) ([]logEntry, error) {
	var out []logEntry
	span, failures := uint64(2_000), 0
	last := time.Now()
	for lo := from; lo <= to; {
		hi := min(lo+span-1, to)
		var logs []logEntry
		err := c.Call(ctx, &logs, "eth_getLogs", map[string]any{
			"address": address, "topics": []string{topic}, "fromBlock": hexNum(lo), "toBlock": hexNum(hi),
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			if failures++; failures > 20 {
				return nil, fmt.Errorf("blocks %d-%d: %w", lo, hi, err)
			}
			span = max(span/2, 1)
			time.Sleep(time.Duration(failures) * time.Second) // public nodes limit the rate
			continue
		}
		out, lo, failures = append(out, logs...), hi+1, 0
		span = min(span*2, 20_000)
		if time.Since(last) > 30*time.Second {
			slog.Info("reading", "events", label, "done", fmt.Sprintf("%.0f%%", 100*float64(lo-from)/float64(to-from+1)))
			last = time.Now()
		}
	}
	return out, nil
}

// blockAt is the first block at or after t. It steps back from head by
// doubling distances, then bisects: it reads only blocks of the last days,
// which a node without archive state still serves.
func blockAt(ctx context.Context, c *rpc.Client, head uint64, t time.Time) (uint64, error) {
	before := func(n uint64) (bool, error) {
		var b *struct {
			Timestamp string `json:"timestamp"`
		}
		if err := c.Call(ctx, &b, "eth_getBlockByNumber", hexNum(n), false); err != nil {
			return false, err
		}
		if b == nil {
			return false, fmt.Errorf("the node has no block %d", n)
		}
		ts, ok := new(big.Int).SetString(strings.TrimPrefix(b.Timestamp, "0x"), 16)
		if !ok {
			return false, fmt.Errorf("block %d has no timestamp", n)
		}
		return ts.Int64() < t.Unix(), nil
	}
	lo, hi := head, head
	for step := uint64(1_000); ; step *= 2 {
		if step >= head {
			lo = 1
			break
		}
		lo = head - step
		ok, err := before(lo)
		if err != nil {
			return 0, err
		}
		if ok {
			break
		}
		hi = lo
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		ok, err := before(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

func hexNum(n uint64) string { return fmt.Sprintf("0x%x", n) }

// tokenMeta reads a token's symbol and decimals.
func tokenMeta(ctx context.Context, c *rpc.Client, token string) (symbol string, decimals int, err error) {
	var dec, sym string
	if err := c.Call(ctx, &dec, "eth_call", map[string]string{"to": token, "data": "0x313ce567"}, "latest"); err != nil {
		return "", 0, fmt.Errorf("decimals(): %w", err)
	}
	d := words(dec)
	if len(d) != 1 {
		return "", 0, errors.New("decimals() returned no number")
	}
	n := new(big.Int).SetBytes(d[0])
	if !n.IsInt64() || n.Int64() > 36 {
		return "", 0, fmt.Errorf("decimals() = %s", n)
	}
	if err := c.Call(ctx, &sym, "eth_call", map[string]string{"to": token, "data": "0x95d89b41"}, "latest"); err != nil {
		return "", 0, fmt.Errorf("symbol(): %w", err)
	}
	return abiString(words(sym)), int(n.Int64()), nil
}

// abiString reads an ABI-encoded string, or a bytes32 one as some old tokens return.
func abiString(w [][]byte) string {
	if len(w) >= 3 {
		if n := new(big.Int).SetBytes(w[1]); n.IsInt64() && int(n.Int64()) <= 32*(len(w)-2) {
			var b []byte
			for _, x := range w[2:] {
				b = append(b, x...)
			}
			return string(b[:n.Int64()])
		}
	}
	if len(w) == 1 {
		return strings.TrimRight(string(w[0]), "\x00")
	}
	return ""
}
