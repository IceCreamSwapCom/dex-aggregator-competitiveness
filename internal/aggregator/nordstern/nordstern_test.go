package nordstern

import (
	"context"
	"net/http"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// A real answer for 0.01 WETH -> USDC on Arbitrum (calldata cut).
const quoteFixture = `{"src":"0x82af49447d8a07e3bd95bd0d56f35241523fbab1","dst":"0xaf88d065e77c8cC2239327C5EDb3A432268e5831",
"fromAmount":"10000000000000000","toAmount":"24997558","minToAmount":"24872105","gasEstimate":"78312","simulationBlock":513804172,
"tx":{"from":"0x1CEc7EA5000000000000000000000000000051AB","to":"0x57f96440f1b1cAD53B40A8924BD540b1279A491c","data":"0x3f0bde256451e0f8a603cfb72e084b658eb7fa65","value":"0"}}`

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aggregator/42161" {
			t.Errorf("path = %s", r.URL.Path)
		}
		aggtest.WantQuery(t, r, map[string]string{
			"src": aggtest.WETH, "dst": aggtest.USDC, "amount": "10000000000000000", "from": aggtest.Taker,
			"slippage": "0.5", "gasPrice": "20000000",
		})
		_, _ = w.Write([]byte(quoteFixture))
	}))
	q, err := a.Quote(context.Background(), aggtest.Request)
	if err != nil {
		t.Fatal(err)
	}
	router := "0x57f96440f1b1cAD53B40A8924BD540b1279A491c"
	if q.AmountOut != "24997558" || q.Tx.To != router || q.Spender != router || q.Tx.Value != "0" || len(q.Raw) == 0 {
		t.Errorf("quote = %+v", q)
	}
	if err := q.Validate(aggtest.Request); err != nil {
		t.Error(err)
	}
}

func TestErrors(t *testing.T) {
	for status, body := range map[int]string{422: `{"error":"no swap route found"}`, 400: `{"error":"No swap route found for this pair"}`} {
		_, err := New(aggtest.Serve(t, aggtest.Answer(status, body))).Quote(context.Background(), aggtest.Request)
		aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	}
	_, err := New(aggtest.Serve(t, aggtest.Answer(429, `rate limited`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
	_, err = New(aggtest.Serve(t, aggtest.Answer(200, `{"toAmount":null}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeInvalid)
}
