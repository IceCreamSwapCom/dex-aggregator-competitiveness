// Package store writes finished rounds to Postgres and reads them back in
// their public form (package api).
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Round is one finished round.
type Round struct {
	Region             string
	ChainID            uint64
	TokenIn, TokenOut  string
	TokenInNative      bool // the native coin or its wrapped token
	TokenOutNative     bool
	AmountIn           *big.Int
	SizeBucket         string
	SlippageBps        int
	Taker              string
	StartedAt          time.Time
	T0Block            uint64
	T0GasPrice         *big.Int
	T30Block           uint64
	T30GasPrice        *big.Int
	MethodologyVersion string
	CollectorVersion   string
	Quotes             []Quote
}

// Quote is one aggregator's answer and what its transaction did.
type Quote struct {
	Aggregator string
	Status     string
	Error      string
	Latency    time.Duration
	QuotedOut  *big.Int
	TxTo       string
	TxData     string
	TxValue    *big.Int
	Spender    string
	Raw        json.RawMessage
	GasLimit   uint64
	T0, T30    *Simulation // nil = not simulated
}

// Simulation is the outcome of one measured run.
type Simulation struct {
	Success   bool
	AmountOut *big.Int
	Gas       uint64
	Revert    string
}

// Store is a Postgres connection pool.
type Store struct{ pool *pgxpool.Pool }

// Open connects and creates the tables if they are missing.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Insert writes a round and its quotes in one transaction and returns the
// round's id.
func (s *Store) Insert(ctx context.Context, r *Round) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO rounds (region, chain_id, token_in, token_out, token_in_native, token_out_native, amount_in, size_bucket,
				slippage_bps, taker, started_at, t0_block, t0_gas_price, t30_block, t30_gas_price,
				methodology_version, collector_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id::text`,
			r.Region, int64(r.ChainID), r.TokenIn, r.TokenOut, r.TokenInNative, r.TokenOutNative, num(r.AmountIn), r.SizeBucket,
			r.SlippageBps, r.Taker, r.StartedAt, int64(r.T0Block), num(r.T0GasPrice), int64(r.T30Block), num(r.T30GasPrice),
			r.MethodologyVersion, r.CollectorVersion,
		).Scan(&id)
		if err != nil {
			return err
		}
		for _, q := range r.Quotes {
			t0, t30 := sim(q.T0), sim(q.T30)
			_, err := tx.Exec(ctx, `INSERT INTO quotes (round_id, aggregator, status, error, latency_ms, quoted_out,
					tx_to, tx_data, tx_value, spender, raw_response, gas_limit,
					t0_success, t0_out, t0_gas, t0_revert, t30_success, t30_out, t30_gas, t30_revert)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
				id, q.Aggregator, q.Status, text(q.Error), q.Latency.Milliseconds(), num(q.QuotedOut),
				text(q.TxTo), text(q.TxData), num(q.TxValue), text(q.Spender), raw(q.Raw), gas(q.GasLimit),
				t0.success, t0.out, t0.gas, t0.revert, t30.success, t30.out, t30.gas, t30.revert,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// simColumns are a Simulation as nullable columns.
type simColumns struct {
	success pgtype.Bool
	out     pgtype.Numeric
	gas     pgtype.Int8
	revert  pgtype.Text
}

func sim(s *Simulation) simColumns {
	if s == nil {
		return simColumns{}
	}
	return simColumns{
		success: pgtype.Bool{Bool: s.Success, Valid: true},
		out:     num(s.AmountOut),
		gas:     gas(s.Gas),
		revert:  text(s.Revert),
	}
}

func num(v *big.Int) pgtype.Numeric {
	if v == nil {
		return pgtype.Numeric{}
	}
	return pgtype.Numeric{Int: v, Valid: true}
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func gas(g uint64) pgtype.Int8 { return pgtype.Int8{Int64: int64(g), Valid: g != 0} }

func raw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// ErrNotFound means there is no round with that id.
var ErrNotFound = errors.New("no such round")

// Round reads one round in its public form.
func (s *Store) Round(ctx context.Context, id string) (*api.Round, error) {
	r := api.Round{ID: id, Quotes: []api.Quote{}}
	err := s.pool.QueryRow(ctx, `SELECT region, chain_id, token_in, token_out, token_in_native, token_out_native, amount_in::text, size_bucket, slippage_bps,
			taker, started_at, t0_block, t0_gas_price::text, t30_block, t30_gas_price::text, methodology_version, collector_version
		FROM rounds WHERE id = $1`, id).
		Scan(&r.Region, &r.ChainID, &r.TokenIn, &r.TokenOut, &r.TokenInNative, &r.TokenOutNative, &r.AmountIn, &r.SizeBucket, &r.SlippageBps,
			&r.Taker, &r.StartedAt, &r.T0Block, &r.T0GasPrice, &r.T30Block, &r.T30GasPrice, &r.MethodologyVersion, &r.CollectorVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT aggregator, status, coalesce(error, ''), latency_ms, coalesce(quoted_out::text, ''),
			coalesce(tx_to, ''), coalesce(tx_data, ''), coalesce(tx_value::text, ''), coalesce(spender, ''), raw_response,
			coalesce(gas_limit, 0),
			t0_success, coalesce(t0_out::text, ''), coalesce(t0_gas, 0), coalesce(t0_revert, ''),
			t30_success, coalesce(t30_out::text, ''), coalesce(t30_gas, 0), coalesce(t30_revert, '')
		FROM quotes WHERE round_id = $1 ORDER BY aggregator`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var q api.Quote
		var t0, t30 api.Simulation
		var t0Ok, t30Ok *bool
		if err := rows.Scan(&q.Aggregator, &q.Status, &q.Error, &q.LatencyMs, &q.QuotedOut,
			&q.TxTo, &q.TxData, &q.TxValue, &q.Spender, &q.Raw, &q.GasLimit,
			&t0Ok, &t0.AmountOut, &t0.Gas, &t0.Revert, &t30Ok, &t30.AmountOut, &t30.Gas, &t30.Revert); err != nil {
			return nil, err
		}
		if t0Ok != nil {
			t0.Success, q.T0 = *t0Ok, &t0
		}
		if t30Ok != nil {
			t30.Success, q.T30 = *t30Ok, &t30
		}
		r.Quotes = append(r.Quotes, q)
	}
	return &r, rows.Err()
}

// Rounds lists up to limit rounds that started before before, newest first,
// on one chain or, with chainID 0, on all of them.
func (s *Store) Rounds(ctx context.Context, chainID uint64, before time.Time, limit int) ([]api.RoundRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id::text, r.chain_id, r.region, r.started_at, r.token_in, r.token_out,
			r.amount_in::text, r.size_bucket,
			(SELECT count(*) FROM quotes q WHERE q.round_id = r.id),
			(SELECT count(*) FROM quotes q WHERE q.round_id = r.id AND q.t0_success)
		FROM rounds r
		WHERE ($1::bigint = 0 OR r.chain_id = $1) AND r.started_at < $2 ORDER BY r.started_at DESC LIMIT $3`, int64(chainID), before, limit)
	if err != nil {
		return nil, err
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (api.RoundRef, error) {
		var ref api.RoundRef
		err := row.Scan(&ref.ID, &ref.ChainID, &ref.Region, &ref.StartedAt, &ref.TokenIn, &ref.TokenOut,
			&ref.AmountIn, &ref.SizeBucket, &ref.Quotes, &ref.SucceededT0)
		return ref, err
	})
	if refs == nil {
		refs = []api.RoundRef{}
	}
	return refs, err
}

// Pool is the connection pool, for package stats: its metrics view and queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
