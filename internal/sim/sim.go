// Package sim simulates an aggregator's transaction the way the taker's wallet
// would send it, with eth_call at a given block, and measures what the taker
// receives. METHODOLOGY.md explains every rule in here; the code is meant to be
// read next to it.
package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/config"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/rpc"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/slots"
)

// The gas price is baseFee + the median tip of the last tipBlocks blocks, each
// block's tip being its median (50th percentile) priority fee.
const tipBlocks = 20

// Gas the proxy's own work needs on top of the swap's gas limit: its calldata
// head, the balance read before and after the swap, and with value the 9,000
// a value-carrying CALL costs. The swap then sees about the gasleft() it would
// see in a transaction with the same gas limit.
const (
	proxyGas      = 15_000
	proxyValueGas = 9_000
)

// Simulator simulates on one chain.
type Simulator struct {
	rpc                *rpc.Client
	chainID            uint64
	maxTxGas           uint64
	gasLimitMultiplier float64
	nextBlockSeconds   uint64 // the next block's timestamp is this much after its parent's
}

func New(c *rpc.Client, ch config.Chain, gasLimitMultiplier float64) *Simulator {
	return &Simulator{rpc: c, chainID: ch.ID, maxTxGas: ch.MaxTxGas, gasLimitMultiplier: gasLimitMultiplier,
		nextBlockSeconds: nextBlockSeconds(ch.BlockTime.Duration)}
}

// nextBlockSeconds is the chain's block time in whole seconds, rounded up:
// block timestamps count seconds, and the next block is never earlier.
func nextBlockSeconds(blockTime time.Duration) uint64 {
	return max(1, uint64(math.Ceil(blockTime.Seconds())))
}

// Block is a pinned block and the gas price transactions are simulated with there.
//
// A swap is simulated as the first transaction of the next block, where a
// transaction sent now lands: on the state after block Number, with
// block.number Number+1 and block.timestamp Time plus the chain's block time.
// With Time 0 it is simulated in block Number itself, as rounds of
// methodology 0.1 and 0.2 were.
type Block struct {
	Number   uint64
	Time     uint64   // block Number's timestamp
	BaseFee  *big.Int // 0 on a chain without EIP-1559
	GasPrice *big.Int
}

// Pin reads the latest block and the gas price a transaction pays in it, with
// one call: eth_feeHistory up to "latest" also names that block and its base
// fee. On a chain without EIP-1559 the base fee is 0 and the tips are the
// whole gas prices transactions paid.
func (s *Simulator) Pin(ctx context.Context) (Block, error) {
	var history struct {
		OldestBlock  string     `json:"oldestBlock"`
		BaseFees     []string   `json:"baseFeePerGas"` // one per block, and one more for the next block
		GasUsedRatio []float64  `json:"gasUsedRatio"`  // one per block
		Reward       [][]string `json:"reward"`
	}
	err := s.rpc.Call(ctx, &history, "eth_feeHistory", hexBig(big.NewInt(tipBlocks)), "latest", []int{50})
	if err != nil {
		return Block{}, err
	}
	blocks := len(history.GasUsedRatio)
	oldest, ok := parseHexBig(history.OldestBlock)
	if !ok || !oldest.IsUint64() || blocks == 0 {
		return Block{}, fmt.Errorf("eth_feeHistory names no blocks (oldestBlock %q)", history.OldestBlock)
	}
	b := Block{Number: oldest.Uint64() + uint64(blocks) - 1, BaseFee: new(big.Int)}
	if len(history.BaseFees) >= blocks {
		if b.BaseFee, ok = parseHexBig(history.BaseFees[blocks-1]); !ok {
			return Block{}, fmt.Errorf("block %d has base fee %q", b.Number, history.BaseFees[blocks-1])
		}
	}
	var tips []*big.Int
	for _, r := range history.Reward {
		if len(r) == 1 {
			if tip, ok := parseHexBig(r[0]); ok {
				tips = append(tips, tip)
			}
		}
	}
	if len(tips) == 0 {
		return Block{}, errors.New("eth_feeHistory returned no rewards")
	}
	slices.SortFunc(tips, func(x, y *big.Int) int { return x.Cmp(y) })
	b.GasPrice = new(big.Int).Add(b.BaseFee, tips[len(tips)/2])
	if b.Time, err = s.Timestamp(ctx, b.Number); err != nil {
		return Block{}, err
	}
	return b, nil
}

// Timestamp is block n's timestamp.
func (s *Simulator) Timestamp(ctx context.Context, n uint64) (uint64, error) {
	var header struct {
		Timestamp string `json:"timestamp"`
	}
	if err := s.rpc.Call(ctx, &header, "eth_getBlockByNumber", hexBig(new(big.Int).SetUint64(n)), false); err != nil {
		return 0, fmt.Errorf("block %d: %w", n, err)
	}
	t, ok := parseHexBig(header.Timestamp)
	if !ok || !t.IsUint64() || t.Sign() == 0 {
		return 0, fmt.Errorf("block %d has timestamp %q", n, header.Timestamp)
	}
	return t.Uint64(), nil
}

// call is an eth_call at block b with the state overrides, in the next block
// unless b.Time is 0.
func (s *Simulator) call(ctx context.Context, out any, call map[string]any, b Block, overrides map[string]any) error {
	params := []any{call, hexBig(new(big.Int).SetUint64(b.Number)), overrides}
	if b.Time != 0 {
		params = append(params, map[string]string{
			"number": hexBig(new(big.Int).SetUint64(b.Number + 1)),
			"time":   hexBig(new(big.Int).SetUint64(b.Time + s.nextBlockSeconds)),
		})
	}
	return s.rpc.Call(ctx, out, "eth_call", params...)
}

// Swap is an aggregator's transaction and what the taker sells with it.
type Swap struct {
	Taker    string
	TokenIn  string // config.NativeToken for the native coin
	TokenOut string
	AmountIn *big.Int
	Slots    slots.Keys // where tokenIn keeps the taker's balance and allowance; ERC-20 only
	To       string
	Data     []byte
	Value    *big.Int
	// OutBefore is the taker's tokenOut balance before the swap, for an ERC-20
	// tokenOut, from Balance; nil counts as 0.
	OutBefore *big.Int
}

// Balance is holder's balance of an ERC-20 token at block b. The proxy reports
// an ERC-20 output as the balance after the swap; the balance before comes from
// this separate call, so that the swap finds the token as cold as in a real
// transaction (see contracts/SimulationProxy.sol). A round reads it once: all
// its swaps start from the same fresh taker, which nothing else touches.
func (s *Simulator) Balance(ctx context.Context, b Block, token, holder string) (*big.Int, error) {
	call := map[string]any{"to": token, "data": fmt.Sprintf("0x%x", slices.Concat(balanceOfSelector, addressWord(holder)))}
	var out string
	if err := s.rpc.Call(ctx, &out, "eth_call", call, hexBig(new(big.Int).SetUint64(b.Number))); err != nil {
		return nil, fmt.Errorf("balance of %s: %w", token, err)
	}
	ret, err := hexBytes(out)
	if err != nil {
		return nil, fmt.Errorf("balance of %s: %w", token, err)
	}
	w, err := words(ret, 1)
	if err != nil {
		return nil, fmt.Errorf("balance of %s: %w", token, err)
	}
	return w[0], nil
}

// Result is what one simulation measured.
type Result struct {
	Success   bool
	AmountOut *big.Int // what the taker's balance of tokenOut grew by
	Gas       uint64   // what the swap uses as a transaction of its own
	Revert    string   // why it failed
}

// Run simulates the swap at block b as a transaction with the given gas limit.
// A swap that fails is a Result; an error means the simulation itself could
// not be run (the node, the network), which is never the aggregator's fault.
func (s *Simulator) Run(ctx context.Context, b Block, sw Swap, gasLimit uint64) (Result, error) {
	callGas := gasLimit + gasLimit/63 + proxyGas // a CALL forwards at most 63/64 of the gas left
	if sw.Value.Sign() > 0 {
		callGas += proxyValueGas
	}
	callGas = min(callGas, s.maxTxGas)

	data := slices.Concat(simulateSelector, addressWord(sw.To), addressWord(sw.TokenOut), word(big.NewInt(96)), bytesTail(sw.Data))
	taker := strings.ToLower(sw.Taker)
	// The taker holds exactly what it sells and what the call's gas costs.
	funding := new(big.Int).Mul(new(big.Int).SetUint64(callGas), b.GasPrice)
	funding.Add(funding, sw.Value)
	overrides := map[string]any{taker: map[string]any{"code": proxyCode, "balance": hexBig(funding)}}
	if !isNative(sw.TokenIn) {
		amount := "0x" + fmt.Sprintf("%064x", sw.AmountIn)
		overrides[strings.ToLower(sw.TokenIn)] = map[string]any{
			"stateDiff": map[string]string{sw.Slots.Balance: amount, sw.Slots.Allowance: amount},
		}
	}
	call := map[string]any{
		"from":     taker,
		"to":       taker,
		"gas":      hexBig(new(big.Int).SetUint64(callGas)),
		"gasPrice": hexBig(b.GasPrice),
		"value":    hexBig(sw.Value),
		"data":     fmt.Sprintf("0x%x", data),
	}

	var out string
	if err := s.call(ctx, &out, call, b, overrides); err != nil {
		if msg, ok := executionFailed(err); ok {
			return Result{Revert: msg}, nil
		}
		return Result{}, err
	}
	ret, err := hexBytes(out)
	if err != nil {
		return Result{}, fmt.Errorf("eth_call returned %.80q", out)
	}
	w, err := words(ret, 3)
	if err != nil {
		return Result{}, errors.New("the proxy returned nothing: the node ignored the state override")
	}
	returnData, err := dynamicBytes(ret, 3)
	if err != nil {
		return Result{}, fmt.Errorf("proxy return data: %w", err)
	}
	r := Result{Success: w[0].Sign() != 0, AmountOut: w[1], Gas: txGas(w[2].Uint64(), sw.Data, sw.Value)}
	if !r.Success {
		r.Revert = decodeRevert(returnData)
		return r, nil
	}
	if isNative(sw.TokenOut) || sw.OutBefore == nil {
		return r, nil // the proxy measured the native output itself
	}
	if r.AmountOut.Sub(r.AmountOut, sw.OutBefore).Sign() < 0 {
		r.AmountOut.SetInt64(0) // the balance shrank
	}
	return r, nil
}

// Measure finds the gas limit a wallet would send the swap with and runs it
// with that limit. A probe at the chain's per-transaction cap gives the gas the
// swap uses; the limit is gasLimitMultiplier × that, at most the cap. A swap
// that fails the probe keeps the cap as its limit, and the probe is its result.
func (s *Simulator) Measure(ctx context.Context, b Block, sw Swap) (uint64, Result, error) {
	r, err := s.Run(ctx, b, sw, s.maxTxGas)
	if err != nil || !r.Success {
		return s.maxTxGas, r, err
	}
	limit := gasLimit(r.Gas, s.gasLimitMultiplier, s.maxTxGas)
	r, err = s.Run(ctx, b, sw, limit)
	return limit, r, err
}

// gasLimit is the limit a wallet sends a swap with that uses gas.
func gasLimit(gas uint64, multiplier float64, maxTxGas uint64) uint64 {
	return min(maxTxGas, uint64(math.Ceil(float64(gas)*multiplier)))
}

// txGas turns the gas the proxy measured around its CALL into the gas of the
// swap as a transaction of its own. The CALL pays what a transaction does not:
// the cold access of the target (2,600, EIP-2929; a transaction pre-warms its
// to) and with value 9,000 less the 2,300 stipend the callee gets. The
// transaction instead pays 21,000 and its calldata (16 gas per non-zero byte,
// 4 per zero byte).
func txGas(measured uint64, data []byte, value *big.Int) uint64 {
	overhead := uint64(2_600)
	if value.Sign() > 0 {
		overhead += 9_000 - 2_300
	}
	gas := max(measured, overhead) - overhead + 21_000
	for _, b := range data {
		if b == 0 {
			gas += 4
		} else {
			gas += 16
		}
	}
	return gas
}

// executionFailed tells a node's "the call failed while executing" (which here
// can only be the swap running out of gas) apart from every other error.
func executionFailed(err error) (string, bool) {
	var re *rpc.Error
	if !errors.As(err, &re) {
		return "", false
	}
	m := strings.ToLower(re.Message)
	if re.Code == 3 || strings.Contains(m, "revert") || strings.Contains(m, "out of gas") || strings.Contains(m, "invalid opcode") {
		return re.Message, true
	}
	return "", false
}

// CheckNode makes sure the RPC serves the configured chain and applies state
// and block overrides, without which no simulation measures anything.
func (s *Simulator) CheckNode(ctx context.Context) error {
	var id string
	if err := s.rpc.Call(ctx, &id, "eth_chainId"); err != nil {
		return err
	}
	if v, ok := parseHexBig(id); !ok || v.Cmp(new(big.Int).SetUint64(s.chainID)) != 0 {
		return fmt.Errorf("the RPC serves chain %s, not %d", id, s.chainID)
	}
	b, err := s.Pin(ctx)
	if err != nil {
		return err
	}
	// A plain call to an address without code succeeds and receives nothing.
	sw := Swap{
		Taker: "0x00000000000000000000000000000000000b0b01", To: "0x00000000000000000000000000000000000b0b02",
		TokenIn: config.NativeToken, TokenOut: config.NativeToken, AmountIn: new(big.Int), Data: []byte{0, 0, 0, 0}, Value: new(big.Int),
	}
	r, err := s.Run(ctx, b, sw, 100_000)
	if err != nil {
		return err
	}
	if !r.Success {
		return fmt.Errorf("a test simulation failed: %s", r.Revert)
	}
	// A contract that returns block.timestamp sees the next block's.
	// (block.number cannot be checked: on Arbitrum it is Ethereum's.)
	const clock = "0x00000000000000000000000000000000000b0b03"
	var out string
	err = s.call(ctx, &out, map[string]any{"to": clock, "data": "0x"}, b, map[string]any{clock: map[string]any{"code": timestampCode}})
	if err != nil {
		return fmt.Errorf("a call with block overrides: %w", err)
	}
	if t, ok := parseHexBig(out); !ok || t.Cmp(new(big.Int).SetUint64(b.Time+s.nextBlockSeconds)) != 0 {
		return fmt.Errorf("the RPC ignores block overrides: block.timestamp is %s, not %d", out, b.Time+s.nextBlockSeconds)
	}
	return nil
}

// timestampCode returns block.timestamp: TIMESTAMP PUSH1 0 MSTORE PUSH1 32 PUSH1 0 RETURN.
const timestampCode = "0x4260005260206000f3"

func isNative(token string) bool { return strings.EqualFold(token, config.NativeToken) }
