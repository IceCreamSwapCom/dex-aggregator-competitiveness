package zerox

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/aggtest"
)

// A real answer of GET /swap/allowance-holder/quote on Arbitrum (calldata cut,
// route left out). The taker had no allowance, hence issues.allowance.
const quoteFixture = `{"allowanceTarget":"0x0000000000001ff3684f28c67538d4d072c22734","blockNumber":"514507129","buyAmount":"39901840851361668","buyToken":"0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","fees":{"integratorFee":null,"integratorFees":null,"zeroExFee":null,"gasFee":null},"issues":{"allowance":{"actual":"0","spender":"0x0000000000001ff3684f28c67538d4d072c22734"},"balance":null,"simulationIncomplete":false,"invalidSourcesPassed":[]},"liquidityAvailable":true,"minBuyAmount":"39702331647104860","mode":"exact-in","sellAmount":"100000000","sellToken":"0xaf88d065e77c8cc2239327c5edb3a432268e5831","totalNetworkFee":"7223937876000","zid":"0x606e7ba9db76202099cdf2da","transaction":{"to":"0x0000000000001ff3684f28c67538d4d072c22734","data":"0x2213bc0b000000000000000000000000dbcd6d6e","gas":"731225","gasPrice":"22511250","value":"0"}}`

const holder = "0x0000000000001ff3684f28c67538d4d072c22734"

func TestQuote(t *testing.T) {
	a := New(aggtest.Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swap/allowance-holder/quote" {
			t.Errorf("path = %s", r.URL.Path)
		}
		aggtest.WantQuery(t, r, map[string]string{
			"chainId": "42161", "sellToken": aggtest.WETH, "buyToken": aggtest.USDC, "sellAmount": "10000000000000000",
			"taker": aggtest.Taker, "slippageBps": "50", "gasPrice": "20000000",
		})
		_, _ = w.Write([]byte(quoteFixture))
	}))
	q, err := a.Quote(context.Background(), aggtest.Request)
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountOut != "39901840851361668" || q.Tx.To != holder || q.Spender != holder || q.Tx.Value != "0" ||
		q.Tx.Data != "0x2213bc0b000000000000000000000000dbcd6d6e" || len(q.Raw) == 0 {
		t.Errorf("quote = %+v", q)
	}

	// Selling the native coin: the value is the sold amount, nothing to approve.
	sold := strings.Replace(quoteFixture, `"value":"0"`, `"value":"100000000000000000"`, 1)
	if q, err := parseQuote([]byte(sold), true); err != nil || q.Tx.Value != "100000000000000000" || q.Spender != "" {
		t.Errorf("native quote = %+v, %v", q, err)
	}
	// An allowance that already covers the sale: no issue, the spender still comes.
	approved := strings.Replace(quoteFixture, `"allowance":{"actual":"0","spender":"`+holder+`"}`, `"allowance":null`, 1)
	if q, err := parseQuote([]byte(approved), false); err != nil || q.Spender != holder {
		t.Errorf("approved quote = %+v, %v", q, err)
	}
}

func TestErrors(t *testing.T) {
	// The whole answer for a pair 0x cannot trade.
	_, err := New(aggtest.Serve(t, aggtest.Answer(200, `{"liquidityAvailable":false,"zid":"0x1"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	_, err = New(aggtest.Serve(t, aggtest.Answer(400, `{"name":"TOKEN_NOT_SUPPORTED","message":"Token is not supported"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeNoRoute)
	_, err = New(aggtest.Serve(t, aggtest.Answer(400, `{"name":"INPUT_INVALID","message":"Invalid input","data":{"details":[{"field":"taker","reason":"Invalid ethereum address"}]}}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
	_, err = New(aggtest.Serve(t, aggtest.Answer(429, `{"name":"RATE_LIMITED"}`))).Quote(context.Background(), aggtest.Request)
	aggtest.WantCode(t, err, aggregator.CodeError)
}
