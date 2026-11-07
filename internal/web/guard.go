package web

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/stats"
)

// The site is public and needs no login, and a month of metrics takes the
// database seconds to add up. So no traffic may cost it more than this:
//   - the metrics only change when the view is refreshed: their answers are
//     kept in memory until the next refresh (cachedBoard);
//   - at most queryWorkers of their queries run at once, however many
//     requests wait for them;
//   - at most exportWorkers CSV exports stream at once, one more is told to
//     try again later;
//   - any other request has requestTimeout, then its queries are cancelled.
const (
	queryWorkers   = 2
	exportWorkers  = 2
	cacheEntries   = 20_000 // a selection is chain × period × size × region × aggregator
	requestTimeout = 30 * time.Second
	exportTimeout  = 10 * time.Minute
)

// cachedBoard answers the metric queries from memory until the next refresh.
// The answers are shared between requests: nothing may change them.
type cachedBoard struct {
	Board               // Round (one round, by key) and Export (streamed) are not cached
	work  chan struct{} // a slot per query that may run at once
	mu    sync.Mutex
	gen   int // counts the refreshes
	m     map[string]any
}

func newCachedBoard(b Board) *cachedBoard {
	return &cachedBoard{Board: b, work: make(chan struct{}, queryWorkers), m: map[string]any{}}
}

// refreshed forgets every answer: the view has changed.
func (c *cachedBoard) refreshed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.m = map[string]any{}
}

func (c *cachedBoard) get(key string) (any, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, c.gen, ok
}

// cached is the answer to key from memory, or from query once a worker is free.
func cached[T any](ctx context.Context, c *cachedBoard, key string, query func() (T, error)) (T, error) {
	if v, _, ok := c.get(key); ok {
		return v.(T), nil
	}
	select {
	case c.work <- struct{}{}:
		defer func() { <-c.work }()
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
	// Another request may have asked the same while this one waited.
	v, gen, ok := c.get(key)
	if ok {
		return v.(T), nil
	}
	out, err := query()
	if err != nil {
		return out, err
	}
	c.mu.Lock()
	if c.gen == gen && len(c.m) < cacheEntries { // an answer from before a refresh is not kept
		c.m[key] = out
	}
	c.mu.Unlock()
	return out, nil
}

func (c *cachedBoard) Leaderboard(ctx context.Context, f stats.Filter) ([]api.LeaderboardRow, error) {
	return cached(ctx, c, fmt.Sprint("leaderboard", f), func() ([]api.LeaderboardRow, error) { return c.Board.Leaderboard(ctx, f) })
}

func (c *cachedBoard) Regions(ctx context.Context) ([]string, error) {
	return cached(ctx, c, "regions", func() ([]string, error) { return c.Board.Regions(ctx) })
}

func (c *cachedBoard) ByChain(ctx context.Context, f stats.Filter) ([]stats.Group, error) {
	return cached(ctx, c, fmt.Sprint("chain", f), func() ([]stats.Group, error) { return c.Board.ByChain(ctx, f) })
}

func (c *cachedBoard) BySize(ctx context.Context, f stats.Filter) ([]stats.Group, error) {
	return cached(ctx, c, fmt.Sprint("size", f), func() ([]stats.Group, error) { return c.Board.BySize(ctx, f) })
}

func (c *cachedBoard) Statuses(ctx context.Context, f stats.Filter) ([]stats.Count, error) {
	return cached(ctx, c, fmt.Sprint("statuses", f), func() ([]stats.Count, error) { return c.Board.Statuses(ctx, f) })
}

func (c *cachedBoard) Failures(ctx context.Context, f stats.Filter, limit int) ([]stats.Count, error) {
	return cached(ctx, c, fmt.Sprint("failures", f, limit), func() ([]stats.Count, error) { return c.Board.Failures(ctx, f, limit) })
}

func (c *cachedBoard) Quotes(ctx context.Context, f stats.Filter, limit int) ([]stats.QuoteRef, error) {
	return cached(ctx, c, fmt.Sprint("quotes", f, limit), func() ([]stats.QuoteRef, error) { return c.Board.Quotes(ctx, f, limit) })
}

// csp lets the pages load their own files and Google's fonts, and nothing
// else; no inline script runs. Inline styles are allowed: the bars set their
// widths with them.
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src https://fonts.gstatic.com; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// secure sets the security headers of every answer.
func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		hd.Set("Strict-Transport-Security", "max-age=31536000")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}
