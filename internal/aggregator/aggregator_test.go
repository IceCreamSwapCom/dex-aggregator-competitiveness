package aggregator

import "testing"

func TestValidate(t *testing.T) {
	erc20 := Request{TokenIn: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Amount: "1000"}
	native := Request{TokenIn: NativeToken, Amount: "1000"}
	router := "0x6131b5fae19ea4f9d964eac0408e4408b66337b5"
	good := Quote{AmountOut: "5", Tx: Tx{To: router, Data: "0x12345678", Value: "0"}, Spender: router}
	if err := good.Validate(erc20); err != nil {
		t.Errorf("valid ERC-20 quote: %v", err)
	}
	nativeQuote := good
	nativeQuote.Tx.Value, nativeQuote.Spender = "1000", ""
	if err := nativeQuote.Validate(native); err != nil {
		t.Errorf("valid native quote: %v", err)
	}

	for name, tc := range map[string]struct {
		edit func(*Quote)
		req  Request
	}{
		"amountOut not a number":  {func(q *Quote) { q.AmountOut = "1e5" }, erc20},
		"no to":                   {func(q *Quote) { q.Tx.To = "" }, erc20},
		"no selector":             {func(q *Quote) { q.Tx.Data = "0x12" }, erc20},
		"value on ERC-20 input":   {func(q *Quote) { q.Tx.Value = "1" }, erc20},
		"no spender":              {func(q *Quote) { q.Spender = "" }, erc20},
		"too little native value": {func(q *Quote) { q.Tx.Value = "999" }, native},
		"too much native value":   {func(q *Quote) { q.Tx.Value = "1001" }, native},
	} {
		q := good
		if tc.req.TokenIn == NativeToken {
			q = nativeQuote
		}
		tc.edit(&q)
		if err := q.Validate(tc.req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
