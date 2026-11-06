package web

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/stats"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/store"
)

const (
	id   = "0b5c3a3e-2f43-4b0c-9a8e-6c1f0d2e4a51"
	eth  = "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	usdc = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
)

type fakeDB struct {
	exportErr error // Export fails with it before writing
	chainID   uint64
	before    time.Time
	limit     int
	filter    stats.Filter
}

func (f *fakeDB) Round(_ context.Context, rid string) (*api.Round, error) {
	if rid != id {
		return nil, store.ErrNotFound
	}
	return &api.Round{
		ID: id, ChainID: 8453, TokenIn: eth, TokenOut: usdc, TokenInNative: true, AmountIn: "300000000000000000",
		SizeBucket: "1k", SlippageBps: 50, Taker: "0x00000000000000000000000000000000000a0001", StartedAt: time.Now(),
		T0Block: 100, T0GasPrice: "6000000", T30Block: 115, T30GasPrice: "6100000", MethodologyVersion: "0.1",
		Quotes: []api.Quote{
			{Aggregator: "a", Status: "ok", LatencyMs: 300, QuotedOut: "1000000000", TxTo: "0x6131b5fae19ea4f9d964eac0408e4408b66337b5",
				TxData: "0x12345678", TxValue: "300000000000000000", GasLimit: 300_000,
				T0:  &api.Simulation{Success: true, AmountOut: "999900000", Gas: 200_000},
				T30: &api.Simulation{Success: false, AmountOut: "0", Gas: 200_000, Revert: "Too little received"}},
			{Aggregator: "b", Status: "ok", LatencyMs: 400, QuotedOut: "999000000", TxTo: "0x6131b5fae19ea4f9d964eac0408e4408b66337b5",
				TxData: "0x12345678", TxValue: "300000000000000000", GasLimit: 16_777_216,
				T0: &api.Simulation{Success: false, Revert: "Too little received"}},
			{Aggregator: "c", Status: "no_route", Error: "no route found", LatencyMs: 100},
		},
	}, nil
}

func (f *fakeDB) Rounds(_ context.Context, chainID uint64, before time.Time, limit int) ([]api.RoundRef, error) {
	f.chainID, f.before, f.limit = chainID, before, limit
	return []api.RoundRef{{ID: id, ChainID: 8453, Region: "eu", StartedAt: time.Now(), TokenIn: eth, TokenOut: usdc,
		AmountIn: "300000000000000000", SizeBucket: "1k", Quotes: 3, SucceededT0: 1}}, nil
}

func (f *fakeDB) Leaderboard(_ context.Context, fl stats.Filter) ([]api.LeaderboardRow, error) {
	f.filter = fl
	v := func(x float64) *float64 { return &x }
	return []api.LeaderboardRow{
		{Aggregator: "a", Requests: 10, RankedRounds: 8, Win: v(0.75), Within1Bp: v(0.9), Within5Bp: v(1),
			AccuracyT0: v(-0.4), AccuracyT30: v(-1.25), Reverts: v(0.1), LatencyP50Ms: 300, LatencyP90Ms: 800, Availability: 0.9},
		{Aggregator: "b", Requests: 10, RankedRounds: 8, Win: v(0.25), LatencyP50Ms: 400, LatencyP90Ms: 900},
	}, nil
}

func (f *fakeDB) Regions(context.Context) ([]string, error) { return []string{"eu", "us"}, nil }

func (f *fakeDB) Export(_ context.Context, fl stats.Filter, w io.Writer) error {
	f.filter = fl
	if f.exportErr != nil {
		return f.exportErr
	}
	_, err := io.WriteString(w, "round_id,aggregator\n"+id+",a\n")
	return err
}

func (f *fakeDB) ByChain(_ context.Context, fl stats.Filter) ([]stats.Group, error) {
	f.filter = fl
	v := func(x float64) *float64 { return &x }
	return []stats.Group{
		{Key: "8453", LeaderboardRow: api.LeaderboardRow{Aggregator: fl.Aggregator, Requests: 6, Win: v(0.5)}},
		{Key: "10", LeaderboardRow: api.LeaderboardRow{Aggregator: fl.Aggregator, Requests: 4, Win: v(1)}},
	}, nil
}

func (f *fakeDB) BySize(_ context.Context, fl stats.Filter) ([]stats.Group, error) {
	v := func(x float64) *float64 { return &x }
	return []stats.Group{
		{Key: "10k", LeaderboardRow: api.LeaderboardRow{Requests: 5, Win: v(0.9)}},
		{Key: "100", LeaderboardRow: api.LeaderboardRow{Requests: 5, Win: v(0.6)}},
	}, nil
}

func (f *fakeDB) Statuses(context.Context, stats.Filter) ([]stats.Count, error) {
	return []stats.Count{{Key: "ok", N: 9}, {Key: "timeout", N: 1}}, nil
}

func (f *fakeDB) Failures(context.Context, stats.Filter, int) ([]stats.Count, error) {
	return []stats.Count{{Key: "reverted at T30", Detail: "Too little received", N: 2}}, nil
}

func (f *fakeDB) Quotes(context.Context, stats.Filter, int) ([]stats.QuoteRef, error) {
	yes, no, one := true, false, 1.0
	return []stats.QuoteRef{
		{RoundID: id, StartedAt: time.Now(), ChainID: 8453, TokenIn: eth, TokenOut: usdc, AmountIn: "300000000000000000",
			SizeBucket: "1k", Status: "ok", T0: &yes, T30: &no, Ranked: &yes, WinShare: &one},
		{RoundID: id, StartedAt: time.Now(), ChainID: 8453, TokenIn: usdc, TokenOut: eth, AmountIn: "1000000000",
			SizeBucket: "1k", Status: "no_route"},
	}, nil
}

// board is fakeDB with Board's per-round scores in place of its Round.
type board struct{ *fakeDB }

func (b board) Round(context.Context, string) (map[string]stats.Score, error) {
	return map[string]stats.Score{"a": {GasCost: "3600", Net: "999896400", Ranked: true, WinShare: 1, Within1: true, Within5: true}}, nil
}

func server(t *testing.T) (*Server, *fakeDB) {
	cfg, err := config.Load("../../config/benchmark.json")
	if err != nil {
		t.Fatal(err)
	}
	db := &fakeDB{}
	return New(cfg, db, board{db}), db
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestLeaderboardPage(t *testing.T) {
	s, db := server(t)
	// The default: all chains together.
	w := get(t, s.Handler(), "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Leaderboard · All chains") ||
		!strings.Contains(w.Body.String(), `href="/api/v1/export.csv?period=7d"`) || db.filter.ChainID != 0 {
		t.Fatalf("all chains: %d, filter %+v", w.Code, db.filter)
	}

	// Who built it, above the leaderboard, once.
	if body := w.Body.String(); strings.Count(body, "Built by the team behind Nordstern") != 1 ||
		strings.Index(body, "Built by the team behind Nordstern") > strings.Index(body, "<h2>Leaderboard</h2>") {
		t.Error("the Nordstern box is not once above the leaderboard")
	}

	// One table: a category per column, the best of each marked.
	if body := w.Body.String(); !strings.Contains(body, `class="best"><span class="inbar">`) || strings.Count(body, `class="best"`) < 4 {
		t.Errorf("leaderboard marks no best")
	}

	w = get(t, s.Handler(), "/?chain=8453&period=24h&size=10k&region=eu")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"Leaderboard · Base", "75.0%",
		// a's quote accuracy at execution (T30), its revert rate and latency: the best of each
		`class="best">−1.25 bp</td>`, `class="best">10.0%</td>`, `class="best">300 ms</td>`,
		// b is second in best rate and latency
		`class="next"><span class="inbar">`, `class="next">400 ms</td>`,
		`<a class="on" href="/?chain=8453&amp;period=24h&amp;region=eu&amp;size=10k">$10k</a>`,
		`<a class="on" href="/?chain=8453&amp;period=24h&amp;region=eu&amp;size=10k">Base</a>`,
		`<a href="/?chain=8453&amp;period=7d&amp;region=eu&amp;size=10k">7d</a>`,
		`href="/api/v1/export.csv?chain=8453&amp;period=24h&amp;region=eu&amp;size=10k"`,
		`href="/aggregators/a?chain=8453&amp;period=24h&amp;region=eu&amp;size=10k"`,
		`<tr data-href="/aggregators/a?chain=8453&amp;period=24h&amp;region=eu&amp;size=10k">`, // the whole row opens it
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	if db.filter.ChainID != 8453 || db.filter.Size != "10k" || db.filter.Region != "eu" || time.Since(db.filter.Since) < 23*time.Hour {
		t.Errorf("filter %+v", db.filter)
	}
	for _, q := range []string{"chain=10", "period=1y", "size=7k"} {
		if w := get(t, s.Handler(), "/?"+q); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}

}

func TestRoundPages(t *testing.T) {
	s, db := server(t)
	w := get(t, s.Handler(), "/rounds?chain=8453")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "0.3 ETH") || db.chainID != 8453 || db.limit != roundsPerPage {
		t.Fatalf("rounds: %d %+v\n%s", w.Code, db, w.Body)
	}
	w = get(t, s.Handler(), "/rounds/"+id)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("round: %d %s", w.Code, body)
	}
	for _, want := range []string{"0.3 ETH → USDC", "999.9", "-1.0", "win", "reverted", "no_route: no route found",
		"go run ./cmd/resim -api http://example.com " + id, "0.006 gwei"} {
		if !strings.Contains(body, want) {
			t.Errorf("round page lacks %q", want)
		}
	}
	if w := get(t, s.Handler(), "/rounds/11111111-2222-3333-4444-555555555555"); w.Code != http.StatusNotFound {
		t.Errorf("missing round: %d", w.Code)
	}
}

func TestAPI(t *testing.T) {
	s, db := server(t)
	h := s.Handler()
	w := get(t, h, "/api/v1/rounds/"+id)
	var rd api.Round
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rd) != nil || rd.ID != id || rd.AmountIn != "300000000000000000" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") == "" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("headers %v", w.Header())
	}
	for _, path := range []string{"/api/v1/rounds/11111111-2222-3333-4444-555555555555", "/api/v1/rounds/not-a-uuid"} {
		if w := get(t, h, path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}

	if w := get(t, h, "/api/v1/rounds"); w.Code != http.StatusOK || db.chainID != 0 || db.limit != 100 || time.Since(db.before) > time.Minute {
		t.Errorf("defaults: %d, %+v", w.Code, db)
	}
	w = get(t, h, "/api/v1/rounds?chain=8453&before=2026-11-01T00:00:00Z&limit=5")
	var refs []api.RoundRef
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &refs) != nil || len(refs) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if db.chainID != 8453 || db.limit != 5 || !db.before.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("query %+v", db)
	}
	for _, q := range []string{"chain=base", "before=yesterday", "limit=0", "limit=1001"} {
		if w := get(t, h, "/api/v1/rounds?"+q); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}

	s.Refreshed(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	w = get(t, h, "/api/v1/leaderboard?period=30d")
	var lb api.Leaderboard
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &lb) != nil || lb.ChainID != 0 || lb.Period != "30d" ||
		len(lb.Rows) != 2 || *lb.Rows[0].Win != 0.75 || !lb.RefreshedAt.Equal(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("leaderboard %d %s", w.Code, w.Body)
	}
}

func TestUnits(t *testing.T) {
	for _, tc := range []struct {
		base     string
		decimals int
		want     string
	}{
		{"3000000500", 6, "3,000.0005"},
		{"473885280888008211", 18, "0.4738852808"},
		{"124218", 8, "0.00124218"},
		{"100000000000000000000000", 18, "100,000"},
		{"-1200000", 6, "−1.2"},
		{"5", 0, "5"},
	} {
		if got := units(tc.base, tc.decimals); got != tc.want {
			t.Errorf("units(%s, %d) = %s, want %s", tc.base, tc.decimals, got, tc.want)
		}
	}
}

func TestExport(t *testing.T) {
	s, db := server(t)
	h := s.Handler()
	if w := get(t, h, "/api/v1/export.csv"); !strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="dexbench-all-7d-`) {
		t.Errorf("all chains: %v", w.Header())
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/export.csv?chain=8453&period=30d&size=10k", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get("Content-Encoding") != "gzip" ||
		!strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="dexbench-8453-30d-10k-`) {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != "round_id,aggregator\n"+id+",a\n" || db.filter.Size != "10k" || time.Since(db.filter.Since) < 29*24*time.Hour {
		t.Errorf("body %q, filter %+v", body, db.filter)
	}

	// Without gzip the CSV comes as it is.
	if w := get(t, h, "/api/v1/export.csv"); w.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(w.Body.String(), "round_id,") {
		t.Errorf("plain: %v %q", w.Header(), w.Body)
	}
	// A failure before the first byte is an error, not an empty file.
	db.exportErr = errors.New("database gone")
	req.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError || w.Header().Get("Content-Encoding") != "" || w.Header().Get("Content-Disposition") != "" {
		t.Errorf("failed export: %d %v", w.Code, w.Header())
	}
	if w := get(t, h, "/api/v1/export.csv?period=1y"); w.Code != http.StatusBadRequest {
		t.Errorf("bad period: %d", w.Code)
	}
}

func TestAggregatorPage(t *testing.T) {
	s, db := server(t)
	w := get(t, s.Handler(), "/aggregators/b?chain=8453&period=24h")
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, body)
	}
	// The only docs linked are Nordstern's, in the box saying who built the benchmark.
	if strings.Count(body, "API docs ↗") != 1 || !strings.Contains(body, `href="https://docs.nordstern.finance/quick-start">Nordstern API docs ↗</a>`) {
		t.Error("links other docs than Nordstern's, once")
	}
	for _, want := range []string{
		"<h1>b", "#2 of 2 by best rate · Base",
		`<span class="chip next r">#2 of 2</span>`,                               // its place in a category
		`href="/?chain=8453&amp;period=24h"`,                                     // back to the same leaderboard
		`href="/aggregators/b?aggregator=b&amp;chain=8453&amp;period=7d">7d</a>`, // the filters reload this page
		"aggregator=b",                     // the export is of its quotes only
		"Base", "chain 10", "$100", "$10k", // by chain (two of them) and by size
		"timeout", "10.0%", "Too little received", // answers and failures
		"won · reverted at T30", "no route", // latest rounds
		"internal/aggregator/b", // its adapter
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Index(body, "$100<") > strings.Index(body, "$10k<") {
		t.Error("sizes are not smallest first")
	}
	if db.filter.Aggregator != "b" || db.filter.ChainID != 8453 {
		t.Errorf("filter %+v", db.filter)
	}
	// The leaderboard is the nav's current page.
	nav := body[strings.Index(body, "<nav>"):strings.Index(body, "</nav>")]
	if strings.Count(nav, "<a ") != 3 || !strings.Contains(nav, `href="/" class="on"`) {
		t.Errorf("nav = %s", nav)
	}
	for _, path := range []string{"/aggregators/nobody", "/aggregators/Bad-Id"} {
		if w := get(t, s.Handler(), path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

func TestLogos(t *testing.T) {
	s, _ := server(t)
	h := s.Handler()
	if w := get(t, h, "/static/logos/nordstern.svg"); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" {
		t.Errorf("logo: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get(t, h, "/static/logos/"); w.Code != http.StatusNotFound {
		t.Errorf("directory listing: %d", w.Code)
	}
	// An aggregator with a logo shows it, one without its monogram.
	if got := avatar("kyberswap", "lg"); got != `<span class="av lg logo"><img src="/static/logos/kyberswap.png" alt="" width="48" height="48"></span>` {
		t.Errorf("logo avatar = %s", got)
	}
	if got := string(avatar("nobody", "")); got != `<span class="av" style="--c: `+color("nobody")+`">N</span>` {
		t.Errorf("monogram avatar = %s", got)
	}
}

func TestMethodologyPage(t *testing.T) {
	s, _ := server(t)
	w := get(t, s.Handler(), "/methodology")
	body := w.Body.String()
	for _, want := range []string{"Methodology version " + s.cfg.MethodologyVersion + ".", `<section id="fair">`,
		`href="` + repo + `/blob/main/METHODOLOGY.md"`, "Quote accuracy", "Revert rate"} {
		if w.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("page lacks %q (%d)", want, w.Code)
		}
	}
	if strings.Contains(body, "Changelog") {
		t.Error("the page shows the history")
	}
}

// TestStylesBalanced checks that every rule in the stylesheet is closed: one
// left open swallows every rule after it.
func TestStylesBalanced(t *testing.T) {
	b, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	css := s[strings.Index(s, "<style>"):strings.Index(s, "</style>")]
	depth := 0
	for _, c := range css {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
		}
		if depth < 0 {
			t.Fatal("a } closes nothing")
		}
	}
	if depth != 0 {
		t.Errorf("%d rules left open", depth)
	}
}
