// Package sushi talks to the Sushi Swap API (https://api.sushi.com): one call
// returns a route through Sushi's RouteProcessor and the transaction for it.
package sushi

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

// Adapter implements aggregator.Aggregator for Sushi.
type Adapter struct{ up *upstream.Client }

func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "sushi" }

type swapResponse struct {
	Status           string       `json:"status"` // Success, Partial or NoWay
	AssumedAmountOut upstream.Num `json:"assumedAmountOut"`
	Tx               struct {
		To    string       `json:"to"`
		Data  string       `json:"data"`
		Value upstream.Num `json:"value"` // left out when zero
	} `json:"tx"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	taker := strings.ToLower(req.Taker)
	q := url.Values{
		"tokenIn":     {req.TokenIn}, // the native coin is 0xEeee…EEeE here too
		"tokenOut":    {req.TokenOut},
		"amount":      {req.Amount},
		"maxSlippage": {strconv.FormatFloat(aggregator.SlippageFraction(req.SlippageBps), 'f', -1, 64)}, // 0.005 = 50 bps
		"sender":      {taker},
		"recipient":   {taker},
	}
	body, err := a.up.Get(ctx, "/swap/v7/"+strconv.FormatUint(req.ChainID, 10)+"?"+q.Encode())
	if err != nil {
		return nil, mapError(err)
	}
	return parseQuote(body, aggregator.IsNative(req.TokenIn))
}

func parseQuote(body []byte, nativeIn bool) (*aggregator.Quote, error) {
	var r swapResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "sushi sent an unreadable answer")
	}
	switch r.Status {
	case "Success":
	case "NoWay":
		return nil, aggregator.Errorf(aggregator.CodeNoRoute, "sushi found no route")
	case "Partial":
		// Only part of the amount can be routed; the transaction would sell less than asked.
		return nil, aggregator.Errorf(aggregator.CodeNoRoute, "sushi can only route part of the amount")
	default:
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "sushi sent no route status")
	}
	amountOut, ok := r.AssumedAmountOut.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "sushi sent no assumedAmountOut")
	}
	q := &aggregator.Quote{AmountOut: amountOut, Tx: aggregator.Tx{To: r.Tx.To, Data: r.Tx.Data, Value: "0"}, Raw: body}
	if v, ok := r.Tx.Value.Uint(); ok {
		q.Tx.Value = v
	}
	if !nativeIn {
		q.Spender = r.Tx.To // the answer names no spender; the contract the transaction calls pulls the tokens
	}
	return q, nil
}

// mapError turns a non-2xx answer (RFC 7807 problem details) into an
// *aggregator.Error. Transport and context errors pass through.
func mapError(err error) error {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var problem struct {
		Title   string `json:"title"`
		Detail  string `json:"detail"`
		Details string `json:"details"` // some problems spell it this way
		Errors  []struct {
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(se.Body, &problem)
	msg := problem.Detail
	if msg == "" {
		msg = problem.Details
	}
	if len(problem.Errors) > 0 && problem.Errors[0].Detail != "" {
		msg = problem.Errors[0].Detail // "Token 0x… is invalid" says more than "The request is invalid."
	}
	if msg == "" {
		msg = problem.Title
	}
	// 422 is "cannot do this swap": unknown token, nothing to route.
	if se.Status == http.StatusUnprocessableEntity {
		return aggregator.NoRoute(msg)
	}
	return aggregator.HTTPError("sushi", se, msg)
}
