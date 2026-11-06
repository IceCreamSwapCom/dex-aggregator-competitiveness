// Package slots asks the token slots API where an ERC-20 token keeps a
// holder's balance and the allowance it gave a spender, so that a simulation
// can give the taker its input with two storage overrides.
package slots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// ErrUnknown means the slots API does not know the token's storage layout.
var ErrUnknown = errors.New("the slots API does not know this token")

// Keys are storage keys of the token contract, 0x + 64 hex digits.
type Keys struct {
	Balance   string // balanceOf[holder]
	Allowance string // allowance[holder][spender]
}

// Client calls GET {base}/{chainId}?token=&holderAddress=&spenderAddress=.
type Client struct{ up *upstream.Client }

// New returns the client. up must send a Referer header: without one the
// public API answers 429.
func New(up *upstream.Client) *Client { return &Client{up: up} }

// Get returns the keys for holder and spender.
func (c *Client) Get(ctx context.Context, chainID uint64, token, holder, spender string) (Keys, error) {
	q := url.Values{"token": {token}, "holderAddress": {holder}, "spenderAddress": {spender}}
	body, err := c.up.Get(ctx, "/"+strconv.FormatUint(chainID, 10)+"?"+q.Encode())
	var se *upstream.StatusError
	if errors.As(err, &se) {
		return Keys{}, fmt.Errorf("slots API answered HTTP %d: %.200s", se.Status, se.Body)
	}
	if err != nil {
		return Keys{}, fmt.Errorf("slots API: %w", err)
	}
	var r struct {
		Balance   *string `json:"balance_slot_overwrite"` // decimal, null while the layout is unknown
		Allowance *string `json:"approval_slot_overwrite"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Keys{}, fmt.Errorf("slots API: unreadable answer %.200s", body)
	}
	if r.Balance == nil || r.Allowance == nil {
		return Keys{}, ErrUnknown
	}
	var k Keys
	if k.Balance, err = key(*r.Balance); err != nil {
		return Keys{}, err
	}
	if k.Allowance, err = key(*r.Allowance); err != nil {
		return Keys{}, err
	}
	return k, nil
}

func key(decimal string) (string, error) {
	v, ok := new(big.Int).SetString(decimal, 10)
	if !ok || v.Sign() < 0 || v.BitLen() > 256 {
		return "", fmt.Errorf("slots API: %q is not a storage key", decimal)
	}
	return fmt.Sprintf("0x%064x", v), nil
}
