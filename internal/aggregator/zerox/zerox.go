// Package zerox talks to the 0x Swap API v2 (https://api.0x.org), in its
// AllowanceHolder variant: one call returns the price and a transaction that
// needs nothing but an ordinary ERC-20 allowance. (The Permit2 variant wants a
// signature appended to the calldata, which the benchmark does not use.)
package zerox

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// Adapter implements aggregator.Aggregator for 0x.
type Adapter struct{ up *upstream.Client }

// New returns the adapter. up must send the two headers every call needs:
// `0x-api-key` (there is no keyless tier) and `0x-version: v2`.
func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "zerox" }

type quoteResponse struct {
	// False (with nothing else in the answer) when 0x has no route.
	LiquidityAvailable *bool        `json:"liquidityAvailable"`
	BuyAmount          upstream.Num `json:"buyAmount"`
	AllowanceTarget    string       `json:"allowanceTarget"`
	Issues             struct {
		// Set when the taker's allowance does not cover the sale.
		Allowance *struct {
			Spender string `json:"spender"`
		} `json:"allowance"`
	} `json:"issues"`
	Transaction struct {
		To    string       `json:"to"`
		Data  string       `json:"data"`
		Value upstream.Num `json:"value"`
	} `json:"transaction"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	q := url.Values{
		"chainId":     {strconv.FormatUint(req.ChainID, 10)},
		"sellToken":   {req.TokenIn}, // the native coin is 0xEeee…EEeE here too
		"buyToken":    {req.TokenOut},
		"sellAmount":  {req.Amount},
		"taker":       {req.Taker},
		"slippageBps": {strconv.Itoa(req.SlippageBps)},
		"gasPrice":    {req.GasPrice}, // wei: the price the route is optimised for
	}
	body, err := a.up.Get(ctx, "/swap/allowance-holder/quote?"+q.Encode())
	if err != nil {
		return nil, mapError(err)
	}
	return parseQuote(body, aggregator.IsNative(req.TokenIn))
}

func parseQuote(body []byte, nativeIn bool) (*aggregator.Quote, error) {
	var r quoteResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "0x sent an unreadable answer")
	}
	if r.LiquidityAvailable != nil && !*r.LiquidityAvailable {
		return nil, aggregator.Errorf(aggregator.CodeNoRoute, "0x has no liquidity for this swap")
	}
	amountOut, ok := r.BuyAmount.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "0x sent no buyAmount")
	}
	q := &aggregator.Quote{AmountOut: amountOut, Tx: aggregator.Tx{To: r.Transaction.To, Data: r.Transaction.Data, Value: "0"}, Raw: body}
	if v, ok := r.Transaction.Value.Uint(); ok {
		q.Tx.Value = v
	}
	if !nativeIn {
		// The AllowanceHolder contract pulls the tokens. The answer names it
		// twice; `issues.allowance` only when the allowance is missing.
		q.Spender = r.AllowanceTarget
		if q.Spender == "" && r.Issues.Allowance != nil {
			q.Spender = r.Issues.Allowance.Spender
		}
	}
	return q, nil
}

// noRoute are the error names that mean "0x cannot do this swap", as opposed
// to something being wrong with the service or with our request.
var noRoute = map[string]bool{
	"TOKEN_NOT_SUPPORTED":                 true,
	"BUY_TOKEN_NOT_AUTHORIZED_FOR_TRADE":  true,
	"SELL_TOKEN_NOT_AUTHORIZED_FOR_TRADE": true,
	"SELL_AMOUNT_TOO_SMALL":               true,
	"INSUFFICIENT_LIQUIDITY":              true,
}

// mapError turns a non-2xx answer ({"name","message","data":{"details":[…]}})
// into an *aggregator.Error. Transport and context errors pass through.
func mapError(err error) error {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var problem struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"data"`
	}
	_ = json.Unmarshal(se.Body, &problem)
	msg := problem.Message
	if len(problem.Data.Details) > 0 && problem.Data.Details[0].Reason != "" {
		msg = problem.Data.Details[0].Reason // says which input, and why
	}
	if noRoute[problem.Name] {
		return aggregator.NoRoute(msg)
	}
	return aggregator.HTTPError("0x", se, msg)
}
