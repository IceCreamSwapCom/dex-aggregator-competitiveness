package kyberswap

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// Real answers for 0.01 WETH -> USDC on Arbitrum (route shortened, calldata cut).
const (
	routeSummary  = `{"tokenIn":"0x82af49447d8a07e3bd95bd0d56f35241523fbab1","amountIn":"10000000000000000","tokenOut":"0xaf88d065e77c8cc2239327c5edb3a432268e5831","amountOut":"25345264","gas":"492835","checksum":"1234"}`
	routesFixture = `{"code":0,"message":"successfully","data":{"routeSummary":` + routeSummary + `,"routerAddress":"0x6131B5fae19EA4f9D964eAc0408E4408b66337b5"}}`
	buildFixture  = `{"code":0,"message":"successfully","data":{"amountIn":"10000000000000000","amountOut":"25345263","gas":"492835","data":"0xe21fd0e90000000000000000000000000000000000000000000000000000000000000020","routerAddress":"0x6131B5fae19EA4f9D964eAc0408E4408b66337b5","transactionValue":"0"}}`
)

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/arbitrum/api/v1/routes":
			aggtest.WantQuery(t, r, map[string]string{
				"tokenIn": aggtest.WETH, "tokenOut": aggtest.USDC, "amountIn": "10000000000000000",
				"gasPrice": "20000000", "origin": aggtest.Taker, "excludeRFQSources": "", // RFQ stays in
			})
			_, _ = w.Write([]byte(routesFixture))
		case "/arbitrum/api/v1/route/build":
			var body struct {
				RouteSummary              json.RawMessage
				Sender, Recipient, Source string
				SlippageTolerance         int
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if string(body.RouteSummary) != routeSummary || body.Sender != aggtest.Taker || body.Recipient != aggtest.Taker ||
				body.SlippageTolerance != 50 || body.Source != "dexbench" {
				t.Errorf("build body = %v", body)
			}
			_, _ = w.Write([]byte(buildFixture))
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	q, err := a.Quote(context.Background(), aggtest.Request)
	if err != nil {
		t.Fatal(err)
	}
	router := "0x6131B5fae19EA4f9D964eAc0408E4408b66337b5"
	if q.AmountOut != "25345263" || q.Tx.To != router || q.Spender != router || q.Tx.Value != "0" {
		t.Errorf("quote = %+v", q)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(q.Raw, &raw); err != nil || len(raw["route"]) == 0 || len(raw["build"]) == 0 {
		t.Errorf("raw = %s", q.Raw)
	}
}

func TestErrors(t *testing.T) {
	_, err := New(aggtest.Serve(t, aggtest.Answer(400, `{"code":4008,"message":"route not found"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	_, err = New(aggtest.Serve(t, aggtest.Answer(200, `{"code":4011,"message":"token not found"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	_, err = New(aggtest.Serve(t, aggtest.Answer(429, ``))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
	req := aggtest.Request
	req.ChainID = 7
	_, err = New(nil).Quote(context.Background(), req)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
}
