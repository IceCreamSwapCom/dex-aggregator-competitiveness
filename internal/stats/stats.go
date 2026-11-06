// Package stats reads the dashboard's numbers from the quote_metrics view,
// which metrics.sql defines.
package stats

import (
	"context"
	_ "embed"
	"encoding/csv"
	"io"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed metrics.sql
var metrics string

// Stats queries one database.
type Stats struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Stats { return &Stats{pool: pool} }

// Create (re)creates the view from metrics.sql, so that it always has the
// definitions of the running version.
func (s *Stats) Create(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, metrics)
	return err
}

// Refresh recomputes the view from the stored rounds. Readers keep seeing
// the previous numbers until it is done.
func (s *Stats) Refresh(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "REFRESH MATERIALIZED VIEW CONCURRENTLY quote_metrics")
	return err
}

// Filter selects the rounds a leaderboard covers. Zero values select all.
type Filter struct {
	ChainID    uint64
	Since      time.Time
	Size       string // size bucket
	Region     string
	Aggregator string // only this aggregator's quotes
}

func (f Filter) args() []any {
	return []any{int64(f.ChainID), f.Since, f.Size, f.Region, f.Aggregator}
}

// selected is the WHERE condition of a Filter on quote_metrics, its parameters $1 to $5.
const selected = `($1::bigint = 0 OR chain_id = $1) AND started_at >= $2 AND ($3 = '' OR size_bucket = $3)
	AND ($4 = '' OR region = $4) AND ($5 = '' OR aggregator = $5)`

// The columns metric rows can be grouped by. Only these constants go into the SQL.
const (
	byAggregator = "aggregator"
	byChain      = "chain_id::text"
	bySize       = "size_bucket"
)

// Group is the metrics of one group of quotes: of an aggregator, or of one
// aggregator's quotes on a chain or in a size bucket.
type Group struct {
	Key string // the aggregator, chain id or size bucket
	api.LeaderboardRow
}

// Leaderboard is one row per aggregator, best win rate first.
func (s *Stats) Leaderboard(ctx context.Context, f Filter) ([]api.LeaderboardRow, error) {
	groups, err := s.metrics(ctx, f, byAggregator)
	board := make([]api.LeaderboardRow, len(groups))
	for i, g := range groups {
		board[i] = g.LeaderboardRow
		board[i].Aggregator = g.Key
	}
	return board, err
}

// ByChain and BySize split f's metrics (usually one aggregator's) by chain
// and by size bucket, best win rate first.
func (s *Stats) ByChain(ctx context.Context, f Filter) ([]Group, error) {
	return s.metrics(ctx, f, byChain)
}
func (s *Stats) BySize(ctx context.Context, f Filter) ([]Group, error) {
	return s.metrics(ctx, f, bySize)
}

func (s *Stats) metrics(ctx context.Context, f Filter, group string) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+group+`, count(*), count(*) FILTER (WHERE ranked),
			(sum(win_share) FILTER (WHERE ranked) / nullif(count(*) FILTER (WHERE ranked), 0))::float8,
			(avg(within_1bp::int) FILTER (WHERE ranked))::float8,
			(avg(within_5bp::int) FILTER (WHERE ranked))::float8,
			avg(accuracy_t0_bps), avg(accuracy_t30_bps),
			-- Of the transactions it returned: reverted at T0, or at T30 after succeeding at T0.
			(avg((NOT t0_success OR NOT t30_success)::int) FILTER (WHERE NOT t0_success OR t30_success IS NOT NULL))::float8,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), percentile_cont(0.9) WITHIN GROUP (ORDER BY latency_ms),
			-- An answer with a transaction, whatever its simulations did; errors, timeouts and no_route are not.
			avg((status = 'ok')::int)::float8
		FROM quote_metrics WHERE `+selected+`
		GROUP BY 1
		ORDER BY 4 DESC NULLS LAST, 12 DESC, 1`, f.args()...)
	if err != nil {
		return nil, err
	}
	groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Group, error) {
		var g Group
		r := &g.LeaderboardRow
		err := row.Scan(&g.Key, &r.Requests, &r.RankedRounds, &r.Win, &r.Within1Bp, &r.Within5Bp,
			&r.AccuracyT0, &r.AccuracyT30, &r.Reverts,
			&r.LatencyP50Ms, &r.LatencyP90Ms, &r.Availability)
		return g, err
	})
	if groups == nil {
		groups = []Group{}
	}
	return groups, err
}

// Count is how often something happened.
type Count struct {
	Key, Detail string
	N           int64
}

// Statuses counts f's quotes by status.
func (s *Stats) Statuses(ctx context.Context, f Filter) ([]Count, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, '', count(*) FROM quote_metrics WHERE `+selected+`
		GROUP BY status ORDER BY 3 DESC`, f.args()...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Count])
}

// roundsSelected is the WHERE condition of a Filter on rounds r joined with quotes q.
const roundsSelected = `($1::bigint = 0 OR r.chain_id = $1) AND r.started_at >= $2 AND ($3 = '' OR r.size_bucket = $3)
	AND ($4 = '' OR r.region = $4) AND ($5 = '' OR q.aggregator = $5)`

// Failures are f's most common failures, at most limit: as Key what failed
// (a status other than ok, "reverted at T0", "reverted at T30"), as Detail
// the error or revert reason.
func (s *Stats) Failures(ctx context.Context, f Filter, limit int) ([]Count, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, reason, count(*) FROM (
			SELECT CASE WHEN q.status <> 'ok' THEN q.status
			            WHEN NOT q.t0_success THEN 'reverted at T0'
			            WHEN NOT q.t30_success THEN 'reverted at T30' END AS kind,
			       coalesce(CASE WHEN q.status <> 'ok' THEN q.error
			                     WHEN NOT q.t0_success THEN q.t0_revert
			                     ELSE q.t30_revert END, '') AS reason
			FROM quotes q JOIN rounds r ON r.id = q.round_id
			WHERE `+roundsSelected+`) failures
		WHERE kind IS NOT NULL
		GROUP BY kind, reason ORDER BY 3 DESC, 1, 2 LIMIT $6`, append(f.args(), limit)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Count])
}

// QuoteRef is one quote of a round in a listing: the trade, and how the quote did.
type QuoteRef struct {
	RoundID              string
	StartedAt            time.Time
	ChainID              uint64
	TokenIn, TokenOut    string
	AmountIn, SizeBucket string
	Status               string
	T0, T30              *bool    // the simulations' success; nil = not simulated
	Ranked               *bool    // nil until the metrics include the round
	WinShare             *float64 // in a ranked round
	Within1, Within5     *bool
}

// Quotes are f's quotes, newest first, at most limit.
func (s *Stats) Quotes(ctx context.Context, f Filter, limit int) ([]QuoteRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id::text, r.started_at, r.chain_id, r.token_in, r.token_out, r.amount_in::text,
			r.size_bucket, q.status, q.t0_success, q.t30_success, m.ranked, m.win_share::float8, m.within_1bp, m.within_5bp
		FROM quotes q JOIN rounds r ON r.id = q.round_id
		LEFT JOIN quote_metrics m ON m.round_id = q.round_id AND m.aggregator = q.aggregator
		WHERE `+roundsSelected+`
		ORDER BY r.started_at DESC LIMIT $6`, append(f.args(), limit)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[QuoteRef])
}

// Regions are the regions that have rounds.
func (s *Stats) Regions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT DISTINCT region FROM quote_metrics ORDER BY region")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Score is how one quote of a round was ranked. Amounts are tokenOut base units.
type Score struct {
	GasCost  string // empty when the gas could not be valued
	Net      string
	Ranked   bool
	WinShare float64
	Within1  bool
	Within5  bool
}

// Round is the scores of a round's quotes by aggregator; empty until the
// view's next refresh after the round.
func (s *Stats) Round(ctx context.Context, id string) (map[string]Score, error) {
	rows, err := s.pool.Query(ctx, `SELECT aggregator, coalesce(round(gas_cost)::text, ''), coalesce(round(net)::text, ''), ranked,
			coalesce(win_share, 0)::float8, coalesce(within_1bp, false), coalesce(within_5bp, false)
		FROM quote_metrics WHERE round_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scores := map[string]Score{}
	for rows.Next() {
		var agg string
		var sc Score
		if err := rows.Scan(&agg, &sc.GasCost, &sc.Net, &sc.Ranked, &sc.WinShare, &sc.Within1, &sc.Within5); err != nil {
			return nil, err
		}
		scores[agg] = sc
	}
	return scores, rows.Err()
}

// Export writes every quote of the rounds f selects as CSV, oldest first:
// the round, the quote, both simulations and how the quote ranked. Calldata
// and the raw answers are left out (they are in the round API); the ranking
// columns are empty until the metrics refresh after the round. Rows are
// streamed, so a month of rounds never sits in memory.
func (s *Stats) Export(ctx context.Context, f Filter, w io.Writer) error {
	rows, err := s.pool.Query(ctx, `SELECT r.id::text AS round_id,
			to_char(r.started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') AS started_at,
			r.region, r.chain_id::text, r.token_in, r.token_out, r.token_in_native::text, r.token_out_native::text,
			r.amount_in::text, r.size_bucket, r.slippage_bps::text, r.taker,
			r.t0_block::text, r.t0_gas_price::text, r.t30_block::text, r.t30_gas_price::text,
			r.methodology_version, r.collector_version,
			q.aggregator, q.status, q.error, q.latency_ms::text, q.quoted_out::text, q.tx_to, q.tx_value::text, q.spender,
			q.gas_limit::text, q.t0_success::text, q.t0_out::text, q.t0_gas::text, q.t0_revert,
			q.t30_success::text, q.t30_out::text, q.t30_gas::text, q.t30_revert,
			round(m.gas_cost)::text AS gas_cost, round(m.net)::text AS net, m.ranked::text, m.win_share::text,
			m.within_1bp::text, m.within_5bp::text
		FROM rounds r JOIN quotes q ON q.round_id = r.id
		LEFT JOIN quote_metrics m ON m.round_id = q.round_id AND m.aggregator = q.aggregator
		WHERE `+roundsSelected+`
		ORDER BY r.started_at, r.id, q.aggregator`, f.args()...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cw := csv.NewWriter(w)
	record := make([]string, len(rows.FieldDescriptions()))
	for i, fd := range rows.FieldDescriptions() {
		record[i] = fd.Name
	}
	if err := cw.Write(record); err != nil {
		return err
	}
	for rows.Next() {
		for i, v := range rows.RawValues() { // every column is text, NULL is empty
			record[i] = string(v)
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}
