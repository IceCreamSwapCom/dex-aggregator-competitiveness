package enso

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// An answer of GET /api/v1/shortcuts/route in the shape Enso's API reference
// documents (RouteShortcutTransaction). It is not a recorded answer: replace
// it with one once the benchmark has an Enso key.
const routeFixture = `{"gas":"312000","amountOut":"23084475","priceImpact":2,"feeAmount":["0"],"minAmountOut":"22969053","createdAt":514507129,"tx":{"data":"0xb35d7e73000000000000000000000000","to":"0xf75584ef6673ad213a685a1b58cc0330b8ea22cf","from":"0xd8da6bf26964af9d7eed9e03e53415d37aa96045","value":"0"},"route":[],"ensoFeeAmount":["0"],"metadata":{"bridgeRefundAssets":[]},"preTransactions":[{"type":"tokenApproval","token":"0x82af49447d8a07e3bd95bd0d56f35241523fbab1","spender":"0xf75584ef6673ad213a685a1b58cc0330b8ea22cf","amount":"10000000000000000","tx":{"data":"0x095ea7b3","to":"0x82af49447d8a07e3bd95bd0d56f35241523fbab1","from":"0xd8da6bf26964af9d7eed9e03e53415d37aa96045","value":"0"}}]}`

const router = "0xf75584ef6673ad213a685a1b58cc0330b8ea22cf"

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/shortcuts/route" {
			t.Errorf("path = %s", r.URL.Path)
		}
		aggtest.WantQuery(t, r, map[string]string{
			"chainId": "42161", "fromAddress": strings.ToLower(aggtest.Taker), "tokenIn": aggtest.WETH, "tokenOut": strings.ToLower(aggtest.USDC),
			"amountIn": "10000000000000000", "slippage": "50", "routingStrategy": "router", "referralCode": "dexbench",
		})
		_, _ = w.Write([]byte(routeFixture))
	}))
	q, err := a.Quote(context.Background(), aggtest.Request)
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountOut != "23084475" || q.Tx.To != router || q.Spender != router || q.Tx.Value != "0" || len(q.Raw) == 0 {
		t.Errorf("quote = %+v", q)
	}
	if err := q.Validate(aggtest.Request); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// Without an approval to send first, the router is the spender.
	bare := strings.Replace(routeFixture, `"type":"tokenApproval"`, `"type":"requiredApproval"`, 1)
	if q, err := parseQuote([]byte(bare), false); err != nil || q.Spender != router {
		t.Errorf("quote = %+v, %v", q, err)
	}
	// Selling the native coin: nothing to approve.
	if q, err := parseQuote([]byte(routeFixture), true); err != nil || q.Spender != "" {
		t.Errorf("native quote = %+v, %v", q, err)
	}
}

func TestErrors(t *testing.T) {
	for _, body := range []string{
		`{"statusCode":400,"message":"Could not find a route","error":"Bad Request"}`,
		`{"statusCode":400,"message":["No route found with sufficient liquidity"]}`,
	} {
		_, err := New(aggtest.Serve(t, aggtest.Answer(400, body))).Quote(context.Background(), aggtest.Request)
		aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	}
	_, err := New(aggtest.Serve(t, aggtest.Answer(401, `{"statusCode":401,"message":"Unauthorized"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
	_, err = New(aggtest.Serve(t, aggtest.Answer(400, `{"statusCode":400,"message":["amountIn must be a number string"]}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
}
