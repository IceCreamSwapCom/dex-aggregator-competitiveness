package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadShippedConfig(t *testing.T) {
	c, err := Load("../../config/benchmark.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.QuoteTimeout.Duration != 5*time.Second || c.StaleDelay.Duration != 30*time.Second {
		t.Errorf("timeouts = %v, %v", c.QuoteTimeout, c.StaleDelay)
	}
	if c.SlippageBps.Default != 50 || c.SlippageBps.Stable != 10 || c.GasLimitMultiplier != 1.5 {
		t.Errorf("slippage %+v, multiplier %v", c.SlippageBps, c.GasLimitMultiplier)
	}
	if len(c.Chains) < 2 || len(c.Aggregators) == 0 {
		t.Errorf("%d chains, %d aggregators", len(c.Chains), len(c.Aggregators))
	}
	// METHODOLOGY.md describes the version the config measures by, with a
	// changelog entry for it.
	doc, err := os.ReadFile("../../METHODOLOGY.md")
	if err != nil {
		t.Fatal(err)
	}
	if v := c.MethodologyVersion; !strings.Contains(string(doc), "Version "+v+".") || !strings.Contains(string(doc), "### "+v+" (") {
		t.Errorf("METHODOLOGY.md does not describe methodologyVersion %s, or has no changelog entry for it", v)
	}
}

func TestParseUnits(t *testing.T) {
	for in, want := range map[string]string{"0.3": "300000", "100": "100000000", "0.000001": "1", "1.": "1000000"} {
		if v, err := ParseUnits(in, 6); err != nil || v.String() != want {
			t.Errorf("ParseUnits(%q, 6) = %v, %v; want %s", in, v, err, want)
		}
	}
	for _, in := range []string{"", ".5", "1e3", "-1", "0.0000001", "1,5"} {
		if _, err := ParseUnits(in, 6); err == nil {
			t.Errorf("ParseUnits(%q, 6) accepted", in)
		}
	}
}

func TestRefusals(t *testing.T) {
	const valid = `{"methodologyVersion":"1","roundsPerMinute":1,"quoteTimeout":"5s","staleDelay":"30s","gasLimitMultiplier":1.5,
		"slippageBps":{"default":50,"stable":10},"aggregators":["x"],"chains":[{"id":1,"name":"Ethereum","maxTxGas":16777216,"weight":1,
		"tokens":{"ETH":{"address":"0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE","decimals":18,"sizes":{"1k":"0.3"}},
		          "USDC":{"address":"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48","decimals":6,"sizes":{"1k":"1000"}}},
		"pairs":[{"a":"ETH","b":"USDC","sizes":{"1k":1}}]}]}`
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for name, edit := range map[string][2]string{
		"unknown field":       {`"roundsPerMinute"`, `"roundsPerMinutes"`},
		"bad duration":        {`"5s"`, `"5"`},
		"multiplier below 1":  {`1.5`, `0.9`},
		"unknown pair token":  {`"b":"USDC"`, `"b":"USDT"`},
		"too many decimals":   {`"1000"`, `"0.0000001"`},
		"no native pair":      {`"address":"0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE"`, `"address":"0xdac17f958d2ee523a2206206994597c13d831ec7"`},
		"token in no pair":    {`"USDC":{`, `"DAI":{"address":"0x6b175474e89094c44da98b954eedeac495271d0f","decimals":18,"sizes":{"1k":"1000"}},"USDC":{`},
		"chain without gas":   {`"maxTxGas":16777216`, `"maxTxGas":0`},
		"address is not hex":  {`0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48`, `0xa0b86991c6218b36c1d19d4a2e9eb0ce3606ebzz`},
		"no aggregators":      {`"aggregators":["x"]`, `"aggregators":[]`},
		"zero chain weight":   {`"weight":1`, `"weight":0`},
		"zero size weight":    {`"sizes":{"1k":1}`, `"sizes":{"1k":0}`},
		"no pair sizes":       {`"sizes":{"1k":1}`, `"sizes":{}`},
		"size of no token":    {`"sizes":{"1k":1}`, `"sizes":{"10k":1}`},
		"aggregator twice":    {`"aggregators":["x"]`, `"aggregators":["x","x"]`},
		"same token twice":    {`"b":"USDC"`, `"b":"ETH"`},
		"missing methodology": {`"methodologyVersion":"1"`, `"methodologyVersion":""`},
	} {
		if !strings.Contains(valid, edit[0]) {
			t.Fatalf("%s: %q is not in the config", name, edit[0])
		}
		if _, err := Parse([]byte(strings.Replace(valid, edit[0], edit[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
