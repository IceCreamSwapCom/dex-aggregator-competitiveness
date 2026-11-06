package sushi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// A real answer of GET /swap/v7/42161 for 0.01 WETH -> USDC (calldata cut). There is no
// tx.value: it is left out when zero.
const swapFixture = `{"status":"Success","amountIn":"10000000000000000","assumedAmountOut":"25296070","gasSpent":105000,"tx":{"from":"0x1cec7ea5000000000000000000000000000051ab","to":"0xac4c6e212a361c968f1725b4d055b47e63f80b75","gasPrice":20030000,"data":"0x5f3bd1c800000000000000000000000082af49447d8a07e3bd95bd0d56f35241523fbab1"}}`

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swap/v7/42161" {
			t.Errorf("path = %s", r.URL.Path)
		}
		taker := strings.ToLower(aggtest.Taker)
		aggtest.WantQuery(t, r, map[string]string{
			"tokenIn": aggtest.WETH, "tokenOut": aggtest.USDC, "amount": "10000000000000000", "maxSlippage": "0.005",
			"sender": taker, "recipient": taker,
		})
		_, _ = w.Write([]byte(swapFixture))
	}))
	q, err := a.Quote(context.Background(), aggtest.Request)
	if err != nil {
		t.Fatal(err)
	}
	processor := "0xac4c6e212a361c968f1725b4d055b47e63f80b75"
	if q.AmountOut != "25296070" || q.Tx.To != processor || q.Spender != processor || q.Tx.Value != "0" {
		t.Errorf("quote = %+v", q)
	}
}

func TestErrors(t *testing.T) {
	for _, status := range []string{"NoWay", "Partial"} {
		body := strings.Replace(swapFixture, `"Success"`, `"`+status+`"`, 1)
		_, err := New(aggtest.Serve(t, aggtest.Answer(200, body))).Quote(context.Background(), aggtest.Request)
		aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	}
	_, err := New(aggtest.Serve(t, aggtest.Answer(422, `{"title":"Unprocessable","detail":"Token 0x… is invalid"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	_, err = New(aggtest.Serve(t, aggtest.Answer(500, `{"title":"Internal Server Error"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
}
