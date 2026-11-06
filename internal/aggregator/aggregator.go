// Package aggregator defines what an aggregator adapter is. Adding an
// aggregator: write a package with a type that implements Aggregator, follow
// the rules in README.md ("Add an aggregator"), and register it in
// cmd/collector/aggregators.go.
package aggregator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// NativeToken is how requests spell the native coin.
const NativeToken = config.NativeToken

// ZeroAddress is how several APIs spell the native coin.
const ZeroAddress = "0x0000000000000000000000000000000000000000"

// Aggregator is one upstream quote API.
type Aggregator interface {
	// ID is the stable identifier, used in the config and the database.
	ID() string
	// Quote asks for the best swap: one request (two for APIs with a separate
	// build step), no retries. Failures the API explains are returned as
	// *Error; transport and context errors are returned as they are.
	Quote(ctx context.Context, req Request) (*Quote, error)
}

// Request is the same for every aggregator in a round.
type Request struct {
	ChainID          uint64
	TokenIn          string // NativeToken for the native coin
	TokenOut         string
	TokenInDecimals  int
	TokenOutDecimals int
	Amount           string // base units, decimal
	Taker            string // sends the swap and receives the output
	SlippageBps      int
	GasPrice         string // wei, decimal: what the simulation uses; a hint for APIs that take one
	IntegratorID     string // the free-form name for APIs that ask who is calling
}

// Tx is the transaction the taker sends.
type Tx struct {
	To    string
	Data  string
	Value string // wei, decimal
}

// Quote is an aggregator's answer.
type Quote struct {
	AmountOut string // base units, decimal: what the aggregator says the taker receives
	Tx        Tx
	Spender   string          // gets the ERC-20 allowance; "" for a native input
	Raw       json.RawMessage // the API's answer as it came, for audits
}

// Validate refuses an answer that cannot be a transaction for this request:
// it must carry exactly the sold amount as value for a native input and no
// value for an ERC-20 input, and name a spender for an ERC-20 input.
func (q *Quote) Validate(req Request) error {
	amountIn, _ := new(big.Int).SetString(req.Amount, 10)
	value, ok := new(big.Int).SetString(q.Tx.Value, 10)
	switch {
	case !IsUint(q.AmountOut):
		return errors.New("amountOut is not an integer")
	case !IsAddress(q.Tx.To):
		return errors.New("tx.to is not an address")
	case !isCalldata(q.Tx.Data):
		return errors.New("tx.data is not calldata")
	case !ok || value.Sign() < 0:
		return errors.New("tx.value is not an integer")
	case IsNative(req.TokenIn) && value.Cmp(amountIn) != 0:
		return fmt.Errorf("tx.value is %s, not the sold amount %s", value, amountIn)
	case !IsNative(req.TokenIn) && value.Sign() != 0:
		return fmt.Errorf("tx.value is %s for an ERC-20 input", value)
	case !IsNative(req.TokenIn) && !IsAddress(q.Spender):
		return errors.New("no spender for the ERC-20 input")
	}
	return nil
}

// Code classifies a failed quote. The values are stored in quotes.status.
type Code string

const (
	CodeNoRoute Code = "no_route" // the aggregator says it cannot do this trade
	CodeError   Code = "error"    // any other failure the API answered with
	CodeInvalid Code = "invalid"  // an answer that is not a usable transaction
)

// Error is a quote failure with a readable message.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Errorf builds an *Error; the message is capped so an API cannot fill the database.
func Errorf(code Code, format string, args ...any) *Error {
	msg := strings.TrimSpace(fmt.Sprintf(format, args...))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return &Error{Code: code, Message: msg}
}

// NoRoute is the error for an aggregator that says it cannot do the trade,
// with its message or else "no route found".
func NoRoute(msg string) *Error {
	if msg == "" {
		msg = "no route found"
	}
	return Errorf(CodeNoRoute, "%s", msg)
}

// HTTPError is the error for a non-2xx answer an adapter has no better reading for.
func HTTPError(id string, se *upstream.StatusError, detail string) *Error {
	if detail != "" {
		return Errorf(CodeError, "%s answered HTTP %d: %s", id, se.Status, detail)
	}
	return Errorf(CodeError, "%s answered HTTP %d", id, se.Status)
}

// SlippagePercent is the slippage as a percentage (0.5 for 50 bps).
func SlippagePercent(bps int) float64 { return float64(bps) / 100 }

// SlippageFraction is the slippage as a fraction (0.005 for 50 bps).
func SlippageFraction(bps int) float64 { return float64(bps) / 10_000 }

// IsNative reports whether addr is the native coin placeholder.
func IsNative(addr string) bool { return strings.EqualFold(addr, NativeToken) }

// ZeroNative spells the native coin as the zero address and everything else in lower case.
func ZeroNative(addr string) string {
	if IsNative(addr) {
		return ZeroAddress
	}
	return strings.ToLower(addr)
}

// IsAddress reports whether s is a 0x-prefixed 20-byte hex address.
func IsAddress(s string) bool { return len(s) == 42 && isHex(s) }

// IsUint reports whether s is a non-empty string of decimal digits.
func IsUint(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// isCalldata requires at least a 4-byte selector.
func isCalldata(s string) bool { return len(s) >= 10 && len(s)%2 == 0 && isHex(s) }

func isHex(s string) bool {
	return len(s) >= 2 && s[:2] == "0x" && strings.Trim(strings.ToLower(s[2:]), "0123456789abcdef") == ""
}
