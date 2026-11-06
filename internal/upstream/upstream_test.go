package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNum(t *testing.T) {
	tests := []struct {
		json     string
		wantUint string // "" = not an unsigned integer
	}{
		{`"24997558"`, "24997558"},
		{`513804172`, "513804172"}, // a JSON number
		{`"0x1ea0078c"`, "513804172"},
		{`"0"`, "0"},
		{`"007"`, "7"},
		{`" 12 "`, "12"},
		// Bigger than any float64 can hold exactly, and bigger than uint64.
		{`"115792089237316195423570985008687907853269984665640564039457584007913129639935"`,
			"115792089237316195423570985008687907853269984665640564039457584007913129639935"},
		{`115792089237316195423570985008687907853269984665640564039457584007913129639935`,
			"115792089237316195423570985008687907853269984665640564039457584007913129639935"},
		{`null`, ""},
		{`""`, ""},
		{`"-5"`, ""},
		{`-5`, ""},
		{`1.5`, ""},
		{`"1e18"`, ""},
		{`"0x"`, ""},
		{`"abc"`, ""},
		{`{"a":1}`, ""},
		{`true`, ""},
	}
	for _, tc := range tests {
		var v struct {
			N Num `json:"n"`
		}
		if err := json.Unmarshal([]byte(`{"n":`+tc.json+`}`), &v); err != nil {
			t.Errorf("%s: unmarshal: %v", tc.json, err)
			continue
		}
		got, ok := v.N.Uint()
		if ok != (tc.wantUint != "") || got != tc.wantUint {
			t.Errorf("%s: Uint() = %q, %v; want %q", tc.json, got, ok, tc.wantUint)
		}
	}
	// A field that is absent stays empty.
	var v struct {
		N Num `json:"n"`
	}
	_ = json.Unmarshal([]byte(`{}`), &v)
	if _, ok := v.N.Uint(); ok {
		t.Error("an absent field must not be a number")
	}
}

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/echo":
			body, _ := io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"method": r.Method, "query": r.URL.RawQuery, "body": string(body),
				"referer": r.Header.Get("Referer"), "apikey": r.Header.Get("apikey"),
				"contentType": r.Header.Get("Content-Type"), "accept": r.Header.Get("Accept"), "userAgent": r.Header.Get("User-Agent"),
			})
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
		}
	}))
	defer srv.Close()
	// A trailing slash on the base URL must not double up.
	c := New(srv.Client(), srv.URL+"/api/", http.Header{"Referer": {"https://example.com"}, "apikey": {"k"}})

	var echo map[string]string
	raw, err := c.Get(context.Background(), "/echo?token=a,b")
	if err != nil || json.Unmarshal(raw, &echo) != nil {
		t.Fatalf("Get: %v %s", err, raw)
	}
	if echo["method"] != "GET" || echo["query"] != "token=a,b" || echo["referer"] != "https://example.com" || echo["apikey"] != "k" ||
		echo["accept"] != "application/json" || echo["userAgent"] == "" || echo["contentType"] != "" {
		t.Errorf("GET as seen by the upstream: %v", echo)
	}

	raw, err = c.PostJSON(context.Background(), "/echo", map[string]string{"block": "latest"})
	if err != nil || json.Unmarshal(raw, &echo) != nil {
		t.Fatalf("PostJSON: %v %s", err, raw)
	}
	if echo["method"] != "POST" || echo["body"] != `{"block":"latest"}` || echo["contentType"] != "application/json" || echo["referer"] == "" {
		t.Errorf("POST as seen by the upstream: %v", echo)
	}

	_, err = c.Get(context.Background(), "/missing")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 502 || string(se.Body) != "<html>502</html>" {
		t.Errorf("error = %#v, want a StatusError carrying the body", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "/echo"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call: error = %v", err)
	}
}
