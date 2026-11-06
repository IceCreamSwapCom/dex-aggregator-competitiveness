package web

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/stats"
)

//go:embed templates
var templateFS embed.FS

// pages are parsed once: each page template together with the layout.
var pages = func() map[string]*template.Template {
	m := map[string]*template.Template{}
	for _, p := range []string{"leaderboard", "aggregator", "rounds", "round", "methodology"} {
		m[p] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+p+".html"))
	}
	return m
}()

func render(w http.ResponseWriter, r *http.Request, page string, data any) {
	var b bytes.Buffer
	if err := pages[page].ExecuteTemplate(&b, "layout", struct {
		Page string
		Data any
	}{page, data}); err != nil {
		serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = b.WriteTo(w)
}

// filterBar is what the filter bar of the leaderboard and aggregator pages shows.
type filterBar struct {
	Sel                                             selection
	ChainPills, PeriodPills, SizePills, RegionPills []pill
	CSV, JSON                                       string // the selection from the API: its quotes, and the leaderboard
	Query                                           string // the selection of every aggregator, as a query string
	Refreshed                                       time.Time
}

func (s *Server) filterBar(r *http.Request, action string, sel selection) (filterBar, error) {
	regions, err := s.board.Regions(r.Context())
	board := sel
	board.Aggregator = ""
	bar := filterBar{Sel: sel, Refreshed: s.refreshedAt(),
		CSV: "/api/v1/export.csv?" + sel.query(), JSON: "/api/v1/leaderboard?" + board.query(), Query: board.query()}
	bar.ChainPills, bar.PeriodPills, bar.SizePills, bar.RegionPills = s.pills(action, sel, regions)
	return bar, err
}

type leaderboardData struct {
	filterBar
	Categories []category
	Rows       []row
	Rounds     int64 // rounds it covers
}

func (s *Server) leaderboardPage(w http.ResponseWriter, r *http.Request) {
	sel, err := s.selection(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sel.Aggregator = "" // the leaderboard is of every aggregator
	ctx, f := r.Context(), sel.filter()
	board, err1 := s.board.Leaderboard(ctx, f)
	bar, err2 := s.filterBar(r, "/", sel)
	if err := errors.Join(err1, err2); err != nil {
		serverError(w, r, err)
		return
	}
	d := leaderboardData{filterBar: bar, Categories: categories, Rows: rows(board, bar.Query)}
	withMetrics(d.Rows, charts(d.Rows))
	for _, row := range board {
		d.Rounds = max(d.Rounds, row.Requests)
	}
	render(w, r, "leaderboard", d)
}

type aggregatorData struct {
	filterBar
	ID, Name string
	Row      *api.LeaderboardRow // nil without rounds in the selection
	Rank, Of int                 // its place on the leaderboard of the same selection
	Charts   []chart
	ByChain  []namedGroup // only when the selection covers more than one chain
	BySize   []namedGroup
	Answers  []share
	Failures []stats.Count
	Quotes   []quoteItem
	Source   string // the adapter's code
	Board    string // the leaderboard of the same selection
}

type namedGroup struct {
	Name string
	stats.Group
}

type share struct {
	Name  string
	N     int64
	Share float64
}

type quoteItem struct {
	stats.QuoteRef
	Chain, In, Out, Amount string
	Result, Class          string // how the quote did, and its color
}

const (
	repo          = "https://github.com/IceCreamSwapCom/dex-aggregator-competitiveness"
	failuresShown = 8
	quotesShown   = 25
	sourceTree    = repo + "/tree/main/internal/aggregator/"
)

func (s *Server) aggregatorPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sel, err := s.selection(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !idPattern.MatchString(id) {
		http.Error(w, "no such aggregator", http.StatusNotFound)
		return
	}
	boardSel := sel
	boardSel.Aggregator = ""
	sel.Aggregator = id
	ctx, f := r.Context(), sel.filter()
	board, err := s.board.Leaderboard(ctx, boardSel.filter())
	if err != nil {
		serverError(w, r, err)
		return
	}
	bar, err := s.filterBar(r, "/aggregators/"+id, sel)
	if err != nil {
		serverError(w, r, err)
		return
	}
	d := aggregatorData{filterBar: bar, ID: id, Name: name(id),
		Of: len(board), Source: sourceTree + id, Board: "/?" + boardSel.query()}
	for i, row := range board {
		if row.Aggregator == id {
			d.Row, d.Rank = &board[i], i+1
		}
	}
	if d.Row == nil && !s.configured(id) {
		http.Error(w, "no such aggregator", http.StatusNotFound)
		return
	}
	d.Charts = charts(rows(board, bar.Query))

	byChain, err1 := s.board.ByChain(ctx, f)
	bySize, err2 := s.board.BySize(ctx, f)
	statuses, err3 := s.board.Statuses(ctx, f)
	failures, err4 := s.board.Failures(ctx, f, failuresShown)
	quotes, err5 := s.board.Quotes(ctx, f, quotesShown)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		serverError(w, r, err)
		return
	}
	if len(byChain) > 1 {
		for _, g := range byChain {
			id, _ := strconv.ParseUint(g.Key, 10, 64)
			d.ByChain = append(d.ByChain, namedGroup{Name: s.chainName(id), Group: g})
		}
	}
	slices.SortFunc(bySize, func(a, b stats.Group) int { return int(config.BucketValue(a.Key) - config.BucketValue(b.Key)) })
	for _, g := range bySize {
		d.BySize = append(d.BySize, namedGroup{Name: "$" + g.Key, Group: g})
	}
	var total int64
	for _, c := range statuses {
		total += c.N
	}
	for _, c := range statuses {
		d.Answers = append(d.Answers, share{Name: strings.ReplaceAll(c.Key, "_", " "), N: c.N, Share: float64(c.N) / float64(total)})
	}
	d.Failures = failures
	for _, q := range quotes {
		in, inDecimals := s.token(q.ChainID, q.TokenIn)
		out, _ := s.token(q.ChainID, q.TokenOut)
		item := quoteItem{QuoteRef: q, Chain: s.chainName(q.ChainID), In: in, Out: out, Amount: units(q.AmountIn, inDecimals)}
		item.Result, item.Class = result(q)
		d.Quotes = append(d.Quotes, item)
	}
	render(w, r, "aggregator", d)
}

// result says in a few words how a quote did, and with which color.
func result(q stats.QuoteRef) (string, string) {
	switch {
	case q.Status != "ok":
		return strings.ReplaceAll(q.Status, "_", " "), "muted"
	case q.T0 == nil || !*q.T0:
		return "reverted at T0", "bad"
	}
	text, class := "", ""
	switch {
	case q.Ranked == nil:
		text, class = "not scored yet", "muted"
	case !*q.Ranked:
		text, class = "not ranked", "muted"
	case q.WinShare != nil && *q.WinShare == 1:
		text, class = "won", "good"
	case q.WinShare != nil && *q.WinShare > 0:
		text, class = "tied win", "good"
	case q.Within1 != nil && *q.Within1:
		text = "within 1 bp"
	case q.Within5 != nil && *q.Within5:
		text = "within 5 bp"
	default:
		text = "behind"
	}
	if q.T30 != nil && !*q.T30 {
		text += " · reverted at T30"
	}
	return text, class
}

type roundsData struct {
	ChainPills []pill
	Rows       []roundRow
	Older      string // link to the next page, "" on the last
}

type roundRow struct {
	api.RoundRef
	Chain, In, Out, Amount string
}

const roundsPerPage = 50

func (s *Server) roundsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	chain := q.Get("chain")
	var chainID uint64
	if chain != "" {
		ch, ok := s.chain(chain)
		if !ok {
			http.Error(w, "chain is not benchmarked", http.StatusBadRequest)
			return
		}
		chainID = ch.ID
	}
	d := roundsData{ChainPills: []pill{{"All chains", "/rounds", chainID == 0}}}
	for _, ch := range s.cfg.Chains {
		d.ChainPills = append(d.ChainPills, pill{ch.Name, "/rounds?chain=" + strconv.FormatUint(ch.ID, 10), ch.ID == chainID})
	}
	before := time.Now()
	if v := q.Get("before"); v != "" {
		var err error
		if before, err = time.Parse(time.RFC3339Nano, v); err != nil {
			http.Error(w, "before is not an RFC 3339 time", http.StatusBadRequest)
			return
		}
	}
	refs, err := s.rounds.Rounds(r.Context(), chainID, before, roundsPerPage)
	if err != nil {
		serverError(w, r, err)
		return
	}
	for _, ref := range refs {
		in, inDecimals := s.token(ref.ChainID, ref.TokenIn)
		out, _ := s.token(ref.ChainID, ref.TokenOut)
		d.Rows = append(d.Rows, roundRow{RoundRef: ref, Chain: s.chainName(ref.ChainID), In: in, Out: out, Amount: units(ref.AmountIn, inDecimals)})
	}
	if len(refs) == roundsPerPage {
		d.Older = "/rounds?chain=" + chain + "&before=" + refs[len(refs)-1].StartedAt.Format(time.RFC3339Nano)
	}
	render(w, r, "rounds", d)
}

type roundData struct {
	R              *api.Round
	Chain, In, Out string
	Amount         string
	Quotes         []quoteRow
	Scored         bool // the metrics include the round already
	Ranked         bool
	Unpriced       bool // a quote succeeded at T0, but its gas could not be valued
	Resim          string
}

type quoteRow struct {
	api.Quote
	Quoted, T0Out, T30Out, Value string
	T0Delta, T30Delta            *float64
	Score                        stats.Score
	GasCost, Net                 string
}

func (s *Server) roundPage(w http.ResponseWriter, r *http.Request) {
	rd, ok := s.findRound(w, r)
	if !ok {
		return
	}
	scores, err := s.board.Round(r.Context(), rd.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	in, inDecimals := s.token(rd.ChainID, rd.TokenIn)
	out, outDecimals := s.token(rd.ChainID, rd.TokenOut)
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	d := roundData{R: rd, Chain: s.chainName(rd.ChainID), In: in, Out: out, Amount: units(rd.AmountIn, inDecimals),
		Scored: len(scores) > 0, Resim: "go run ./cmd/resim -api " + scheme + "://" + r.Host + " " + rd.ID}
	for _, q := range rd.Quotes {
		row := quoteRow{Quote: q, Quoted: units(q.QuotedOut, outDecimals), Value: units(q.TxValue, 18), Score: scores[q.Aggregator]}
		if q.T0 != nil && q.T0.Success {
			row.T0Out, row.T0Delta = units(q.T0.AmountOut, outDecimals), deltaBps(q.T0.AmountOut, q.QuotedOut)
		}
		if q.T30 != nil && q.T30.Success {
			row.T30Out, row.T30Delta = units(q.T30.AmountOut, outDecimals), deltaBps(q.T30.AmountOut, q.QuotedOut)
		}
		if row.Score.GasCost != "" {
			row.GasCost = units(row.Score.GasCost, outDecimals)
		}
		if row.Score.Net != "" {
			row.Net = units(row.Score.Net, outDecimals)
		}
		d.Ranked = d.Ranked || row.Score.Ranked
		d.Unpriced = d.Unpriced || (d.Scored && q.T0 != nil && q.T0.Success && row.Score.GasCost == "")
		d.Quotes = append(d.Quotes, row)
	}
	render(w, r, "round", d)
}

// methodologyPage is the methodology in short; METHODOLOGY.md has the full rules.
func (s *Server) methodologyPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, "methodology", struct{ Version, Full, Adapters, Resim, Repo string }{
		s.cfg.MethodologyVersion, repo + "/blob/main/METHODOLOGY.md", repo + "/tree/main/internal/aggregator",
		repo + "/tree/main/cmd/resim", repo})
}
