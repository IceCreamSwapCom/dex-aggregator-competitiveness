// Package upstream is the HTTP client for third-party APIs (aggregators, the
// slots API, DefiLlama): one tuned, shared http.Client and a small client bound
// to a base URL + headers. JSON-RPC goes through package rpc.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxBody caps how much of an upstream answer is read into memory.
const maxBody = 16 << 20

// Referer is sent where an API needs one; it points at this project.
const Referer = "https://github.com/IceCreamSwapCom/dex-aggregator-competitiveness"

// userAgent identifies the benchmark to the aggregators.
const userAgent = "dex-aggregator-competitiveness/1"

// NewHTTPClient returns the client every upstream call shares. Connections are
// kept alive for a long time because a quote that has to open a fresh TLS
// connection is ~100-200 ms slower than one that does not.
func NewHTTPClient() *http.Client {
	return &http.Client{
		// A backstop only; every call carries its own context deadline.
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       4 * time.Minute,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ExpectContinueTimeout: time.Second,
			// Notice a dead HTTP/2 connection quickly instead of hanging a quote on it.
			HTTP2: &http.HTTP2Config{SendPingTimeout: 20 * time.Second, PingTimeout: 5 * time.Second},
		},
	}
}

// Stopwatch adds up how long upstreams took to answer the requests made under
// one context (WithStopwatch): for each request, the time from sending it to
// the last byte of its answer, or to the failure. Getting a connection (DNS,
// TCP and TLS for a new one) is our side of the call and is left out, and so
// is whatever the caller does before, between and after its requests.
type Stopwatch struct{ ns atomic.Int64 }

// Elapsed is the time measured so far.
func (s *Stopwatch) Elapsed() time.Duration { return time.Duration(s.ns.Load()) }

type stopwatchKey struct{}

// WithStopwatch makes every request sent with the returned context count
// towards s.
func WithStopwatch(ctx context.Context, s *Stopwatch) context.Context {
	return context.WithValue(ctx, stopwatchKey{}, s)
}

// timed starts measuring one request for the stopwatch of ctx, if it has one.
// The returned context must be the request's; stop is called when the answer
// has been read or the request has failed.
func timed(ctx context.Context) (_ context.Context, stop func()) {
	sw, _ := ctx.Value(stopwatchKey{}).(*Stopwatch)
	if sw == nil {
		return ctx, func() {}
	}
	var (
		mu         sync.Mutex // the transport may call the trace from its own goroutines
		waiting    time.Time  // when the request began waiting for a connection
		connecting time.Duration
	)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) {
			mu.Lock()
			waiting = time.Now()
			mu.Unlock()
		},
		// Once per attempt, so a request the transport sends again on a fresh
		// connection has both waits taken out.
		GotConn: func(httptrace.GotConnInfo) {
			mu.Lock()
			if !waiting.IsZero() {
				connecting += time.Since(waiting)
				waiting = time.Time{}
			}
			mu.Unlock()
		},
	})
	started := time.Now()
	return ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		sw.ns.Add(int64(time.Since(started) - connecting))
	}
}

// StatusError is returned for every answer that is not 2xx.
type StatusError struct {
	Status int
	Body   []byte // first few KiB of the answer
}

func (e *StatusError) Error() string { return fmt.Sprintf("upstream answered HTTP %d", e.Status) }

// Client sends requests to one upstream API.
type Client struct {
	hc     *http.Client
	base   string
	header http.Header
}

// New binds hc to a base URL. header is added to every request (API keys,
// Referer); it is never logged.
func New(hc *http.Client, baseURL string, header http.Header) *Client {
	return &Client{hc: hc, base: strings.TrimRight(baseURL, "/"), header: header}
}

// Get fetches base+pathAndQuery and returns the body of a 2xx answer.
func (c *Client) Get(ctx context.Context, pathAndQuery string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, pathAndQuery, nil)
}

// PostJSON posts body as JSON and returns the body of a 2xx answer.
func (c *Client) PostJSON(ctx context.Context, path string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, path, b)
}

func (c *Client) do(ctx context.Context, method, pathAndQuery string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	ctx, stop := timed(ctx)
	defer stop() // after the body has been read
	req, err := http.NewRequestWithContext(ctx, method, c.base+pathAndQuery, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, withoutQuery(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Status: resp.StatusCode, Body: data[:min(len(data), 4096)]}
	}
	return data, nil
}

// withoutQuery removes the query string from the URL a transport error
// carries. Queries hold wallet addresses, and for some providers an API key;
// neither belongs in a log line or an error message.
func withoutQuery(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if i := strings.IndexByte(ue.URL, '?'); i >= 0 {
			ue.URL = ue.URL[:i]
		}
	}
	return err
}
