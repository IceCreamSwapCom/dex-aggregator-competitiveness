// Package enso talks to Enso's route API (https://api.enso.build): one call
// returns the best route and the transaction for Enso's router.
package enso

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

// Adapter implements aggregator.Aggregator for Enso.
type Adapter struct{ up *upstream.Client }

// New returns the adapter. up must send `Authorization: Bearer <key>`: there
// is no keyless tier.
func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "enso" }

type routeResponse struct {
	AmountOut upstream.Num `json:"amountOut"`
	Tx        struct {
		To    string       `json:"to"`
		Data  string       `json:"data"`
		Value upstream.Num `json:"value"`
	} `json:"tx"`
	// Approvals to send first; for an ERC-20 input one names the spender.
	PreTransactions []struct {
		Type    string `json:"type"`
		Spender string `json:"spender"`
	} `json:"preTransactions"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	q := url.Values{
		"chainId":         {strconv.FormatUint(req.ChainID, 10)},
		"fromAddress":     {strings.ToLower(req.Taker)},
		"tokenIn":         {strings.ToLower(req.TokenIn)}, // the native coin is 0xeeee…eeee
		"tokenOut":        {strings.ToLower(req.TokenOut)},
		"amountIn":        {req.Amount},
		"slippage":        {strconv.Itoa(req.SlippageBps)}, // basis points
		"routingStrategy": {"router"},                      // a plain transaction to Enso's router, approved like any spender
	}
	if req.IntegratorID != "" {
		q.Set("referralCode", req.IntegratorID) // at most 16 characters, emitted on chain
	}
	body, err := a.up.Get(ctx, "/api/v1/shortcuts/route?"+q.Encode())
	if err != nil {
		return nil, mapError(err)
	}
	return parseQuote(body, aggregator.IsNative(req.TokenIn))
}

func parseQuote(body []byte, nativeIn bool) (*aggregator.Quote, error) {
	var r routeResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "enso sent an unreadable answer")
	}
	amountOut, ok := r.AmountOut.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "enso sent no amountOut")
	}
	q := &aggregator.Quote{AmountOut: amountOut, Tx: aggregator.Tx{To: r.Tx.To, Data: r.Tx.Data, Value: "0"}, Raw: body}
	if v, ok := r.Tx.Value.Uint(); ok {
		q.Tx.Value = v
	}
	if !nativeIn {
		q.Spender = q.Tx.To // the router pulls the tokens
		for _, p := range r.PreTransactions {
			if p.Type == "tokenApproval" && p.Spender != "" {
				q.Spender = p.Spender
			}
		}
	}
	return q, nil
}

// mapError turns a non-2xx answer into an *aggregator.Error. Enso does not
// document its error answers; its message comes as a string or, for invalid
// parameters, a list of strings. Transport and context errors pass through.
func mapError(err error) error {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Message json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(se.Body, &body)
	var msg string
	if json.Unmarshal(body.Message, &msg) != nil {
		var list []string
		_ = json.Unmarshal(body.Message, &list)
		msg = strings.Join(list, "; ")
	}
	low := strings.ToLower(msg)
	if se.Status == http.StatusNotFound || strings.Contains(low, "route") || strings.Contains(low, "liquidity") {
		return aggregator.NoRoute(msg)
	}
	return aggregator.HTTPError("enso", se, msg)
}
