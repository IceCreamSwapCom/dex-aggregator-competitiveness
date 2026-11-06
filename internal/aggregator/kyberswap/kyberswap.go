// Package kyberswap talks to the KyberSwap Aggregator API
// (https://aggregator-api.kyberswap.com). A quote takes two calls: find a
// route, then have it encoded for the taker.
package kyberswap

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// maxSlippageBps is the most /route/build accepts without ignoreCappedSlippage.
const maxSlippageBps = 2000

// slugs: the API addresses chains by name and has no endpoint that lists them
// (docs: "Supported chains", read 2026-10-11).
var slugs = map[uint64]string{
	1: "ethereum", 10: "optimism", 56: "bsc", 130: "unichain", 137: "polygon", 143: "monad", 146: "sonic",
	999: "hyperevm", 2020: "ronin", 4326: "megaeth", 4663: "robinhood", 5000: "mantle", 5042: "arc",
	8453: "base", 9745: "plasma", 42161: "arbitrum", 42793: "etherlink", 43114: "avalanche", 59144: "linea",
	80094: "berachain",
}

// Kyber's error codes that mean "not this pair, not this size".
var noRoute = map[int]bool{
	4008: true, // route not found
	4009: true, // amountIn is greater than max allowed
	4010: true, // no pool is eligible to find route on
	4011: true, // tokenIn / tokenOut can not be found
}

// Adapter implements aggregator.Aggregator for KyberSwap.
type Adapter struct{ up *upstream.Client }

// New returns the adapter. up sends the `x-client-id` header (the integrator
// id): without it the API applies stricter rate limits.
func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "kyberswap" }

// envelope wraps every answer, also the failed ones.
type envelope struct {
	Code    int             `json:"code"` // 0 = success
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type routeData struct {
	// RouteSummary goes back to /route/build byte for byte: it is checksummed.
	RouteSummary json.RawMessage `json:"routeSummary"`
}

type buildRequest struct {
	RouteSummary      json.RawMessage `json:"routeSummary"`
	Sender            string          `json:"sender"`
	Recipient         string          `json:"recipient"`
	Origin            string          `json:"origin"`
	SlippageTolerance int             `json:"slippageTolerance"` // basis points
	Source            string          `json:"source,omitempty"`
}

type buildData struct {
	AmountOut        upstream.Num `json:"amountOut"`
	Data             string       `json:"data"`
	RouterAddress    string       `json:"routerAddress"`
	TransactionValue upstream.Num `json:"transactionValue"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	slug, ok := slugs[req.ChainID]
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeNoRoute, "kyberswap does not serve chain %d", req.ChainID)
	}

	// 1. The route, from every source the API uses by default (RFQ included).
	q := url.Values{
		"tokenIn":  {req.TokenIn}, // the native coin is 0xEeee…EEeE here too
		"tokenOut": {req.TokenOut},
		"amountIn": {req.Amount},
		"gasPrice": {req.GasPrice}, // wei
		"origin":   {req.Taker},
	}
	var route routeData
	routeBody, err := a.call(func() ([]byte, error) { return a.up.Get(ctx, "/"+slug+"/api/v1/routes?"+q.Encode()) }, &route)
	if err != nil {
		return nil, err
	}
	if len(route.RouteSummary) == 0 {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "kyberswap sent no route summary")
	}

	// 2. The transaction for this taker.
	var built buildData
	buildBody, err := a.call(func() ([]byte, error) {
		return a.up.PostJSON(ctx, "/"+slug+"/api/v1/route/build", buildRequest{
			RouteSummary: route.RouteSummary, Sender: req.Taker, Recipient: req.Taker, Origin: req.Taker,
			SlippageTolerance: min(req.SlippageBps, maxSlippageBps), Source: req.IntegratorID,
		})
	}, &built)
	if err != nil {
		return nil, err
	}

	amountOut, ok := built.AmountOut.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "kyberswap sent no amountOut")
	}
	raw, _ := json.Marshal(map[string]json.RawMessage{"route": routeBody, "build": buildBody})
	out := &aggregator.Quote{
		AmountOut: amountOut,
		Tx:        aggregator.Tx{To: built.RouterAddress, Data: built.Data, Value: "0"},
		Raw:       raw,
	}
	if v, ok := built.TransactionValue.Uint(); ok {
		out.Tx.Value = v
	}
	if !aggregator.IsNative(req.TokenIn) {
		out.Spender = built.RouterAddress // per the docs: approve the router the response names
	}
	return out, nil
}

// call runs one API call, unwraps its envelope into out and returns the body.
func (a *Adapter) call(do func() ([]byte, error), out any) ([]byte, error) {
	body, err := do()
	if err != nil {
		var se *upstream.StatusError
		if !errors.As(err, &se) {
			return nil, err // transport or context: the round classifies it
		}
		var env envelope
		_ = json.Unmarshal(se.Body, &env)
		if noRoute[env.Code] {
			return nil, aggregator.NoRoute(env.Message)
		}
		return nil, aggregator.HTTPError("kyberswap", se, env.Message)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "kyberswap sent an unreadable answer")
	}
	switch {
	case noRoute[env.Code]:
		return nil, aggregator.NoRoute(env.Message)
	case env.Code != 0:
		return nil, aggregator.Errorf(aggregator.CodeError, "kyberswap answered code %d: %s", env.Code, env.Message)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "kyberswap sent an unreadable answer")
	}
	return body, nil
}
