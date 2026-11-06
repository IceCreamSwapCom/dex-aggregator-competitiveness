package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
)

const (
	eth  = "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	weth = "0x4200000000000000000000000000000000000006"
	usdc = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	aaa  = "0x00000000000000000000000000000000000000aa"
	bbb  = "0x00000000000000000000000000000000000000bb"
	ccc  = "0x00000000000000000000000000000000000000cc"
)

func TestDerive(t *testing.T) {
	ch := config.Chain{ID: 8453, Name: "Base", Tokens: map[string]config.Token{
		"ETH":  {Address: config.NativeToken, Decimals: 18},
		"WETH": {Address: weth, Decimals: 18, WrapsNative: true},
		"USDC": {Address: usdc, Decimals: 6, Stable: true},
	}}
	var ts []trade
	add := func(n int, in, out, receiver string, amountIn, amountOut float64) {
		for i := range n {
			ts = append(ts, trade{in: in, out: out, receiver: fmt.Sprint(receiver, i), amountIn: amountIn, amountOut: amountOut})
		}
	}
	add(60, eth, usdc, "r1", 1e18, 3000e6)           // $3000 each: ETH at $3000, bucket 1k
	add(30, usdc, eth, "r2", 300e6, 0.1e18)          // $300 each: bucket 100 (log10 300 = 2.48)
	add(10, weth, aaa, "r3", 1e18, 1000e18)          // $3000 each; AAA at $3
	add(10, aaa, bbb, "r4", 100e18, 600e6)           // $300 each; BBB trades only against AAA, so it has no price
	add(5, ccc, usdc, "r5", 1e18, 50_000e6)          // CCC cannot be simulated
	add(20, eth, usdc, "r6", 0.0001e18, 0.3e6)       // $0.30: dust
	add(1, eth, weth, "r7", 1000e18, 1000e18)        // wrapping
	add(3, usdc, aaa, "r8", 1e6, 1e30)               // way off AAA's price: left out
	add(1, eth, usdc, "whale", 1000e18, 3_000_000e6) // one receiver, capped at 1% of the volume

	resolve := func(a string) (meta, error) {
		switch a {
		case aaa:
			return meta{"AAA", 18}, nil
		}
		return meta{}, errors.New("slots API does not know the token")
	}
	tokens, pairs, rep, err := derive(ts, ch, []string{"USDC"}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.skipped) != 2 || !strings.Contains(rep.skipped[0], "slots API does not know") || !strings.Contains(rep.skipped[1], bbb+" has no price") {
		t.Errorf("skipped %q", rep.skipped)
	}
	// Volume before the cap: 180k + 9k + 30k + 3k + 250k (CCC) + 3M (whale) = 3.472M. A receiver counts with at
	// most 34,720: the whale, and each of the five $50k CCC trades.
	if want := 180_000 + 9_000 + 30_000 + 3_000 + 5*34_720 + 34_720.0; rep.volume < want-1 || rep.volume > want+1 {
		t.Errorf("volume %.0f, want %.0f", rep.volume, want)
	}
	got := map[string]map[string]int{}
	for _, p := range pairs {
		got[p.A+"/"+p.B] = p.Sizes
	}
	// CCC/USDC and AAA/BBB are left out; ETH/USDC and WETH/AAA are not yet 80% of the volume.
	if len(got) != 2 || got["USDC/ETH"]["1k"] == 0 || got["USDC/ETH"]["100"] == 0 || got["USDC/ETH"]["1m"] == 0 ||
		got["AAA/WETH"]["1k"] == 0 {
		t.Fatalf("pairs %v", got)
	}
	sum := 0
	for _, s := range got {
		for _, w := range s {
			sum += w
		}
	}
	if sum < 9_998 || sum > 10_002 {
		t.Errorf("weights add up to %d", sum)
	}
	if a := tokens["AAA"]; a.Address != aaa || a.Decimals != 18 || a.Sizes["1k"] != "330" || a.Sizes["10"] != "3.3" || a.Stable {
		t.Errorf("AAA = %+v", a)
	}
	if e := tokens["ETH"]; e.Sizes["100"] != "0.033" || e.Sizes["1m"] != "330" {
		t.Errorf("ETH sizes %v", e.Sizes)
	}
	if _, ok := tokens["CCC"]; ok {
		t.Error("CCC is in the tokens")
	}
}

func TestNeedsNativePair(t *testing.T) {
	ch := config.Chain{Tokens: map[string]config.Token{
		"ETH":  {Address: config.NativeToken, Decimals: 18},
		"USDC": {Address: usdc, Decimals: 6, Stable: true},
	}}
	var ts []trade
	for i := range 10 {
		ts = append(ts, trade{in: usdc, out: aaa, receiver: fmt.Sprint(i), amountIn: 1000e6, amountOut: 1000e18})
		ts = append(ts, trade{in: eth, out: usdc, receiver: fmt.Sprint(i), amountIn: 0.01e18, amountOut: 30e6})
	}
	_, pairs, rep, err := derive(ts, ch, []string{"USDC"}, func(string) (meta, error) { return meta{"AAA", 18}, nil })
	if err != nil {
		t.Fatal(err)
	}
	// USDC trades against ETH, so AAA/USDC can value its gas through USDC.
	if len(pairs) != 2 || len(rep.skipped) != 0 {
		t.Errorf("pairs %v, skipped %v", pairs, rep.skipped)
	}

	// Without ETH/USDC, no pair can value its gas.
	if _, _, _, err := derive(ts[:1], ch, []string{"USDC"}, func(string) (meta, error) { return meta{"AAA", 18}, nil }); err == nil {
		t.Error("a chain without a native pair was derived")
	}
}

func TestUnits(t *testing.T) {
	for _, tc := range []struct {
		whole    float64
		decimals int
		want     string
	}{
		{0.0312, 18, "0.031"}, {3.12, 18, "3.1"}, {31.2, 6, "31"}, {31_234, 6, "31000"}, {1.234e23, 18, "120000000000000000000000"},
		{0.0312, 2, "0.03"}, {0.004, 2, ""}, {0.99, 0, "1"},
	} {
		if got := units(tc.whole, tc.decimals); got != tc.want {
			t.Errorf("units(%v, %d) = %q, want %q", tc.whole, tc.decimals, got, tc.want)
		}
	}
	for usd, want := range map[float64]string{3.2: "10", 31: "10", 32: "100", 3_000: "1k", 5e6: "1m", 5e9: "1m"} {
		if got := bucketOf(usd); got != want {
			t.Errorf("bucketOf(%v) = %s, want %s", usd, got, want)
		}
	}
}

func TestFormatReadsBack(t *testing.T) {
	cfg, err := config.Load("../../config/benchmark.json")
	if err != nil {
		t.Fatal(err)
	}
	chains := cfg.Chains
	for i := range chains {
		if chains[i].Derived == nil {
			chains[i].Derived = &config.Derived{FromBlock: 1, ToBlock: 2, Anchors: []string{"USDC"}}
		}
	}
	for n, tk := range chains[0].Tokens {
		tk.Sizes = map[string]string{"1m": "9", "10": "1", "1k": "5", "100": "2"}
		chains[0].Tokens[n] = tk
	}
	for i := range chains[0].Pairs {
		chains[0].Pairs[i].Sizes = map[string]int{"1k": 1}
	}
	out, err := format(cfg, chains)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"sizes": { "10": "1", "100": "2", "1k": "5", "1m": "9" }`) {
		t.Errorf("sizes are not in bucket order:\n%.3000s", out)
	}
}
