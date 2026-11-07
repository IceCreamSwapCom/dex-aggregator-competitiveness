package web

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/store"
)

const maxLimit = 1000

func (s *Server) leaderboardAPI(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.parse(w, r)
	if !ok {
		return
	}
	sel.Aggregator = "" // the leaderboard is of every aggregator
	rows, err := s.board.Leaderboard(r.Context(), sel.filter())
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, api.Leaderboard{
		ChainID: sel.ChainID, Period: sel.Period, Size: sel.Size, Region: sel.Region, Since: sel.Since,
		RefreshedAt: s.refreshedAt(), Rows: rows,
	})
}

func (s *Server) roundAPI(w http.ResponseWriter, r *http.Request) {
	rd, ok := s.findRound(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400") // a stored round never changes
	writeJSON(w, rd)
}

func (s *Server) roundsAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var chainID uint64
	before, limit := time.Now(), 100
	var err error
	if v := q.Get("chain"); v != "" {
		if chainID, err = strconv.ParseUint(v, 10, 64); err != nil {
			http.Error(w, "chain is not a chain id", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("before"); v != "" {
		if before, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "before is not an RFC 3339 time", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > maxLimit {
			http.Error(w, "limit is not between 1 and 1000", http.StatusBadRequest)
			return
		}
	}
	refs, err := s.rounds.Rounds(r.Context(), chainID, before, limit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, refs)
}

// findRound reads the round the path names, or answers 404.
func (s *Server) findRound(w http.ResponseWriter, r *http.Request) (*api.Round, bool) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		http.Error(w, "no such round", http.StatusNotFound)
		return nil, false
	}
	rd, err := s.rounds.Round(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such round", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		serverError(w, r, err)
		return nil, false
	}
	return rd, true
}

// exportCSV streams the quotes of a leaderboard's selection as CSV, the rows
// its numbers are computed from.
func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.parse(w, r)
	if !ok {
		return
	}
	select {
	case s.exports <- struct{}{}:
		defer func() { <-s.exports }()
	default:
		w.Header().Set("Retry-After", "60")
		http.Error(w, "other exports are running; try again in a minute", http.StatusTooManyRequests)
		return
	}
	// A month of rounds takes longer than the server's write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(exportTimeout))
	ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
	defer cancel()
	chain := "all"
	if sel.ChainID != 0 {
		chain = strconv.FormatUint(sel.ChainID, 10)
	}
	name := "dexbench-" + chain + "-" + sel.Period
	for _, part := range []string{sel.Aggregator, sel.Size, sel.Region} {
		if part != "" {
			name += "-" + part
		}
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + "-" + time.Now().UTC().Format("20060102") + ".csv"}))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	sw := &sentWriter{w: w}
	var out io.Writer = sw
	var gz *gzip.Writer
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") { // CSV shrinks about tenfold
		w.Header().Set("Content-Encoding", "gzip")
		gz = gzip.NewWriter(sw)
		out = gz
	}
	err := s.board.Export(ctx, sel.filter(), out)
	switch {
	case err == nil && gz != nil:
		_ = gz.Close()
	case err != nil && !sw.sent:
		w.Header().Del("Content-Encoding")
		w.Header().Del("Content-Disposition")
		serverError(w, r, err)
	case err != nil:
		// Part of the file is out: all that is left is to cut the download short.
		slog.Error("export broke off", "err", err)
	}
}

// sentWriter notes whether anything reached the client yet.
type sentWriter struct {
	w    io.Writer
	sent bool
}

func (s *sentWriter) Write(p []byte) (int, error) {
	s.sent = true
	return s.w.Write(p)
}
