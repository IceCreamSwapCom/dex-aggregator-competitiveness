// Package rpc is a minimal Ethereum JSON-RPC client over HTTP.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

// Client sends JSON-RPC calls to one node.
type Client struct {
	url string
	hc  *http.Client
	id  atomic.Uint64
}

// New returns a client for the node at url. The url may carry an API key, so
// it is never part of an error message.
func New(url string, hc *http.Client) *Client { return &Client{url: url, hc: hc} }

// Error is an error the node answered with, as opposed to a failed transport.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Call runs method with params and decodes the result into out.
func (c *Client) Call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: building the request failed", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", method, ctx.Err())
		}
		return fmt.Errorf("%s: the node could not be reached", method) // the error text would carry the url
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s: reading the answer: %w", method, err)
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("%s: the node answered HTTP %d without JSON-RPC", method, resp.StatusCode)
	}
	if r.Error != nil {
		return r.Error
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("%s: unexpected result %.200s", method, r.Result)
	}
	return nil
}
