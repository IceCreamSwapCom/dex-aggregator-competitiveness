// Package lifi talks to LI.FI (https://li.quest/v1): for a same-chain swap it
// picks the best of the DEX aggregators it integrates and returns one
// transaction for its diamond contract.
//
// Without an API key LI.FI allows 75 quotes per two hours per IP and takes a
// 0.25% fee, so the benchmark uses a key.
package lifi

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

// LI.FI error codes (https://docs.li.fi, error codes).
const (
	codeNoQuote      = 1002
	codeRateLimit    = 1005
	codeUnauthorized = 1010
)

// Adapter implements aggregator.Aggregator for LI.FI.
type Adapter struct{ up *upstream.Client }

// New returns the adapter. up must send `x-lifi-api-key`.
func New(up *upstream.Client) *Adapter { return &Adapter{up: up} }

func (a *Adapter) ID() string { return "lifi" }

type quoteResponse struct {
	Estimate struct {
		ToAmount        upstream.Num `json:"toAmount"`
		ApprovalAddress string       `json:"approvalAddress"`
	} `json:"estimate"`
	TransactionRequest struct {
		To    string       `json:"to"`
		Data  string       `json:"data"`
		Value upstream.Num `json:"value"` // hex
	} `json:"transactionRequest"`
}

func (a *Adapter) Quote(ctx context.Context, req aggregator.Request) (*aggregator.Quote, error) {
	chain := strconv.FormatUint(req.ChainID, 10)
	taker := strings.ToLower(req.Taker)
	q := url.Values{
		"fromChain": {chain},
		"toChain":   {chain},
		// The native coin is the zero address on the EVM chains LI.FI lists (Celo is an exception).
		"fromToken":   {aggregator.ZeroNative(req.TokenIn)},
		"toToken":     {aggregator.ZeroNative(req.TokenOut)},
		"fromAmount":  {req.Amount},
		"fromAddress": {taker},
		"toAddress":   {taker},
		"slippage":    {strconv.FormatFloat(aggregator.SlippageFraction(req.SlippageBps), 'f', -1, 64)}, // 0.005 = 50 bp
	}
	if req.IntegratorID != "" {
		q.Set("integrator", req.IntegratorID)
	}
	body, err := a.up.Get(ctx, "/v1/quote?"+q.Encode())
	if err != nil {
		return nil, mapError(err)
	}
	return parseQuote(body, aggregator.IsNative(req.TokenIn))
}

func parseQuote(body []byte, nativeIn bool) (*aggregator.Quote, error) {
	var r quoteResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "lifi sent an unreadable answer")
	}
	amountOut, ok := r.Estimate.ToAmount.Uint()
	if !ok {
		return nil, aggregator.Errorf(aggregator.CodeInvalid, "lifi sent no toAmount")
	}
	q := &aggregator.Quote{
		AmountOut: amountOut,
		Tx:        aggregator.Tx{To: r.TransactionRequest.To, Data: r.TransactionRequest.Data, Value: "0"},
		Raw:       body,
	}
	if v, ok := r.TransactionRequest.Value.Uint(); ok {
		q.Tx.Value = v
	}
	if !nativeIn {
		q.Spender = r.Estimate.ApprovalAddress
		if q.Spender == "" {
			q.Spender = q.Tx.To
		}
	}
	return q, nil
}

// mapError turns a non-2xx answer ({"message","code"}) into an
// *aggregator.Error. Transport and context errors pass through.
func mapError(err error) error {
	var se *upstream.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	_ = json.Unmarshal(se.Body, &body)
	// LI.FI also reports these two in the body, whatever the HTTP status.
	switch body.Code {
	case codeRateLimit:
		se.Status = http.StatusTooManyRequests
	case codeUnauthorized:
		se.Status = http.StatusUnauthorized
	}
	if se.Status == http.StatusNotFound || body.Code == codeNoQuote {
		return aggregator.NoRoute(body.Message)
	}
	return aggregator.HTTPError("lifi", se, body.Message)
}
