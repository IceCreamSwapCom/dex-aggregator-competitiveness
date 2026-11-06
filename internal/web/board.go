package web

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
)

// The dashboard puts the leaderboard's metrics in four plain-language
// categories: what an integrating project asks of an aggregator. Each is one
// metric of the leaderboard as the methodology defines it, so every number on
// the page is in the JSON API too.

// category is one of them: how to read it from a row, and how to show it.
type category struct {
	Title  string
	Unit   string // under the title: what the number is
	Help   string // what it measures, precisely
	Lower  bool   // lower is better
	value  func(r api.LeaderboardRow) *float64
	format func(v float64) string
}

var categories = []category{
	{Title: "Best rate", Unit: "of trades won",
		Help: "Share of the compared trades where it gave the user the most of all aggregators: what the user receives " +
			"when its transaction is executed, minus the gas it costs. A trade is compared when at least two aggregators " +
			"delivered. Exact ties split the win.",
		value: func(r api.LeaderboardRow) *float64 { return r.Win }, format: percent},
	{Title: "Quote accuracy", Unit: "avg. bp vs. quote",
		Help: "How much the user received compared with its quote, on average, when the transaction was executed 30 " +
			"seconds after the quote, about the time a user takes to sign it in a wallet: −1.50 bp is 0.015% less than " +
			"quoted. Receiving more than quoted counts as 0, so 0 is the best possible.",
		value: func(r api.LeaderboardRow) *float64 { return r.AccuracyT30 }, format: basisPoints},
	{Title: "Revert rate", Unit: "of returned transactions", Lower: true,
		Help: "Share of the transactions it returned that reverted when executed 30 seconds after the quote. A " +
			"transaction that already reverts in the block it was quoted at is not executed again and counts as " +
			"reverted. Requests answered without a transaction (an error, no route, a timeout) are not counted here; " +
			"they lose it the trade under best rate.",
		value: func(r api.LeaderboardRow) *float64 { return r.Reverts }, format: percent},
	{Title: "Latency", Unit: "median response time", Lower: true,
		Help:  "Median time from sending the request to the API until it answered with a quote and its transaction. A timeout counts as 5,000 ms.",
		value: func(r api.LeaderboardRow) *float64 { v := r.LatencyP50Ms; return &v }, format: millis},
}

func percent(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

// basisPoints is a signed bp value, with a true minus sign; no sign on what
// rounds to 0.
func basisPoints(v float64) string {
	s := fmt.Sprintf("%.2f bp", math.Abs(v))
	if v < 0 && s != "0.00 bp" {
		return "−" + s
	}
	return s
}
func millis(v float64) string { return thousands(int64(math.Round(v))) + " ms" }

func thousands(v int64) string {
	s := fmt.Sprint(v)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// names are the aggregators' names as they write them; colors their brand's,
// roughly. An aggregator without one gets its id and a color of its own.
var names = map[string]string{"nordstern": "Nordstern", "kyberswap": "KyberSwap", "velora": "Velora", "sushi": "Sushi",
	"fly": "Fly", "zerox": "0x", "lifi": "LI.FI", "enso": "Enso"}

var colors = map[string]string{"nordstern": "#f2b84b", "kyberswap": "#31cb9e", "velora": "#4f7bff", "sushi": "#f45aa6",
	"fly": "#9f7aea", "zerox": "#cfd3dc", "lifi": "#d946ef", "enso": "#38bdf8"}

func name(id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id
}

func color(id string) string {
	if c, ok := colors[id]; ok {
		return c
	}
	h := fnv.New32a()
	h.Write([]byte(id))
	return palette[h.Sum32()%uint32(len(palette))]
}

// palette is for the aggregators without a color of their own.
var palette = []string{"#60a5fa", "#f472b6", "#34d399", "#fbbf24", "#a78bfa", "#fb923c", "#22d3ee", "#f87171"}

// monogram is what an aggregator's avatar shows.
func monogram(id string) string {
	n := name(id)
	if strings.HasPrefix(n, "0x") {
		return "0x"
	}
	return strings.ToUpper(n[:1])
}

// row is a leaderboard row as the tables show it.
type row struct {
	api.LeaderboardRow
	Pos        int
	Name, Href string
	Metrics    []metric // one per category, filled by withMetrics
}

func rows(board []api.LeaderboardRow, query string) []row {
	out := make([]row, len(board))
	for i, r := range board {
		out[i] = row{LeaderboardRow: r, Pos: i + 1, Name: name(r.Aggregator),
			Href: "/aggregators/" + r.Aggregator + "?" + query}
	}
	return out
}

// metric is one category's value in a leaderboard row.
type metric struct {
	Text string
	V    *float64 // what the column sorts by
	Bar  float64  // in % of the longest
	Best bool     // first of all aggregators, maybe tied
	Next bool     // second: right behind the best, maybe tied
}

// withMetrics fills every row's metrics from the charts of the same rows.
func withMetrics(rows []row, charts []chart) {
	for i := range rows {
		for _, ch := range charts {
			for _, st := range ch.Rows {
				if st.Aggregator == rows[i].Aggregator {
					rows[i].Metrics = append(rows[i].Metrics, metric{Text: st.Text, V: ch.value(st.LeaderboardRow), Bar: st.Bar, Best: st.Rank == 1, Next: st.Rank == 2})
				}
			}
		}
	}
}

// standing is one aggregator's place in a category.
type standing struct {
	row
	Rank int // 0 without a value
	Text string
	Bar  float64 // the bar's length, in % of the longest
}

// chart is a category with every aggregator in it, best first.
type chart struct {
	category
	Rows []standing
}

// rank orders rows by a category. Equal values as shown share a rank.
func rank(c category, rows []row) chart {
	ch := chart{category: c}
	var top float64
	for _, r := range rows {
		st := standing{row: r, Text: "–"}
		if v := c.value(r.LeaderboardRow); v != nil {
			st.Text = c.format(*v)
			top = math.Max(top, math.Abs(*v))
		}
		ch.Rows = append(ch.Rows, st)
	}
	val := func(st standing) (float64, bool) {
		v := c.value(st.LeaderboardRow)
		if v == nil {
			return 0, false
		}
		if c.Lower {
			return -*v, true
		}
		return *v, true
	}
	slices.SortStableFunc(ch.Rows, func(a, b standing) int {
		va, oka := val(a)
		vb, okb := val(b)
		switch {
		case oka != okb:
			if oka {
				return -1
			}
			return 1
		case va > vb:
			return -1
		case va < vb:
			return 1
		}
		return 0
	})
	for i := range ch.Rows {
		st := &ch.Rows[i]
		v := c.value(st.LeaderboardRow)
		if v == nil {
			continue
		}
		st.Rank = i + 1
		if i > 0 && ch.Rows[i-1].Text == st.Text {
			st.Rank = ch.Rows[i-1].Rank
		}
		if top > 0 {
			st.Bar = math.Abs(*v) / top * 100
		}
	}
	return ch
}

func charts(rows []row) []chart {
	out := make([]chart, len(categories))
	for i, c := range categories {
		out[i] = rank(c, rows)
	}
	return out
}

// pill is one choice of a filter: a link to the same page with it chosen.
type pill struct {
	Label, Href string
	On          bool
}

func (s *Server) pills(action string, sel selection, regions []string) (chains, periodPills, sizePills, regionPills []pill) {
	link := func(change func(*selection)) string {
		v := sel
		change(&v)
		return action + "?" + v.query()
	}
	chains = append(chains, pill{"All chains", link(func(v *selection) { v.ChainID, v.Size = 0, "" }), sel.ChainID == 0})
	for _, ch := range s.cfg.Chains {
		chains = append(chains, pill{ch.Name, link(func(v *selection) {
			v.ChainID = ch.ID
			if !slices.Contains(sizes(ch), v.Size) {
				v.Size = ""
			}
		}), sel.ChainID == ch.ID})
	}
	for _, p := range periods {
		periodPills = append(periodPills, pill{p.Name, link(func(v *selection) { v.Period = p.Name }), sel.Period == p.Name})
	}
	sizePills = append(sizePills, pill{"Any size", link(func(v *selection) { v.Size = "" }), sel.Size == ""})
	for _, b := range sel.Sizes {
		sizePills = append(sizePills, pill{"$" + b, link(func(v *selection) { v.Size = b }), sel.Size == b})
	}
	if len(regions) > 1 {
		regionPills = append(regionPills, pill{"All regions", link(func(v *selection) { v.Region = "" }), sel.Region == ""})
		for _, r := range regions {
			regionPills = append(regionPills, pill{r, link(func(v *selection) { v.Region = r }), sel.Region == r})
		}
	}
	return chains, periodPills, sizePills, regionPills
}
