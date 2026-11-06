// Package velora talks to the Velora (formerly ParaSwap) Market API
// (https://api.velora.xyz): one call returns the price route and the
// transaction for the Augustus v6.2 router.
package velora

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

// Adapter implements aggregator.Aggregator for Velora.
type Adapter struct{ up *upstream.Client }

// New returns the adapter. The free tier needs no key and allows 1 request per
// second and 5000 per day.
func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "velora" }

type swapResponse struct {
	PriceRoute struct {
		DestAmount         upstream.Num `json:"destAmount"`
		TokenTransferProxy string       `json:"tokenTransferProxy"` // the ERC-20 spender
	} `json:"priceRoute"`
	TxParams struct {
		To    string       `json:"to"`
		Data  string       `json:"data"`
		Value upstream.Num `json:"value"`
	} `json:"txParams"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	q := url.Values{
		// The native coin is 0xEeee…EEeE in exactly that casing; lower case is rejected.
		"srcToken":  {req.TokenIn},
		"destToken": {req.TokenOut},
		"amount":    {req.Amount},
		// Required: unknown tokens are refused without them, wrong ones misprice.
		"srcDecimals":  {strconv.Itoa(req.TokenInDecimals)},
		"destDecimals": {strconv.Itoa(req.TokenOutDecimals)},
		"side":         {"SELL"},
		"network":      {strconv.FormatUint(req.ChainID, 10)},
		"userAddress":  {strings.ToLower(req.Taker)},
		"slippage":     {strconv.Itoa(req.SlippageBps)}, // basis points
		"version":      {"6.2"},                         // the default is the legacy v5 router
	}
	if req.IntegratorID != "" {
		q.Set("partner", req.IntegratorID) // without one the request is booked on "anon", which charges 1 bp
	}
	body, err := a.up.Get(ctx, "/swap?"+q.Encode())
	if err != nil {
		return nil, mapError(err)
	}
	return parseQuote(body, aggregator.IsNative(req.TokenIn))
}

func parseQuote(body []byte, nativeIn bool) (*aggregator.Quote, error) {
	var r swapResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "velora sent an unreadable answer")
	}
	amountOut, ok := r.PriceRoute.DestAmount.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "velora sent no destAmount")
	}
	q := &aggregator.Quote{AmountOut: amountOut, Tx: aggregator.Tx{To: r.TxParams.To, Data: r.TxParams.Data, Value: "0"}, Raw: body}
	if v, ok := r.TxParams.Value.Uint(); ok {
		q.Tx.Value = v
	}
	if !nativeIn {
		q.Spender = r.PriceRoute.TokenTransferProxy
		if q.Spender == "" {
			q.Spender = q.Tx.To // v6.2: Augustus itself pulls the tokens
		}
	}
	return q, nil
}

// mapError turns a non-2xx answer ({"error": "..."}) into an *aggregator.Error.
// Transport and context errors pass through.
func mapError(err error) error {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(se.Body, &body)
	msg := strings.ToLower(body.Error)
	if se.Status == http.StatusNotFound || // "No routes found with enough liquidity", "Token not found. ..."
		strings.Contains(msg, "no route") || strings.Contains(msg, "max_impact") || strings.Contains(msg, "too small") || strings.Contains(msg, "too large") {
		return aggregator.NoRoute(body.Error)
	}
	return aggregator.HTTPError("velora", se, body.Error)
}
