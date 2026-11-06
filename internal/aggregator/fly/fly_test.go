package fly

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// A real answer selling 0.01 ETH for USDC on Arbitrum (typed data and calldata cut).
const quoteFixture = `{"quote":{"id":"b4becf08-f68e-43ab-b715-897ba9803734","amountOut":"25301318","targetAddress":"0x20f6ee51340adeed01a59b0e65cb3703f3dc860c","resourceEstimate":{"gasLimit":"416740"}},"transaction":{"from":"0x1cec7ea5000000000000000000000000000051ab","to":"0x20f6ee51340adeed01a59b0e65cb3703f3dc860c","data":"0x46ec278a0000000000000000000000000000000000000000000000000000000000000020","chainId":42161,"type":2,"gasLimit":"0","value":"10000000000000000"}}`

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aggregator/quote/transaction" {
			t.Errorf("path = %s", r.URL.Path)
		}
		taker := strings.ToLower(aggtest.Taker)
		aggtest.WantQuery(t, r, map[string]string{
			"network": "arbitrum", "fromTokenAddress": aggregator.ZeroAddress, "toTokenAddress": strings.ToLower(aggtest.USDC),
			"sellAmount": "10000000000000000", "slippage": "0.005", "fromAddress": taker, "toAddress": taker, "gasless": "false",
		})
		_, _ = w.Write([]byte(quoteFixture))
	}))
	req := aggtest.Request
	req.TokenIn = aggregator.NativeToken
	q, err := a.Quote(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountOut != "25301318" || q.Tx.To != "0x20f6ee51340adeed01a59b0e65cb3703f3dc860c" || q.Tx.Value != "10000000000000000" || q.Spender != "" {
		t.Errorf("quote = %+v", q)
	}
	if err := q.Validate(req); err != nil {
		t.Error(err)
	}
}

func TestErrors(t *testing.T) {
	_, err := New(aggtest.Serve(t, aggtest.Answer(404, `{"message":"Couldn't recognize to token"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	_, err = New(aggtest.Serve(t, aggtest.Answer(429, `{"message":"Too many requests"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
	req := aggtest.Request
	req.ChainID = 7
	_, err = New(nil).Quote(context.Background(), req)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
}
