package slots

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/slots/8453" || q.Get("token") != "0xt" || q.Get("holderAddress") != "0xh" || q.Get("spenderAddress") != "0xs" {
			t.Errorf("request = %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"balance_slot_overwrite":"255","approval_slot_overwrite":"1"}`))
	}))
	defer srv.Close()
	k, err := New(upstream.New(srv.Client(), srv.URL+"/slots/", nil)).Get(context.Background(), 8453, "0xt", "0xh", "0xs")
	if err != nil {
		t.Fatal(err)
	}
	if k.Balance != "0x00000000000000000000000000000000000000000000000000000000000000ff" ||
		k.Allowance != "0x0000000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("keys = %+v", k)
	}
}

func TestUnknownToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"balance_slot_overwrite":null,"approval_slot_overwrite":null}`))
	}))
	defer srv.Close()
	if _, err := New(upstream.New(srv.Client(), srv.URL, nil)).Get(context.Background(), 1, "0xt", "0xh", "0xs"); !errors.Is(err, ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown", err)
	}
}
