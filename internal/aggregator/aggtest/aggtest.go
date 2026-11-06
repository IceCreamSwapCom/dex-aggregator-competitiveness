// Package aggtest has what the adapter tests share: a request and a fake API.
package aggtest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

const (
	WETH  = "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"
	USDC  = "0xaf88d065e77c8cC2239327C5EDb3A432268e5831"
	Taker = "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"
)

// Request is 0.01 WETH -> USDC on Arbitrum, the trade the fixtures were cut from.
var Request = aggregator.Request{
	ChainID: 42161, TokenIn: WETH, TokenOut: USDC, TokenInDecimals: 18, TokenOutDecimals: 6,
	Amount: "10000000000000000", Taker: Taker, SlippageBps: 50, GasPrice: "20000000", IntegratorID: "dexbench",
}

// Serve starts a fake API and returns a client for it.
func Serve(t *testing.T, h http.HandlerFunc) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return upstream.New(srv.Client(), srv.URL, nil)
}

// Answer is a handler that always answers status with body.
func Answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// WantCode fails the test unless err is an *aggregator.Error with code.
func WantCode(t *testing.T, err error, code aggregator.Code) {
	t.Helper()
	var ae *aggregator.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Errorf("error = %v, want %s", err, code)
	}
}

// WantQuery fails the test unless the request carried every parameter in want.
func WantQuery(t *testing.T, r *http.Request, want map[string]string) {
	t.Helper()
	q := r.URL.Query()
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), v)
		}
	}
}
