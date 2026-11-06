package stats

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/store"
)

const (
	eth  = "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	usdc = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	usdt = "0xfde4c96c8593536e31f229ea8f37b2ada2699bb2"
)

func n(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return v
}

func ok(out string, gas uint64) *store.Simulation {
	return &store.Simulation{Success: true, AmountOut: n(out), Gas: gas}
}

func quote(agg string, latencyMs int, quoted string, t0, t30 *store.Simulation) store.Quote {
	q := store.Quote{Aggregator: agg, Status: "ok", Latency: time.Duration(latencyMs) * time.Millisecond, T0: t0, T30: t30}
	if quoted != "" {
		q.QuotedOut = n(quoted)
	}
	return q
}

var reverted = &store.Simulation{Revert: "Too little received"}

// TestLeaderboard checks metrics.sql against leaderboards worked out by hand.
// It needs a Postgres it may create tables in, see internal/store.
func TestLeaderboard(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	chain := uint64(900_000_000 + rand.IntN(1_000_000)) // a chain of this test run only
	start := time.Now().Add(-8 * time.Hour).Truncate(time.Second)
	round := func(at time.Duration, in, out string, amountIn string, quotes ...store.Quote) string {
		t.Helper()
		return insert(t, db, chain, start.Add(at), in, out, amountIn, quotes...)
	}

	// A: 1 ETH → USDC. It prices USDC at the median (the lower of two) of
	// 3000.0000 and 3000.0005 USDC per ETH: 3e-9 USDC base units per wei.
	// Gas costs x 100,000 × 1 gwei × 3e-9 = 300,000 and y 1,200,000. y gets 500
	// more but pays 900,000 more gas: x wins, y is 899,500 (3 bp) behind.
	round(0, eth, usdc, "1000000000000000000",
		quote("x", 100, "3000000000", ok("3000000000", 100_000), ok("2999000000", 100_000)),
		quote("y", 200, "3000001000", ok("3000000500", 400_000), reverted),
		quote("z", 300, "3000000000", reverted, nil),
		store.Quote{Aggregator: "w", Status: "no_route", Latency: 50 * time.Millisecond},
	)
	// B: USDC → ETH, gas valued 1:1. x and y tie exactly and split the win;
	// z is 50% behind.
	round(time.Minute, usdc, eth, "3000000000",
		quote("x", 200, "1000000000000000", ok("1000000000000000", 100_000), nil),
		quote("y", 300, "1000000000000000", ok("1000000000000000", 100_000), nil),
		quote("z", 400, "500000000000000", ok("500000000000000", 100_000), nil),
		store.Quote{Aggregator: "w", Status: "timeout", Latency: 5 * time.Second},
	)
	// C: USDT → USDC 7 hours after the last round that priced USDC, and no
	// round ever priced USDT: its gas cannot be valued, so it is not ranked.
	// y delivers 100,000 more than it quoted: its accuracy counts as 0, not +1 bp.
	round(7*time.Hour, usdt, usdc, "1000000000",
		quote("x", 300, "999000000", ok("999000000", 100_000), nil),
		quote("y", 400, "999500000", ok("999600000", 100_000), nil),
	)
	// D: ETH → USDC with one success: it prices USDC itself but is not ranked.
	round(7*time.Hour+time.Minute, eth, usdc, "1000000000000000000",
		quote("x", 400, "3000000000", ok("3000000000", 100_000), nil),
		quote("y", 500, "3000000000", reverted, nil),
	)

	s := New(db.Pool())
	if err := s.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	board, err := s.Leaderboard(ctx, Filter{ChainID: chain, Since: start.Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	f := func(v float64) *float64 { return &v }
	want := []api.LeaderboardRow{
		// x: wins A, half of B; within 1 bp in both. Its T30 at A came 1,000,000 short of the quote: −3.33 bps.
		// Only A was simulated at T30, so only A counts for its reverts.
		{Aggregator: "x", Requests: 4, RankedRounds: 2, Win: f(0.75), Within1Bp: f(1), Within5Bp: f(1),
			AccuracyT0: f(0), AccuracyT30: f(-10_000.0 / 3_000),
			Reverts: f(0), LatencyP50Ms: 250, LatencyP90Ms: 370, Availability: 1},
		// y: half of B; within 5 bp in both, within 1 bp only in B. At A it delivered 500 less than quoted, and
		// reverted at T30; at D it reverted at T0. Both reverts leave its availability alone.
		{Aggregator: "y", Requests: 4, RankedRounds: 2, Win: f(0.25), Within1Bp: f(0.5), Within5Bp: f(1),
			AccuracyT0: f(-500.0 / 3_000_001_000 * 10_000 / 3), Reverts: f(1), LatencyP50Ms: 350, LatencyP90Ms: 470, Availability: 1},
		// z reverted at T0 in A: a revert, not a missing answer.
		{Aggregator: "z", Requests: 2, RankedRounds: 2, Win: f(0), Within1Bp: f(0), Within5Bp: f(0),
			AccuracyT0: f(0), Reverts: f(1), LatencyP50Ms: 350, LatencyP90Ms: 390, Availability: 1},
		// w never answered with a transaction: no reverts to count, availability 0.
		{Aggregator: "w", Requests: 2, RankedRounds: 2, Win: f(0), Within1Bp: f(0), Within5Bp: f(0),
			LatencyP50Ms: 2525, LatencyP90Ms: 4505, Availability: 0},
	}
	if len(board) != len(want) {
		t.Fatalf("leaderboard = %+v", board)
	}
	for i := range want {
		g, w := board[i], want[i]
		same := g.Aggregator == w.Aggregator && g.Requests == w.Requests && g.RankedRounds == w.RankedRounds &&
			near(g.Win, w.Win) && near(g.Within1Bp, w.Within1Bp) && near(g.Within5Bp, w.Within5Bp) &&
			near(g.AccuracyT0, w.AccuracyT0) && near(g.AccuracyT30, w.AccuracyT30) &&
			near(g.Reverts, w.Reverts) && near(&g.LatencyP50Ms, &w.LatencyP50Ms) && near(&g.LatencyP90Ms, &w.LatencyP90Ms) &&
			near(&g.Availability, &w.Availability)
		if !same {
			t.Errorf("row %d =\n %s\nwant\n %s", i, show(g), show(w))
		}
	}

	// Filters: another region or size has nothing; the period cuts rounds off.
	if b, _ := s.Leaderboard(ctx, Filter{ChainID: chain, Since: start.Add(-time.Minute), Region: "elsewhere"}); len(b) != 0 {
		t.Errorf("other region: %+v", b)
	}
	if b, _ := s.Leaderboard(ctx, Filter{ChainID: chain, Since: start.Add(-time.Minute), Size: "100k"}); len(b) != 0 {
		t.Errorf("other size: %+v", b)
	}
	if b, _ := s.Leaderboard(ctx, Filter{ChainID: chain, Since: start.Add(time.Hour)}); len(b) != 2 || b[0].RankedRounds != 0 {
		t.Errorf("last two rounds only: %+v", b)
	}

	// One aggregator's page: its metrics by chain and size, its answers, failures and quotes.
	all := Filter{ChainID: chain, Since: start.Add(-time.Minute)}
	fx, fy, fw := all, all, all
	fx.Aggregator, fy.Aggregator, fw.Aggregator = "x", "y", "w"
	if g, err := s.ByChain(ctx, fx); err != nil || len(g) != 1 || g[0].Key != fmt.Sprint(chain) ||
		g[0].Requests != 4 || !near(g[0].Win, f(0.75)) {
		t.Errorf("x by chain = %+v, %v", g, err)
	}
	if g, err := s.BySize(ctx, fy); err != nil || len(g) != 1 || g[0].Key != "1k" || !near(g[0].Win, f(0.25)) {
		t.Errorf("y by size = %+v, %v", g, err)
	}
	if c, err := s.Statuses(ctx, fw); err != nil || len(c) != 2 || c[0].N != 1 || c[1].N != 1 {
		t.Errorf("w statuses = %+v, %v", c, err)
	}
	wantFailures := []Count{{"reverted at T0", "Too little received", 1}, {"reverted at T30", "Too little received", 1}}
	if c, err := s.Failures(ctx, fy, 5); err != nil || !slices.Equal(c, wantFailures) {
		t.Errorf("y failures = %+v, %v", c, err)
	}
	if q, err := s.Quotes(ctx, fy, 2); err != nil || len(q) != 2 || q[0].StartedAt.Before(q[1].StartedAt) ||
		q[0].T0 == nil || *q[0].T0 || q[0].Ranked == nil || *q[0].Ranked || q[0].SizeBucket != "1k" || q[0].AmountIn != "1000000000000000000" {
		t.Errorf("y quotes = %+v, %v", q, err) // newest first: D, where y reverted, not ranked
	}

	// The export: every quote of the selection, with the numbers it ranked by.
	var out bytes.Buffer
	if err := s.Export(ctx, Filter{ChainID: chain, Since: start.Add(-time.Minute)}, &out); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1+12 { // header, then A and B with 4 quotes, C and D with 2
		t.Fatalf("%d records", len(records))
	}
	col := map[string]int{}
	for i, name := range records[0] {
		col[name] = i
	}
	first := records[1] // round A, aggregator w (no route)
	x := records[2]     // round A, aggregator x
	if first[col["aggregator"]] != "w" || first[col["status"]] != "no_route" || first[col["t0_success"]] != "" ||
		x[col["aggregator"]] != "x" || x[col["amount_in"]] != "1000000000000000000" || x[col["t0_out"]] != "3000000000" ||
		x[col["gas_cost"]] != "300000" || x[col["net"]] != "2999700000" || x[col["ranked"]] != "true" ||
		x[col["win_share"]] != "1.00000000000000000000" || x[col["within_1bp"]] != "true" || !strings.HasSuffix(x[col["started_at"]], "Z") {
		t.Errorf("header %v\nw %v\nx %v", records[0], first, x)
	}
}

// insert stores a round in which every quote is in the 1k bucket.
func insert(t *testing.T, db *store.Store, chain uint64, at time.Time, in, out, amountIn string, quotes ...store.Quote) string {
	t.Helper()
	gwei := big.NewInt(1_000_000_000)
	id, err := db.Insert(context.Background(), &store.Round{
		Region: "test", ChainID: chain, TokenIn: in, TokenOut: out, TokenInNative: in == eth, TokenOutNative: out == eth,
		AmountIn: n(amountIn), SizeBucket: "1k", SlippageBps: 50, Taker: "0xtaker", StartedAt: at,
		T0Block: 1, T0GasPrice: gwei, T30Block: 16, T30GasPrice: gwei, MethodologyVersion: "test", CollectorVersion: "test",
		Quotes: quotes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestGasThroughTokenIn checks that a round whose tokenOut no round has priced
// values its gas through tokenIn: its own rate times tokenIn's price.
func TestGasThroughTokenIn(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	chain := uint64(900_000_000 + rand.IntN(1_000_000))
	start := time.Now().Add(-time.Hour).Truncate(time.Second)

	// P prices USDC at 3e-9 base units per wei, as round A of TestLeaderboard.
	insert(t, db, chain, start, eth, usdc, "1000000000000000000",
		quote("x", 100, "3000000000", ok("3000000000", 100_000), nil),
		quote("y", 100, "3000000000", ok("3000000000", 100_000), nil))
	// Q: 1000 USDC → USDT. Its own rate is the lower median 0.999 USDT per USDC,
	// so USDT is 2.997e-9 per wei and 100,000 gas at 1 gwei costs 299,700. y
	// wins; x is 500,000 (5.004 bp) behind.
	q := insert(t, db, chain, start.Add(time.Minute), usdc, usdt, "1000000000",
		quote("x", 100, "999000000", ok("999000000", 100_000), nil),
		quote("y", 100, "999500000", ok("999500000", 100_000), nil))

	s := New(db.Pool())
	if err := s.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	sc, err := s.Round(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	x, y := sc["x"], sc["y"]
	if x.GasCost != "299700" || x.Net != "998700300" || !x.Ranked || x.WinShare != 0 || x.Within5 ||
		y.Net != "999200300" || y.WinShare != 1 {
		t.Errorf("scores = %+v", sc)
	}
}

func near(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return math.Abs(*a-*b) < 1e-9*math.Max(1, math.Abs(*b))
}

func show(r api.LeaderboardRow) string {
	p := func(v *float64) any {
		if v == nil {
			return "nil"
		}
		return *v
	}
	return fmt.Sprint(r.Aggregator, r.Requests, r.RankedRounds, p(r.Win), p(r.Within1Bp), p(r.Within5Bp),
		p(r.AccuracyT0), p(r.AccuracyT30), p(r.Reverts),
		r.LatencyP50Ms, r.LatencyP90Ms, r.Availability)
}
