package main

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
)

// The rules of the derivation (METHODOLOGY.md, 1.1).
const (
	coverage       = 0.8   // pairs are taken by volume until they cover this share of it,
	maxPairs       = 20    // or until there are this many
	receiverCap    = 0.01  // one receiving address counts with at most this share of a chain's volume
	minPriceTrades = 3     // a token is priced from at least this many trades
	maxSideRatio   = 2.0   // a trade whose two sides are valued this far apart is left out
	minTradeUSD    = 3.163 // smaller trades are dust (below the "10" bucket: 10 / √10)
	maxTradeUSD    = 1e9   // larger ones are bogus events (a router logs what it is called with)
)

// buckets are the size buckets, in dollars: a trade goes to the nearest one on
// a log scale.
var buckets = []string{"10", "100", "1k", "10k", "100k", "1m"}

func bucketOf(usd float64) string {
	e := int(math.Round(math.Log10(usd)))
	return buckets[min(max(e, 1), len(buckets))-1]
}

// meta is what is known about a token that is not in the config yet.
type meta struct {
	symbol   string
	decimals int
}

// report says what a derivation used and left out.
type report struct {
	trades, valued int
	volume         float64 // USD, after the receiver cap
	covered        float64 // share of the volume in the pairs taken
	skipped        []string
}

type pairKey struct{ a, b string } // a < b

func keyOf(x, y string) pairKey {
	if x > y {
		x, y = y, x
	}
	return pairKey{x, y}
}

// derive turns a chain's trades into its tokens and weighted pairs. The
// anchors are the config tokens that count as $1. resolve returns the symbol
// and decimals of a token the config does not have, or an error when the
// benchmark cannot trade it (the slots API does not know it).
func derive(trades []trade, ch config.Chain, anchorNames []string, resolve func(address string) (meta, error)) (map[string]config.Token, []config.Pair, report, error) {
	rep := report{trades: len(trades)}
	byAddress := map[string]string{} // address -> config name
	native := strings.ToLower(config.NativeToken)
	wrapped := ""
	for name, t := range ch.Tokens {
		a := strings.ToLower(t.Address)
		byAddress[a] = name
		if t.WrapsNative {
			wrapped = a
		}
	}
	anchors := map[string]float64{} // USD per base unit
	for _, name := range anchorNames {
		t, ok := ch.Tokens[name]
		if !ok {
			return nil, nil, rep, fmt.Errorf("the anchor %s is not a token of the chain", name)
		}
		anchors[strings.ToLower(t.Address)] = math.Pow10(-t.Decimals)
	}
	if len(anchors) == 0 {
		return nil, nil, rep, errors.New("no stablecoin to price the trades in")
	}
	// The native coin and its wrapped token have one price.
	same := func(a string) string {
		if a == native && wrapped != "" {
			return wrapped
		}
		return a
	}
	countsAsNative := func(a string) bool { return a == native || a == wrapped }
	price := prices(trades, anchors, same, same(native))
	usd := func(a string, amount float64) (float64, bool) {
		p, ok := price[same(a)]
		return p * amount, ok
	}

	// Value every trade at its smaller side, and cap each receiver.
	type valued struct {
		t   trade
		usd float64
	}
	var vs []valued
	byReceiver := map[string]float64{}
	total := 0.0
	for _, t := range trades {
		vi, okIn := usd(t.in, t.amountIn)
		vo, okOut := usd(t.out, t.amountOut)
		var v float64
		switch {
		case okIn && okOut:
			if max(vi, vo) > maxSideRatio*min(vi, vo) {
				continue
			}
			v = min(vi, vo)
		case okIn:
			v = vi
		case okOut:
			v = vo
		default:
			continue
		}
		if v < minTradeUSD || v > maxTradeUSD || same(t.in) == same(t.out) { // dust, bogus, or wrapping the native coin
			continue
		}
		vs = append(vs, valued{t, v})
		byReceiver[t.receiver] += v
		total += v
	}
	rep.valued = len(vs)
	volume := map[pairKey]map[string]float64{}
	pairTotal := map[pairKey]float64{}
	for _, v := range vs {
		x := v.usd
		if r := byReceiver[v.t.receiver]; r > receiverCap*total {
			x *= receiverCap * total / r
		}
		k := keyOf(v.t.in, v.t.out)
		if volume[k] == nil {
			volume[k] = map[string]float64{}
		}
		volume[k][bucketOf(v.usd)] += x
		pairTotal[k] += x
		rep.volume += x
	}
	if rep.volume == 0 {
		return nil, nil, rep, errors.New("no trade could be valued")
	}

	// Take pairs by volume.
	keys := make([]pairKey, 0, len(pairTotal))
	for k := range pairTotal {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y pairKey) int {
		return cmp.Or(cmp.Compare(pairTotal[y], pairTotal[x]), cmp.Compare(x.a, y.a), cmp.Compare(x.b, y.b))
	})
	metas := map[string]meta{}
	failed := map[string]error{}
	known := func(a string) error {
		if _, ok := byAddress[a]; ok {
			return nil
		}
		if err, ok := failed[a]; ok {
			return err
		}
		if _, ok := metas[a]; ok {
			return nil
		}
		m, err := resolve(a)
		if err != nil {
			failed[a] = err
			return err
		}
		metas[a] = m
		return nil
	}
	var taken []pairKey
	covered := 0.0
	take := func(k pairKey) bool {
		why := ""
		if _, ok := price[same(k.a)]; !ok {
			why = k.a + " has no price"
		} else if _, ok := price[same(k.b)]; !ok {
			why = k.b + " has no price"
		} else if err := cmp.Or(known(k.a), known(k.b)); err != nil {
			why = err.Error()
		}
		if why != "" {
			rep.skipped = append(rep.skipped, fmt.Sprintf("%s/%s (%.1f%%): %s", k.a, k.b, 100*pairTotal[k]/rep.volume, why))
			return false
		}
		taken = append(taken, k)
		covered += pairTotal[k]
		return true
	}
	for _, k := range keys {
		if covered >= coverage*rep.volume || len(taken) == maxPairs {
			break
		}
		take(k)
	}
	// A pair needs one token that is native or trades against it, to value gas
	// (config.Chain.check). For a pair without one, take the biggest pair of one
	// of its tokens with the native coin too, past the coverage if need be.
	for _, k := range slices.Clone(taken) {
		if slices.ContainsFunc(taken, func(n pairKey) bool {
			return (countsAsNative(n.a) || countsAsNative(n.b)) && (n.a == k.a || n.b == k.a || n.a == k.b || n.b == k.b)
		}) {
			continue
		}
		for _, n := range keys {
			if (countsAsNative(n.a) || countsAsNative(n.b)) && (n.a == k.a || n.b == k.a || n.a == k.b || n.b == k.b) &&
				!slices.Contains(taken, n) && take(n) {
				break
			}
		}
	}

	// Name the tokens: the config's name, or the token's symbol.
	names := map[string]string{}
	used := map[string]bool{}
	for a, n := range byAddress {
		names[a] = n
		used[n] = true
	}
	for _, k := range taken {
		for _, a := range []string{k.a, k.b} {
			if _, ok := names[a]; ok {
				continue
			}
			n := tokenName(metas[a].symbol, a)
			if used[n] {
				n += "-" + a[2:6]
			}
			names[a], used[n] = n, true
		}
	}

	// Sizes: every bucket in token units, two significant digits.
	tokens := map[string]config.Token{}
	tokenOf := func(a string) config.Token {
		if t, ok := tokens[names[a]]; ok {
			return t
		}
		var t config.Token
		if n, ok := byAddress[a]; ok {
			t = ch.Tokens[n]
		} else {
			t = config.Token{Address: a, Decimals: metas[a].decimals}
		}
		perToken := price[same(a)] * math.Pow10(t.Decimals) // USD
		if _, ok := byAddress[a]; !ok {
			// A new token is a stablecoin when its symbol says USD and it trades at $1 ± 2%.
			t.Stable = strings.Contains(strings.ToUpper(metas[a].symbol), "USD") && math.Abs(perToken-1) < 0.02
		}
		t.Sizes = map[string]string{}
		for _, b := range buckets {
			if s := units(float64(config.BucketValue(b))/perToken, t.Decimals); s != "" {
				t.Sizes[b] = s
			}
		}
		tokens[names[a]] = t
		return t
	}

	// Weights in basis points of the volume taken. A pair needs one token that
	// is native or trades against it, to value gas (config.Chain.check).
	var pairs []config.Pair
	for _, k := range taken {
		ta, tb := tokenOf(k.a), tokenOf(k.b)
		p := config.Pair{A: names[k.a], B: names[k.b], Sizes: map[string]int{}}
		for b, v := range volume[k] {
			if w := int(math.Round(10_000 * v / covered)); w > 0 && ta.Sizes[b] != "" && tb.Sizes[b] != "" {
				p.Sizes[b] = w
			}
		}
		if len(p.Sizes) > 0 {
			pairs = append(pairs, p)
		} else {
			covered -= pairTotal[k]
		}
	}
	for {
		priced := map[string]bool{}
		for _, p := range pairs {
			if countsAsNative(strings.ToLower(tokens[p.A].Address)) || countsAsNative(strings.ToLower(tokens[p.B].Address)) {
				priced[p.A], priced[p.B] = true, true
			}
		}
		n := len(pairs)
		pairs = slices.DeleteFunc(pairs, func(p config.Pair) bool {
			if priced[p.A] || priced[p.B] {
				return false
			}
			rep.skipped = append(rep.skipped, fmt.Sprintf("%s/%s: neither token trades against the native coin", p.A, p.B))
			covered -= pairVolume(volume, tokens, p)
			return true
		})
		if len(pairs) == n {
			break
		}
	}
	if len(pairs) == 0 {
		return nil, nil, rep, errors.New("no pair could be taken")
	}
	inPair := map[string]bool{}
	for _, p := range pairs {
		inPair[p.A], inPair[p.B] = true, true
	}
	for _, n := range anchorNames {
		if !inPair[n] {
			return nil, nil, rep, fmt.Errorf("the anchor %s is in no pair taken", n)
		}
	}
	for n := range tokens {
		if !inPair[n] {
			delete(tokens, n)
		}
	}
	rep.covered = covered / rep.volume
	return tokens, pairs, rep, nil
}

func pairVolume(volume map[pairKey]map[string]float64, tokens map[string]config.Token, p config.Pair) float64 {
	v := 0.0
	for _, x := range volume[keyOf(strings.ToLower(tokens[p.A].Address), strings.ToLower(tokens[p.B].Address))] {
		v += x
	}
	return v
}

// prices are USD per base unit, at the median rate of a token's trades (at
// least minPriceTrades, each of minTradeUSD to maxTradeUSD): the median of
// the trades rather than of their volume, so that a few huge bogus events
// cannot set a price. The stablecoins (anchors) are $1,
// the native coin from its trades against them, and every other token from its
// trades against the stablecoins and the native coin. A price never comes from
// another derived price, so one thin token's bad price cannot spread.
func prices(trades []trade, anchors map[string]float64, same func(string) string, native string) map[string]float64 {
	price := maps.Clone(anchors)
	if p, ok := observe(trades, price, same)[native].median(); ok {
		price[native] = p
	}
	base := maps.Clone(price)
	for a, os := range observe(trades, base, same) {
		if p, ok := os.median(); ok {
			price[a] = p
		}
	}
	return price
}

type observations []float64 // rates, USD per base unit

// observe collects, for every token not in priced, the rate of each of its
// trades against a priced token.
func observe(trades []trade, priced map[string]float64, same func(string) string) map[string]observations {
	seen := map[string]observations{}
	add := func(token string, amount, usd float64) {
		if _, ok := priced[token]; !ok && usd >= minTradeUSD && usd <= maxTradeUSD {
			seen[token] = append(seen[token], usd/amount)
		}
	}
	for _, t := range trades {
		in, out := same(t.in), same(t.out)
		if p, ok := priced[in]; ok {
			add(out, t.amountOut, p*t.amountIn)
		}
		if p, ok := priced[out]; ok {
			add(in, t.amountIn, p*t.amountOut)
		}
	}
	return seen
}

// median is the lower middle rate.
func (os observations) median() (float64, bool) {
	if len(os) < minPriceTrades {
		return 0, false
	}
	slices.Sort(os)
	return os[(len(os)-1)/2], true
}

// units is usd worth of a token in whole tokens, to two significant digits and
// at most the token's decimals; "" when that rounds to nothing.
func units(whole float64, decimals int) string {
	if whole <= 0 || math.IsInf(whole, 0) || math.IsNaN(whole) {
		return ""
	}
	e := int(math.Floor(math.Log10(whole))) // whole = d.dd… × 10^e
	var v float64
	if e >= 1 {
		scale := math.Pow10(e - 1)
		v = math.Round(whole/scale) * scale
	} else {
		digits := min(1-e, decimals)
		v = math.Round(whole*math.Pow10(digits)) / math.Pow10(digits)
	}
	if v <= 0 {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// tokenName is a token's symbol as a config key: letters, digits, '.', '-' and '_'.
func tokenName(symbol, address string) string {
	n := strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_", r)) {
			return r
		}
		return -1
	}, symbol)
	if n == "" || len(n) > 20 {
		return "T" + address[2:8]
	}
	return n
}
