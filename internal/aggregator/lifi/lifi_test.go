package lifi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// A real answer of GET /v1/quote for 0.01 WETH -> USDC on Arbitrum, cut to
// what the adapter reads (calldata cut).
const quoteFixture = `{"type":"lifi","tool":"nordstern","estimate":{"fromAmount":"10000000000000000","toAmount":"23084475","toAmountMin":"22969053","approvalAddress":"0x1231DEB6f5749EF6cE6943a275A1D3E7486F4EaE","executionDuration":0},"transactionRequest":{"chainId":42161,"gasPrice":"0x1327cf0","gasLimit":"0x311631","value":"0x0","to":"0x1231DEB6f5749EF6cE6943a275A1D3E7486F4EaE","data":"0x5fd9ae2ee572595bab0f7e4546e4466e2d89d7eba338ed719846f9a5ca603a9b99361d13","from":"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"}}`

const diamond = "0x1231DEB6f5749EF6cE6943a275A1D3E7486F4EaE"

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/quote" {
			t.Errorf("path = %s", r.URL.Path)
		}
		taker := strings.ToLower(aggtest.Taker)
		aggtest.WantQuery(t, r, map[string]string{
			"fromChain": "42161", "toChain": "42161", "fromToken": aggtest.WETH, "toToken": strings.ToLower(aggtest.USDC),
			"fromAmount": "10000000000000000", "fromAddress": taker, "toAddress": taker, "slippage": "0.005", "integrator": "dexbench",
		})
		_, _ = w.Write([]byte(quoteFixture))
	}))
	q, err := a.Quote(context.Background(), aggtest.Request)
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountOut != "23084475" || q.Tx.To != diamond || q.Spender != diamond || q.Tx.Value != "0" || len(q.Raw) == 0 {
		t.Errorf("quote = %+v", q)
	}

	// Selling the native coin: the zero address goes out, the value is hex on the wire.
	native := aggtest.Request
	native.TokenIn = aggregator.NativeToken
	a = New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		aggtest.WantQuery(t, r, map[string]string{"fromToken": aggregator.ZeroAddress})
		_, _ = w.Write([]byte(strings.Replace(quoteFixture, `"value":"0x0"`, `"value":"0x2386f26fc10000"`, 1)))
	}))
	if q, err := a.Quote(context.Background(), native); err != nil || q.Tx.Value != "10000000000000000" || q.Spender != "" {
		t.Errorf("native quote = %+v, %v", q, err)
	}
}

func TestErrors(t *testing.T) {
	for status, body := range map[int]string{
		404: `{"message":"No available quotes for the requested transfer","code":1002}`,
		400: `{"message":"No available quotes for the requested transfer","code":1002}`,
	} {
		_, err := New(aggtest.Serve(t, aggtest.Answer(status, body))).Quote(context.Background(), aggtest.Request)
		aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	}
	_, err := New(aggtest.Serve(t, aggtest.Answer(400, `{"message":"Rate limit exceeded","code":1005}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
}
