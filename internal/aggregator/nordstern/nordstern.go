// Package nordstern talks to the Nordstern aggregator API
// (https://api.nordstern.finance). Nordstern is run by the operators of this
// benchmark; its adapter follows the same rules as every other one.
package nordstern

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// Adapter implements aggregator.Aggregator for Nordstern.
type Adapter struct{ up *upstream.Client }

// New returns the adapter. up must send a Referer header: without one the
// public API answers 429.
func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "nordstern" }

type quoteResponse struct {
	ToAmount upstream.Num `json:"toAmount"`
	Tx       struct {
		To    string       `json:"to"`
		Data  string       `json:"data"`
		Value upstream.Num `json:"value"`
	} `json:"tx"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	q := url.Values{
		"src":      {req.TokenIn}, // the native coin is 0xEeee…EEeE here too
		"dst":      {req.TokenOut},
		"amount":   {req.Amount},
		"from":     {req.Taker},
		"slippage": {strconv.FormatFloat(aggregator.SlippagePercent(req.SlippageBps), 'f', -1, 64)}, // percent
		"gasPrice": {req.GasPrice},                                                                  // wei
	}
	body, err := a.up.Get(ctx, "/aggregator/"+strconv.FormatUint(req.ChainID, 10)+"?"+q.Encode())
	if err != nil {
		return nil, mapError(err)
	}
	return parseQuote(body, aggregator.IsNative(req.TokenIn))
}

func parseQuote(body []byte, nativeIn bool) (*aggregator.Quote, error) {
	var r quoteResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "nordstern sent an unreadable answer")
	}
	amountOut, ok := r.ToAmount.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "nordstern sent no toAmount")
	}
	q := &aggregator.Quote{AmountOut: amountOut, Tx: aggregator.Tx{To: r.Tx.To, Data: r.Tx.Data, Value: "0"}, Raw: body}
	if v, ok := r.Tx.Value.Uint(); ok {
		q.Tx.Value = v
	}
	if !nativeIn {
		q.Spender = r.Tx.To // the router pulls the input token itself
	}
	return q, nil
}

// mapError turns a non-2xx answer into an *aggregator.Error. Transport and
// context errors pass through for the round to classify.
func mapError(err error) error {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(se.Body, &body) // 429 and gateway bodies are not JSON
	if se.Status == 422 || strings.Contains(strings.ToLower(body.Error), "no swap route") {
		return aggregator.NoRoute(body.Error)
	}
	return aggregator.HTTPError("nordstern", se, body.Error)
}
