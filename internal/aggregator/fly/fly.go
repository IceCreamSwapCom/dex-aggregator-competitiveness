// Package fly talks to the Fly (formerly Magpie) aggregator API
// (https://api.fly.trade). Its combined endpoint returns quote and
// transaction in one call.
package fly

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// networks: the API addresses chains by name, as GET /token-manager/networks
// lists them (read 2026-10-11).
var networks = map[uint64]string{1: "ethereum", 999: "hyperevm", 4663: "outlaw", 8453: "base", 42161: "arbitrum"}

// Adapter implements aggregator.Aggregator for Fly.
type Adapter struct{ up *upstream.Client }

func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "fly" }

type quoteResponse struct {
	Quote struct {
		AmountOut     upstream.Num `json:"amountOut"`
		TargetAddress string       `json:"targetAddress"` // the router: what needs the allowance
	} `json:"quote"`
	Transaction struct {
		To    string       `json:"to"`
		Data  string       `json:"data"`
		Value upstream.Num `json:"value"`
	} `json:"transaction"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	network, ok := networks[req.ChainID]
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeNoRoute, "fly does not serve chain %d", req.ChainID)
	}
	taker := strings.ToLower(req.Taker)
	q := url.Values{
		"network":          {network},
		"fromTokenAddress": {aggregator.ZeroNative(req.TokenIn)}, // the native coin is the zero address
		"toTokenAddress":   {aggregator.ZeroNative(req.TokenOut)},
		"sellAmount":       {req.Amount},
		"slippage":         {strconv.FormatFloat(aggregator.SlippageFraction(req.SlippageBps), 'f', -1, 64)},
		"fromAddress":      {taker},
		"toAddress":        {taker},
		"gasless":          {"false"}, // anything else fails
	}
	// One call for quote and transaction; it skips Fly's own gas estimation,
	// which would fail for a taker without balance and allowance.
	body, err := a.up.Get(ctx, "/aggregator/quote/transaction?"+q.Encode())
	if err != nil {
		return nil, mapError(err)
	}
	return parseQuote(body, aggregator.IsNative(req.TokenIn))
}

func parseQuote(body []byte, nativeIn bool) (*aggregator.Quote, error) {
	var r quoteResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "fly sent an unreadable answer")
	}
	amountOut, ok := r.Quote.AmountOut.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "fly sent no amountOut")
	}
	q := &aggregator.Quote{AmountOut: amountOut, Tx: aggregator.Tx{To: r.Transaction.To, Data: r.Transaction.Data, Value: "0"}, Raw: body}
	if v, ok := r.Transaction.Value.Uint(); ok {
		q.Tx.Value = v
	}
	if !nativeIn {
		q.Spender = r.Quote.TargetAddress
		if q.Spender == "" {
			q.Spender = q.Tx.To
		}
	}
	return q, nil
}

// mapError turns a non-2xx answer ({"message","code","id"}) into an
// *aggregator.Error. Transport and context errors pass through.
func mapError(err error) error {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(se.Body, &body)
	msg := strings.ToLower(body.Message)
	// 404 is how Fly says it cannot quote: "Couldn't recognize to token", no path.
	if se.Status == http.StatusNotFound || strings.Contains(msg, "no route") || strings.Contains(msg, "liquidity") {
		return aggregator.NoRoute(body.Message)
	}
	return aggregator.HTTPError("fly", se, body.Message)
}
