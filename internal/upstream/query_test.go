package upstream

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A transport error must not carry the query string: it holds wallet addresses
// and, for some providers, an API key, and the error ends up in the log.
func TestTransportErrorHasNoQuery(t *testing.T) {
	// Nothing listens on this port, so the request fails in the transport.
	c := New(&http.Client{Timeout: 2 * time.Second}, "http://127.0.0.1:1", nil)
	_, err := c.Get(context.Background(), "/quote?payer=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045&apiKey=secret")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	msg := err.Error()
	if strings.Contains(msg, "payer") || strings.Contains(msg, "secret") || strings.Contains(msg, "?") {
		t.Errorf("transport error leaks the query: %s", msg)
	}
	if !strings.Contains(msg, "/quote") {
		t.Errorf("transport error lost the path: %s", msg)
	}
}
