// Package config loads config/benchmark.json: the public rules every collector
// runs with. Everything in that file is part of the methodology, so a value
// that does not make sense stops the start instead of being worked around.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"
)

// NativeToken is how the config, the aggregator adapters and the simulation
// spell a chain's native coin.
const NativeToken = "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE"

// Config is the whole benchmark.
type Config struct {
	MethodologyVersion string   `json:"methodologyVersion"`
	RoundsPerMinute    float64  `json:"roundsPerMinute"`    // per collector, over all chains
	QuoteTimeout       Duration `json:"quoteTimeout"`       // per aggregator, all of its calls included
	StaleDelay         Duration `json:"staleDelay"`         // from the T0 block to the T30 block
	GasLimitMultiplier float64  `json:"gasLimitMultiplier"` // gas limit = multiplier × gas the swap uses
	SlippageBps        struct {
		Default int `json:"default"`
		Stable  int `json:"stable"` // when both tokens are stablecoins
	} `json:"slippageBps"`
	IntegratorID string   `json:"integratorId"` // the free-form name APIs that ask who is calling get
	Aggregators  []string `json:"aggregators"`  // asked in every round of every chain
	Chains       []Chain  `json:"chains"`
}

// Chain is one chain and the trades benchmarked on it. cmd/distribution
// derives Weight, the tokens and the pairs from real trades.
type Chain struct {
	ID        uint64           `json:"id"`
	Name      string           `json:"name"`
	MaxTxGas  uint64           `json:"maxTxGas"`  // the most gas one transaction may use
	Weight    int              `json:"weight"`    // a round is on this chain with probability weight / sum of all chains' weights
	DefiLlama string           `json:"defillama"` // the chain's name in DefiLlama's aggregator volumes, for cmd/distribution
	Derived   *Derived         `json:"derived,omitempty"`
	Tokens    map[string]Token `json:"tokens"`
	Pairs     []Pair           `json:"pairs"`
}

// Derived is what cmd/distribution derived a chain's weights from: the router
// events of these blocks, priced in these stablecoins (token names), and
// DefiLlama's 30-day aggregator volume of the chain at the time.
// cmd/distribution -reproduce reads the same blocks again.
type Derived struct {
	FromBlock          uint64   `json:"fromBlock"`
	ToBlock            uint64   `json:"toBlock"`
	Trades             int      `json:"trades"` // read from the routers' events
	Anchors            []string `json:"anchors"`
	DefiLlamaVolume30d int64    `json:"defillamaVolume30d"` // USD
}

// Token is one token of a chain. Sizes maps a size bucket ("1k") to an amount
// in whole tokens ("0.3").
type Token struct {
	Address     string            `json:"address"`
	Decimals    int               `json:"decimals"`
	Stable      bool              `json:"stable"`
	WrapsNative bool              `json:"wrapsNative"`
	Sizes       map[string]string `json:"sizes"`
}

// Pair is traded in both directions. Sizes maps a size bucket to a weight: a
// round trades the pair at that size with probability weight / sum of all the
// chain's weights.
type Pair struct {
	A     string         `json:"a"`
	B     string         `json:"b"`
	Sizes map[string]int `json:"sizes"`
}

// Native reports whether the token is the chain's native coin.
func (t Token) Native() bool { return strings.EqualFold(t.Address, NativeToken) }

// CountsAsNative reports whether the token is the native coin or its wrapped
// token: both are priced 1:1 against the native coin the gas is paid in.
func (t Token) CountsAsNative() bool { return t.Native() || t.WrapsNative }

// BucketValue reads a size bucket ("10", "1k", "10k", "1m") as dollars.
func BucketValue(b string) int64 {
	mult := int64(1)
	switch {
	case strings.HasSuffix(b, "k"):
		mult, b = 1_000, strings.TrimSuffix(b, "k")
	case strings.HasSuffix(b, "m"):
		mult, b = 1_000_000, strings.TrimSuffix(b, "m")
	}
	v, err := strconv.ParseInt(b, 10, 64)
	if err != nil {
		return 0
	}
	return v * mult
}

// Amount is the size bucket in base units.
func (t Token) Amount(bucket string) *big.Int {
	v, err := ParseUnits(t.Sizes[bucket], t.Decimals)
	if err != nil {
		panic(err) // Load has checked every size
	}
	return v
}

// Duration reads a Go duration string ("5s").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

// Load reads and checks the config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and checks a config. Unknown fields are refused: a typo must
// not silently fall back to a default.
func Parse(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) check() error {
	switch {
	case c.MethodologyVersion == "":
		return errors.New("methodologyVersion is missing")
	case c.RoundsPerMinute <= 0:
		return errors.New("roundsPerMinute must be positive")
	case c.QuoteTimeout.Duration <= 0, c.StaleDelay.Duration <= 0:
		return errors.New("quoteTimeout and staleDelay must be positive")
	case c.GasLimitMultiplier < 1:
		return errors.New("gasLimitMultiplier must be at least 1")
	case c.SlippageBps.Default <= 0, c.SlippageBps.Stable <= 0:
		return errors.New("slippageBps must be positive")
	case len(c.Aggregators) == 0:
		return errors.New("no aggregators")
	case len(c.Chains) == 0:
		return errors.New("no chains")
	}
	asked := map[string]bool{}
	for _, id := range c.Aggregators {
		if asked[id] {
			return fmt.Errorf("aggregator %q is listed twice", id)
		}
		asked[id] = true
	}
	seen := map[uint64]bool{}
	for _, ch := range c.Chains {
		if seen[ch.ID] {
			return fmt.Errorf("chain %d is listed twice", ch.ID)
		}
		seen[ch.ID] = true
		if err := ch.check(); err != nil {
			return fmt.Errorf("chain %d: %w", ch.ID, err)
		}
	}
	return nil
}

func (ch Chain) check() error {
	switch {
	case ch.ID == 0 || ch.Name == "":
		return errors.New("id and name are required")
	case ch.MaxTxGas < 21_000:
		return errors.New("maxTxGas is missing")
	case ch.Weight <= 0:
		return errors.New("weight must be positive")
	case len(ch.Pairs) == 0:
		return errors.New("no pairs")
	}
	for name, t := range ch.Tokens {
		if !isAddress(t.Address) {
			return fmt.Errorf("token %s: address %q is not an address", name, t.Address)
		}
		if t.Decimals < 0 || t.Decimals > 36 {
			return fmt.Errorf("token %s: decimals %d", name, t.Decimals)
		}
		if len(t.Sizes) == 0 {
			return fmt.Errorf("token %s: no sizes", name)
		}
		for bucket, s := range t.Sizes {
			if BucketValue(bucket) <= 0 {
				return fmt.Errorf("token %s: %q is not a size bucket", name, bucket)
			}
			if v, err := ParseUnits(s, t.Decimals); err != nil || v.Sign() <= 0 {
				return fmt.Errorf("token %s: size %s = %q is not a positive amount", name, bucket, s)
			}
		}
	}
	paired := map[string]bool{} // tokens that are in a pair
	priced := map[string]bool{} // tokens that count as native or are in a pair with one
	pairs := map[[2]string]bool{}
	for _, p := range ch.Pairs {
		a, okA := ch.Tokens[p.A]
		b, okB := ch.Tokens[p.B]
		switch {
		case !okA || !okB:
			return fmt.Errorf("pair %s/%s: unknown token", p.A, p.B)
		case p.A == p.B:
			return fmt.Errorf("pair %s/%s: same token twice", p.A, p.B)
		case pairs[[2]string{p.A, p.B}] || pairs[[2]string{p.B, p.A}]:
			return fmt.Errorf("pair %s/%s is listed twice", p.A, p.B)
		case len(p.Sizes) == 0:
			return fmt.Errorf("pair %s/%s: no sizes", p.A, p.B)
		}
		pairs[[2]string{p.A, p.B}] = true
		for bucket, w := range p.Sizes {
			if w <= 0 {
				return fmt.Errorf("pair %s/%s: size %s: weight must be positive", p.A, p.B, bucket)
			}
			if a.Sizes[bucket] == "" || b.Sizes[bucket] == "" {
				return fmt.Errorf("pair %s/%s: size %s is not a size of both tokens", p.A, p.B, bucket)
			}
		}
		paired[p.A], paired[p.B] = true, true
		if a.CountsAsNative() || b.CountsAsNative() {
			priced[p.A], priced[p.B] = true, true
		}
	}
	for name := range ch.Tokens {
		if !paired[name] {
			return fmt.Errorf("token %s is in no pair", name)
		}
	}
	// Gas is valued in tokenOut through rounds that trade it against the
	// native coin, or through tokenIn when tokenOut has none (METHODOLOGY.md, 3.1).
	for _, p := range ch.Pairs {
		if !priced[p.A] && !priced[p.B] {
			return fmt.Errorf("pair %s/%s: one of its tokens needs a pair with the native coin, to value gas in it", p.A, p.B)
		}
	}
	return nil
}

// ParseUnits turns a whole-token amount ("0.3") into base units.
func ParseUnits(s string, decimals int) (*big.Int, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals {
		return nil, fmt.Errorf("%q has more than %d decimals", s, decimals)
	}
	digits := whole + frac + strings.Repeat("0", decimals-len(frac))
	if whole == "" || strings.Trim(digits, "0123456789") != "" {
		return nil, fmt.Errorf("%q is not a decimal amount", s)
	}
	v, _ := new(big.Int).SetString(digits, 10)
	return v, nil
}

func isAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	return strings.Trim(strings.ToLower(s[2:]), "0123456789abcdef") == ""
}
