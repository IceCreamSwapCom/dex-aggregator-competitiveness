package web

import (
	"fmt"
	"html/template"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
)

var funcs = template.FuncMap{
	"pct":    pct,
	"bps":    bps,
	"short":  short,
	"time":   func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"gwei":   func(wei string) string { return units(wei, 9) },
	"ptr":    func(v float64) *float64 { return &v },
	"int":    func(v uint64) string { return units(strconv.FormatUint(v, 10), 0) },
	"avatar": avatar,
	"name":   name,
	"menu": func(label string, pills []pill) any {
		return struct {
			Label string
			Pills []pill
		}{label, pills}
	},
	"ago":    ago,
	"tip":    tip,
	"head":   head,
	"region": regionName,
	"asset":  asset,
	"unix":   func(t time.Time) int64 { return t.Unix() },
	"bucket": config.BucketValue,
	"groups": func(name string, rows []namedGroup, charts []chart) any {
		return struct {
			Name   string
			Rows   []namedGroup
			Charts []chart
		}{name, rows, charts}
	},
	"count": thousands,
	"accuracy": func(v *float64) string {
		if v == nil {
			return "–"
		}
		return basisPoints(*v)
	},
	"millis": millis,
	"periodName": func(p string) string {
		return map[string]string{"24h": "the last 24 hours", "7d": "the last 7 days", "30d": "the last 30 days"}[p]
	},
	"width": func(v *float64) string {
		if v == nil {
			return "0"
		}
		return fmt.Sprintf("%.1f", *v*100)
	},
	"sortv": func(v *float64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'g', 8, 64)
	},
}

// tip is the one way a page explains something: a ? that shows text on hover
// and focus (app.js).
func tip(text string) template.HTML {
	return template.HTML(`<span class="tip" tabindex="0" data-tip="` + template.HTMLEscapeString(text) + `">?</span>`)
}

// head is the head of a table column: its title, and a tip saying what it means.
func head(title, help string) template.HTML {
	return template.HTML(`<span class="h">` + template.HTMLEscapeString(title) + " " + string(tip(help)) + `</span>`)
}

// ago is how long before now t was, roughly.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// pct is a share as a percentage; – when there is nothing to compute it from.
func pct(v *float64) string {
	if v == nil {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", *v*100)
}

// bps is a signed basis-point difference.
func bps(v *float64) string {
	if v == nil {
		return "–"
	}
	if math.Abs(*v) < 0.05 {
		return "0.0"
	}
	return fmt.Sprintf("%+.1f", *v)
}

// short is an address as 0x1234…abcd.
func short(addr string) string {
	if len(addr) < 12 {
		return addr
	}
	return addr[:6] + "…" + addr[len(addr)-4:]
}

// units is an amount of base units in whole tokens, with thousands separators
// and 10 significant digits: enough to see a 0.01 bp difference.
func units(base string, decimals int) string {
	v, ok := new(big.Int).SetString(base, 10)
	if !ok {
		return base
	}
	sign := ""
	if v.Sign() < 0 {
		sign = "−"
		v.Abs(v)
	}
	s := v.String()
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-decimals], s[len(s)-decimals:]
	keep := max(0, 10-len(whole))
	if whole == "0" {
		keep = len(frac) - len(strings.TrimLeft(frac, "0")) + 10
	}
	frac = strings.TrimRight(frac[:min(len(frac), keep)], "0")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if frac == "" {
		return sign + whole
	}
	return sign + whole + "." + frac
}

// deltaBps is (got − quoted) / quoted in basis points.
func deltaBps(got, quoted string) *float64 {
	g, ok1 := new(big.Rat).SetString(got)
	q, ok2 := new(big.Rat).SetString(quoted)
	if !ok1 || !ok2 || q.Sign() == 0 {
		return nil
	}
	d, _ := new(big.Rat).Quo(new(big.Rat).Sub(g, q), q).Float64()
	d *= 10_000
	return &d
}
