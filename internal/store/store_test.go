package store

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/api"
)

// TestInsert needs a Postgres it may create tables in:
// TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres go test ./internal/store
func TestInsert(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, schema); err != nil { // a second start must not fail
		t.Fatalf("schema is not idempotent: %v", err)
	}

	big256 := new(big.Int).Lsh(big.NewInt(1), 255) // numeric(78) holds every uint256
	r := &Round{
		Region: "test", ChainID: 8453, TokenIn: "0xin", TokenOut: "0xout", TokenInNative: true, AmountIn: big256, SizeBucket: "1k",
		SlippageBps: 50, Taker: "0xtaker", StartedAt: time.Now(), T0Block: 100, T0GasPrice: big.NewInt(7),
		T30Block: 115, T30GasPrice: big.NewInt(8), MethodologyVersion: "0.1", CollectorVersion: "dev",
		Quotes: []Quote{
			{Aggregator: "a", Status: "ok", Latency: 1500 * time.Millisecond, QuotedOut: big.NewInt(1000), TxTo: "0xr",
				TxData: "0x12345678", TxValue: big.NewInt(0), Spender: "0xr", Raw: []byte(`{"amountOut":"1000"}`), GasLimit: 300000,
				T0:  &Simulation{Success: true, AmountOut: big.NewInt(999), Gas: 200000},
				T30: &Simulation{Success: false, Revert: "Error(Too little received)"}},
			{Aggregator: "b", Status: "no_route", Error: "no route found", Latency: time.Second},
		},
	}
	id, err := s.Insert(ctx, r)
	if err != nil {
		t.Fatal(err)
	}

	var amountIn, raw string
	var t0Out, t30Out *string
	var t30Success *bool
	if err := s.pool.QueryRow(ctx, `SELECT r.amount_in::text, q.raw_response->>'amountOut', q.t0_out::text, q.t30_out::text, q.t30_success
		FROM rounds r JOIN quotes q ON q.round_id = r.id WHERE r.id = $1 AND q.aggregator = 'a'`, id).
		Scan(&amountIn, &raw, &t0Out, &t30Out, &t30Success); err != nil {
		t.Fatal(err)
	}
	if amountIn != big256.String() || raw != "1000" || t0Out == nil || *t0Out != "999" || t30Out != nil || t30Success == nil || *t30Success {
		t.Errorf("read back amountIn=%s raw=%s t0Out=%v t30Out=%v t30Success=%v", amountIn, raw, t0Out, t30Out, t30Success)
	}
	var simulated *bool
	if err := s.pool.QueryRow(ctx, `SELECT t0_success FROM quotes WHERE round_id = $1 AND aggregator = 'b'`, id).Scan(&simulated); err != nil {
		t.Fatal(err)
	}
	if simulated != nil {
		t.Errorf("a quote without simulation has t0_success = %v, want NULL", *simulated)
	}

	// Read back in the public form.
	got, err := s.Round(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(r.StartedAt.Truncate(time.Microsecond)) {
		t.Errorf("startedAt %v, want %v", got.StartedAt, r.StartedAt)
	}
	want := &api.Round{
		ID: id, Region: "test", ChainID: 8453, TokenIn: "0xin", TokenOut: "0xout", TokenInNative: true, AmountIn: big256.String(), SizeBucket: "1k",
		SlippageBps: 50, Taker: "0xtaker", StartedAt: got.StartedAt, T0Block: 100, T0GasPrice: "7", T30Block: 115, T30GasPrice: "8",
		MethodologyVersion: "0.1", CollectorVersion: "dev",
		Quotes: []api.Quote{
			{Aggregator: "a", Status: "ok", LatencyMs: 1500, QuotedOut: "1000", TxTo: "0xr", TxData: "0x12345678", TxValue: "0",
				Spender: "0xr", Raw: json.RawMessage(`{"amountOut":"1000"}`), GasLimit: 300000,
				T0:  &api.Simulation{Success: true, AmountOut: "999", Gas: 200000},
				T30: &api.Simulation{Success: false, Revert: "Error(Too little received)"}},
			{Aggregator: "b", Status: "no_route", Error: "no route found", LatencyMs: 1000},
		},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	if _, err := s.Round(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing round: %v", err)
	}
	refs, err := s.Rounds(ctx, 8453, time.Now().Add(time.Second), 1)
	if err != nil || len(refs) != 1 || refs[0].ID != id || refs[0].ChainID != 8453 || refs[0].Region != "test" || refs[0].TokenIn != "0xin" ||
		refs[0].AmountIn != big256.String() || refs[0].Quotes != 2 || refs[0].SucceededT0 != 1 {
		t.Errorf("rounds = %+v, %v", refs, err)
	}
	if refs, err := s.Rounds(ctx, 1, time.Now(), 10); err != nil || refs == nil {
		t.Errorf("rounds of a chain without any = %v, %v", refs, err)
	}
}
