package velora

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// A real answer of GET /swap for 0.01 WETH -> USDC on Arbitrum (route and calldata cut).
const swapFixture = `{"priceRoute":{"blockNumber":513862895,"network":42161,"srcToken":"0x82af49447d8a07e3bd95bd0d56f35241523fbab1","srcDecimals":18,"srcAmount":"10000000000000000","destToken":"0xaf88d065e77c8cc2239327c5edb3a432268e5831","destDecimals":6,"destAmount":"25336957","bestRoute":[],"gasCost":"131096","side":"SELL","version":"6.2","contractAddress":"0x6a000f20005980200259b80c5102003040001068","tokenTransferProxy":"0x6a000f20005980200259b80c5102003040001068","partnerFee":0},"txParams":{"from":"0x1cec7ea5000000000000000000000000000051ab","to":"0x6a000f20005980200259b80c5102003040001068","value":"0","data":"0x876a02f60000000000000000000000000000000000000000000000000000000000000060","chainId":42161}}`

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swap" {
			t.Errorf("path = %s", r.URL.Path)
		}
		aggtest.WantQuery(t, r, map[string]string{
			"srcToken": aggtest.WETH, "destToken": aggtest.USDC, "amount": "10000000000000000", "srcDecimals": "18", "destDecimals": "6",
			"side": "SELL", "network": "42161", "userAddress": strings.ToLower(aggtest.Taker), "slippage": "50", "version": "6.2", "partner": "dexbench",
		})
		_, _ = w.Write([]byte(swapFixture))
	}))
	q, err := a.Quote(context.Background(), aggtest.Request)
	if err != nil {
		t.Fatal(err)
	}
	augustus := "0x6a000f20005980200259b80c5102003040001068"
	if q.AmountOut != "25336957" || q.Tx.To != augustus || q.Spender != augustus || q.Tx.Value != "0" {
		t.Errorf("quote = %+v", q)
	}
}

func TestErrors(t *testing.T) {
	for status, body := range map[int]string{404: `{"error":"No routes found with enough liquidity"}`, 400: `{"error":"ESTIMATED_LOSS_GREATER_THAN_MAX_IMPACT"}`} {
		_, err := New(aggtest.Serve(t, aggtest.Answer(status, body))).Quote(context.Background(), aggtest.Request)
		aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	}
	_, err := New(aggtest.Serve(t, aggtest.Answer(429, `{"error":"Rate limited"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
}
