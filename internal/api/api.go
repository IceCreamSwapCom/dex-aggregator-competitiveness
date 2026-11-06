// Package api is the public JSON of the round API, GET /api/v1/rounds and
// /api/v1/rounds/{id}. A round in this form is everything resim needs to
// re-simulate it. Amounts and wei values are decimal strings: most JSON readers
// lose precision above 2^53. Addresses are lower-case 0x hex.
package api

import (
	"encoding/json"
	"time"
)

// Round is one stored round.
type Round struct {
	ID                 string    `json:"id"`
	Region             string    `json:"region"`
	ChainID            uint64    `json:"chainId"`
	TokenIn            string    `json:"tokenIn"`
	TokenOut           string    `json:"tokenOut"`
	TokenInNative      bool      `json:"tokenInNative"` // the native coin or its wrapped token
	TokenOutNative     bool      `json:"tokenOutNative"`
	AmountIn           string    `json:"amountIn"`
	SizeBucket         string    `json:"sizeBucket"`
	SlippageBps        int       `json:"slippageBps"`
	Taker              string    `json:"taker"`
	StartedAt          time.Time `json:"startedAt"`
	T0Block            uint64    `json:"t0Block"`
	T0GasPrice         string    `json:"t0GasPrice"`
	T30Block           uint64    `json:"t30Block"`
	T30GasPrice        string    `json:"t30GasPrice"`
	MethodologyVersion string    `json:"methodologyVersion"`
	CollectorVersion   string    `json:"collectorVersion"`
	Quotes             []Quote   `json:"quotes"`
}

// Quote is one aggregator's answer in a round. The transaction fields are set
// when the aggregator answered with one, the simulations when it was simulated.
type Quote struct {
	Aggregator string          `json:"aggregator"`
	Status     string          `json:"status"` // ok | no_route | timeout | error | invalid
	Error      string          `json:"error,omitempty"`
	LatencyMs  int64           `json:"latencyMs"`
	QuotedOut  string          `json:"quotedOut,omitempty"`
	TxTo       string          `json:"txTo,omitempty"`
	TxData     string          `json:"txData,omitempty"`
	TxValue    string          `json:"txValue,omitempty"`
	Spender    string          `json:"spender,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"` // the aggregator's answer as it came
	GasLimit   uint64          `json:"gasLimit,omitempty"`
	T0         *Simulation     `json:"t0,omitempty"`
	T30        *Simulation     `json:"t30,omitempty"`
}

// Simulation is the outcome of one measured run.
type Simulation struct {
	Success   bool   `json:"success"`
	AmountOut string `json:"amountOut,omitempty"`
	Gas       uint64 `json:"gas,omitempty"` // as a transaction of its own
	Revert    string `json:"revert,omitempty"`
}

// RoundRef is a round in a listing.
type RoundRef struct {
	ID          string    `json:"id"`
	ChainID     uint64    `json:"chainId"`
	Region      string    `json:"region"`
	StartedAt   time.Time `json:"startedAt"`
	TokenIn     string    `json:"tokenIn"`
	TokenOut    string    `json:"tokenOut"`
	AmountIn    string    `json:"amountIn"`
	SizeBucket  string    `json:"sizeBucket"`
	Quotes      int       `json:"quotes"`      // aggregators asked
	SucceededT0 int       `json:"succeededT0"` // of them, quotes that succeeded at T0
}

// LeaderboardRow is one aggregator's metrics over the rounds a leaderboard
// covers (GET /api/v1/leaderboard). Shares are fractions (0.25 = 25%).
// A metric without any round to compute it from is null.
type LeaderboardRow struct {
	Aggregator   string   `json:"aggregator"`
	Requests     int64    `json:"requests"`     // rounds it was asked in
	RankedRounds int64    `json:"rankedRounds"` // of those, rounds that rank
	Win          *float64 `json:"win"`
	Within1Bp    *float64 `json:"within1bp"`
	Within5Bp    *float64 `json:"within5bp"`
	AccuracyT0   *float64 `json:"accuracyT0Bps"`  // mean of min((simulated − quoted) / quoted, 0)
	AccuracyT30  *float64 `json:"accuracyT30Bps"` // the same 30 s later
	Reverts      *float64 `json:"reverts"`        // transactions that reverted at T0 or T30, of all it returned
	LatencyP50Ms float64  `json:"latencyP50Ms"`
	LatencyP90Ms float64  `json:"latencyP90Ms"`
	Availability float64  `json:"availability"` // requests that it answered with a transaction
}

// Leaderboard is GET /api/v1/leaderboard, of all chains unless chain= names
// one: the rounds it covers, and one row per aggregator, best win rate first.
type Leaderboard struct {
	ChainID     uint64           `json:"chainId"` // 0: all chains together
	Period      string           `json:"period"`  // 24h | 7d | 30d
	Size        string           `json:"size,omitempty"`
	Region      string           `json:"region,omitempty"`
	Since       time.Time        `json:"since"`
	RefreshedAt time.Time        `json:"refreshedAt"` // when the metrics were last recomputed
	Rows        []LeaderboardRow `json:"rows"`
}
