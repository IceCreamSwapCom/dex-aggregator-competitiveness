// Package web serves the public, read-only side of the benchmark: the
// dashboard pages and the JSON API, both from the same queries.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/stats"
)

// Rounds reads stored rounds; *store.Store is one.
type Rounds interface {
	Round(ctx context.Context, id string) (*api.Round, error)
	Rounds(ctx context.Context, chainID uint64, before time.Time, limit int) ([]api.RoundRef, error)
}

// Board reads the metrics; *stats.Stats is one.
type Board interface {
	Leaderboard(ctx context.Context, f stats.Filter) ([]api.LeaderboardRow, error)
	Regions(ctx context.Context) ([]string, error)
	Round(ctx context.Context, id string) (map[string]stats.Score, error)
	Export(ctx context.Context, f stats.Filter, w io.Writer) error
	ByChain(ctx context.Context, f stats.Filter) ([]stats.Group, error)
	BySize(ctx context.Context, f stats.Filter) ([]stats.Group, error)
	Statuses(ctx context.Context, f stats.Filter) ([]stats.Count, error)
	Failures(ctx context.Context, f stats.Filter, limit int) ([]stats.Count, error)
	Quotes(ctx context.Context, f stats.Filter, limit int) ([]stats.QuoteRef, error)
}

// Server serves one database. The config names the chains and tokens.
type Server struct {
	cfg       *config.Config
	rounds    Rounds
	board     Board
	refreshed atomic.Int64 // unix seconds of the last metrics refresh
}

func New(cfg *config.Config, rounds Rounds, board Board) *Server {
	return &Server{cfg: cfg, rounds: rounds, board: board}
}

// Refreshed records when the metrics were last recomputed.
func (s *Server) Refreshed(t time.Time) { s.refreshed.Store(t.Unix()) }

func (s *Server) refreshedAt() time.Time { return time.Unix(s.refreshed.Load(), 0).UTC() }

// Handler serves
//
//	GET /                                           leaderboard
//	GET /aggregators/{id}                           one aggregator: its metrics in detail, failures, quotes
//	GET /rounds, /rounds/{id}                       round explorer
//	GET /methodology                                the methodology in short; links METHODOLOGY.md
//	GET /api/v1/leaderboard?chain=&period=&size=&region=
//	GET /api/v1/export.csv?chain=&period=&size=&region=&aggregator=   the quotes behind that leaderboard
//	GET /api/v1/rounds?chain=&before=&limit=        rounds that started before before (RFC 3339,
//	                                                default now), newest first, at most limit (100, ≤ 1000)
//	GET /api/v1/rounds/{id}                         one round, everything resim needs
//	GET /static/logos/{id}.png|svg                  the aggregators' logos
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.leaderboardPage)
	mux.HandleFunc("GET /aggregators/{id}", s.aggregatorPage)
	mux.HandleFunc("GET /rounds", s.roundsPage)
	mux.HandleFunc("GET /rounds/{id}", s.roundPage)
	mux.HandleFunc("GET /methodology", s.methodologyPage)
	mux.HandleFunc("GET /api/v1/leaderboard", s.leaderboardAPI)
	mux.HandleFunc("GET /api/v1/export.csv", s.exportCSV)
	mux.HandleFunc("GET /api/v1/rounds", s.roundsAPI)
	mux.HandleFunc("GET /api/v1/rounds/{id}", s.roundAPI)
	mux.Handle("GET /static/", static())
	return mux
}

// period is how far back a leaderboard reaches.
type period struct {
	Name string
	D    time.Duration
}

var periods = []period{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}}

// selection is a leaderboard's filter as the query string gave it.
type selection struct {
	ChainID    uint64 // 0: all chains
	ChainName  string
	Sizes      []string // the size buckets of the selected chains
	Period     string
	Size       string
	Region     string
	Aggregator string // only this aggregator's quotes, "" for all
	Since      time.Time
}

// selection reads chain (default: all chains), period (default 7d), size and
// region from the query string.
func (s *Server) selection(q url.Values) (selection, error) {
	sel := selection{ChainName: "All chains", Sizes: sizes(s.cfg.Chains...), Period: "7d", Size: q.Get("size"), Region: q.Get("region")}
	if v := q.Get("chain"); v != "" {
		ch, ok := s.chain(v)
		if !ok {
			return sel, fmt.Errorf("chain %q is not benchmarked", v)
		}
		sel.ChainID, sel.ChainName, sel.Sizes = ch.ID, ch.Name, sizes(ch)
	}
	if v := q.Get("period"); v != "" {
		sel.Period = v
	}
	i := slices.IndexFunc(periods, func(p period) bool { return p.Name == sel.Period })
	if i < 0 {
		return sel, errors.New("period is not one of 24h, 7d, 30d")
	}
	if v := q.Get("aggregator"); v != "" {
		if !idPattern.MatchString(v) {
			return sel, fmt.Errorf("aggregator %q is not an aggregator id", v)
		}
		sel.Aggregator = v
	}
	if sel.Size != "" && !slices.Contains(sel.Sizes, sel.Size) {
		return sel, fmt.Errorf("size %q is not a size bucket", sel.Size)
	}
	sel.Since = time.Now().Add(-periods[i].D).UTC()
	return sel, nil
}

func (sel selection) filter() stats.Filter {
	return stats.Filter{ChainID: sel.ChainID, Since: sel.Since, Size: sel.Size, Region: sel.Region, Aggregator: sel.Aggregator}
}

// query is the selection as a query string, for the JSON and CSV links.
func (sel selection) query() string {
	q := url.Values{"period": {sel.Period}}
	if sel.ChainID != 0 {
		q.Set("chain", strconv.FormatUint(sel.ChainID, 10))
	}
	if sel.Size != "" {
		q.Set("size", sel.Size)
	}
	if sel.Region != "" {
		q.Set("region", sel.Region)
	}
	if sel.Aggregator != "" {
		q.Set("aggregator", sel.Aggregator)
	}
	return q.Encode()
}

// configured reports whether the config lists the aggregator.
func (s *Server) configured(id string) bool { return slices.Contains(s.cfg.Aggregators, id) }

// chain finds a configured chain by its decimal id.
func (s *Server) chain(id string) (config.Chain, bool) {
	for _, ch := range s.cfg.Chains {
		if fmt.Sprint(ch.ID) == id {
			return ch, true
		}
	}
	return config.Chain{}, false
}

func (s *Server) chainName(id uint64) string {
	for _, ch := range s.cfg.Chains {
		if ch.ID == id {
			return ch.Name
		}
	}
	return fmt.Sprintf("chain %d", id)
}

// token is a token of the config by address. A token the config no longer
// has is shown by its address, in base units.
func (s *Server) token(chainID uint64, address string) (symbol string, decimals int) {
	for _, ch := range s.cfg.Chains {
		if ch.ID != chainID {
			continue
		}
		for name, t := range ch.Tokens {
			if strings.EqualFold(t.Address, address) {
				return name, t.Decimals
			}
		}
	}
	return short(address), 0
}

// sizes are the size buckets of the chains, smallest first.
func sizes(chains ...config.Chain) []string {
	seen := map[string]bool{}
	var out []string
	for _, ch := range chains {
		for _, t := range ch.Tokens {
			for b := range t.Sizes {
				if !seen[b] {
					seen[b] = true
					out = append(out, b)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b string) int { return int(config.BucketValue(a) - config.BucketValue(b)) })
	return out
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// idPattern is what an aggregator id looks like.
var idPattern = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // public data
	_ = json.NewEncoder(w).Encode(v)
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
