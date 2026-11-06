# Methodology

Version 0.2. Every stored round carries the methodology version it was measured under. A change to any rule
below gets a new version and a changelog entry, so data measured under different rules is never mixed silently.
The parameters of the rules (chains, pairs, sizes and their weights, timeouts, slippage, the gas limit
multiplier) are in `config/benchmark.json`.

The benchmark asks how much a user would really get from each aggregator for the same trade, sent the moment the
aggregator answered, and how well that holds up when the transaction lands later. Every rule applies to every
aggregator in the same way.

## 1. A round

A round is one trade on one chain, asked of every aggregator of the configuration at the same moment. Every
aggregator is asked on every chain: one that does not serve the chain fails there (`no_route` or `error`) and is
judged by the round like any other failure.

### The trade

The benchmark trades what users trade, in the same proportions (section 1.1):

- **Chain:** picked at random with the chain's weight, its share of the aggregator volume of the configured chains.
- **Pair and size:** a configured pair and one of its size buckets, picked together at random with their weight,
  the share of the chain's volume that trades this pair at this size. Then one of the pair's two directions, with
  equal chance.
- **Amount:** the size bucket ($10, $100, $1k, $10k, $100k or $1m, set in token units) times a random factor
  between 0.8 and 1.25 (uniform on a log scale). No two rounds sell exactly the same amount, so no answer can be
  prepared or cached for one.
- **Taker:** a fresh random address for every round, the same for every aggregator in it. It holds nothing and
  has never been used.

### The same request for everyone

- **Slippage:** 50 bp, or 10 bp when both tokens are stablecoins.
- **Gas price:** the T0 gas price (section 2.3), passed to every API that takes one as a hint.
- **Sources:** each API's defaults, RFQ included. An RFQ quote that expires within 30 s reverts at T30, as it
  would for a user.
- **Fees:** no integrator or partner fee is asked for. Where an API charges its own fee by default, that is what
  its users pay, and what is measured.
- **Transaction:** an approval plus one transaction. Permit2 signatures and gasless or intent orders cannot be
  simulated as one call and are not used.
- **API keys:** an API that has no keyless access, or whose keyless access allows fewer requests than the
  benchmark sends or charges a fee that keyed integrators do not pay, is asked with a key. Which ones do is listed
  in `cmd/collector/aggregators.go`.
- **Each aggregator at its best:** aggregators serve a request by who sends it (an API key, an integrator id, a
  Referer): limits, infrastructure, fees and quotes can differ. Each one is asked the way that gets its best
  offer, so the benchmark compares the best every aggregator is able to deliver, not what its least favored
  caller gets. An aggregator can give the benchmark a key or id of its own for that.

Each aggregator's adapter (`internal/aggregator/<name>`) and `cmd/collector/aggregators.go` show the exact request
it sends, with every header.

### 1.1 Where the weights come from

`cmd/distribution` derives the chains' weights, pairs, sizes and their weights, and writes them into the
configuration. It uses only public data, with no account at any service:

- **Chains:** DefiLlama's aggregator volume of the last 30 days, summed over every aggregator it lists. A chain's
  weight is its share of the configured chains' volume, in basis points.
- **Trades:** every swap the LI.FI (`LiFiGenericSwapCompleted`) and KyberSwap (`Swapped`) routers logged on the
  chain over the last 7 days, read from the chain with `eth_getLogs`. These two log both tokens and both amounts
  of every swap. One trade per transaction: LI.FI also routes through KyberSwap, and the outer swap is the user's.
- **Prices:** derived from the same trades, at the median rate of at least 3 of a token's trades of $3.16 to $1
  billion: the median of the trades, not of their volume, so that a few huge bogus events cannot set a price.
  The chain's anchor stablecoins count as $1 (recorded in `derived.anchors`; the first time, the configuration's
  stablecoins); the native coin (and its wrapped token) is priced from its trades
  against them, every other token from its trades against the stablecoins and the native coin. A price never
  comes from another derived price, so a thin token's bad price cannot spread.
- **Value:** a trade counts with the smaller of its two sides. A trade whose sides are more than 2× apart, a trade
  under $3.16 or over $1 billion (a router logs whatever it is called with), and wrapping or unwrapping the native
  coin are left out. One receiving address counts with at most
  1% of the chain's volume, so that no single wallet or bot decides the distribution.
- **Size bucket:** the nearest bucket on a log scale ($10 covers $3.16–31.6, and so on; $1m covers everything
  above $316k).
- **Pairs:** taken by volume until they cover 80% of it, or 20 pairs. A pair is left out when one of its tokens
  has no price or the token slots API cannot simulate it. A pair needs one token that is the native coin or trades
  against it, to value gas (section 3.1): for a pair without one, the biggest pair of one of its tokens with the
  native coin is taken too.
- **Weights:** each (pair, size) gets its share of the volume taken, in basis points. Smaller than half a basis
  point is left out.
- **Tokens:** the configuration keeps its tokens' names and flags. A new token is named by its symbol, and counts
  as a stablecoin when its symbol contains "USD" and it trades at $1 ± 2%. Sizes are the bucket's dollars in token
  units, to two significant digits.

Each chain records what it was derived from (`derived`): the blocks, the number of trades read and the anchors.
`cmd/distribution -reproduce` reads the same blocks again and derives the same pairs and sizes, provided the node
returns the same events; it warns when the number of trades differs. New weights change what is measured, so
they come with a new methodology version.

### Timing

1. **T0** is pinned first: the latest block of the chain's RPC and its gas price.
2. Every aggregator is then asked at once: the requests are released together, so none gets a head start.
3. Each aggregator has **5 s** for its whole answer, both calls included for APIs that need two.
4. **Latency** is the time from sending a request to having read its answer, added up over the adapter's
   requests. Opening a connection (DNS, TCP, TLS) is ours and is left out. A timeout counts as 5 s.

### Statuses

Every aggregator gets exactly one status per round:

| Status | Meaning |
|---|---|
| `ok` | a transaction that passed the checks below |
| `no_route` | the aggregator says it cannot do the trade |
| `timeout` | no answer within 5 s |
| `error` | any other failure, including HTTP 429 |
| `invalid` | an answer that is not a usable transaction |

An answer is `invalid` when its output amount or transaction is malformed, when `tx.value` is not exactly the
amount sold for a native input or not 0 for an ERC-20 input, or when an ERC-20 input has no spender to approve.

### Our failures are never an aggregator's

If our side fails (the RPC, the token slots API, the database), the whole round is dropped and logged. No
aggregator is judged by a round in which the simulation itself could not be run.

## 2. Simulation

### 2.1 Mechanism

Every `ok` transaction is simulated with `eth_call` and state overrides. A full node is enough; no archive node
and no `eth_simulateV1` is needed.

A small contract, `contracts/SimulationProxy.sol`, is placed at the taker's address for the call. The call goes
from the taker to the taker, and the proxy calls the aggregator's `to` with its `data` and `value`. The router
therefore sees `msg.sender == tx.origin == taker`, as with a real wallet.

### 2.2 State

- **Input:** the taker holds exactly the amount sold, and for an ERC-20 has approved exactly that amount to the
  aggregator's spender. Both are set as storage overrides; the token slots API only says where the token keeps
  them. A token's slots are the same for every aggregator.
- **Native balance:** exactly the transaction's value plus what the call's gas costs.
- **Output:** what the taker's balance of the output token grew by. Event logs are not used. Fee-on-transfer and
  rebasing tokens show up as what a user really receives.
- For an ERC-20 output, the balance before the swap is read with a separate `eth_call` at the same block. Reading
  it inside the proxy would warm the token's storage and make the swap cheaper than a real transaction.
- Block number, timestamp and coinbase are what the node uses for the block. There are no block overrides.

### 2.3 Gas price and gas limit

Some liquidity pays more when it detects a simulation, and only there. The two signals an `eth_call` gets wrong
by default are set like in a real transaction:

- **Gas price:** the base fee of the T0 block plus the median of the last 20 blocks' median priority fees. On a
  chain without a base fee: the median of the gas prices paid.
- **Gas limit:** a probe at the chain's per-transaction gas cap measures the gas the swap uses. The measured run
  then gets 1.5 × that, at most the cap, plus a fixed allowance for the proxy's own work, so the router sees
  about the `gasleft()` it would see in a real transaction. A swap that fails the probe keeps the cap.

**Gas of a transaction:** what the swap would use as a transaction of its own. It is the gas the proxy measured
around its call, minus what the call costs that a transaction does not (2,600 for the cold target; with value,
another 6,700), plus 21,000 and the calldata gas (16 per non-zero byte, 4 per zero byte). Checked against a real
node: a USDC transfer through the proxy measures 62,240 gas, against the node's own `eth_estimateGas` of 62,772
(an estimate is a gas limit, slightly above the gas used).

### 2.4 T0 and T30

- **T0:** every `ok` transaction, at the block the aggregators were asked at.
- **T30:** every transaction that succeeded at T0, again: the same transaction with the same gas limit, at the
  latest block 30 s after T0, as if the user had signed it at T0 and it landed 30 s later. All T30 simulations of
  a round run at the same block.

### 2.5 Known limitations

- What depends on the block builder or the position in a block cannot be simulated. T30 is the backstop.
- Rollup L1 data fees are not counted. Since blobs (EIP-4844) they are far below 1 bp: measured on Base, a 2 KB
  swap's L1 fee was about 4% of its L2 gas cost. The calldata's L2 gas is counted.
- The taker is a contract during the simulation. Wallets with code (EIP-7702) are common, so a router cannot
  reject senders with code.

## 3. Ranking and metrics

All definitions are one SQL file, `internal/stats/metrics.sql`, with one row per quote. The leaderboard only adds
these rows up.

### 3.1 Net output

**net = output at T0 − gas cost**, in the output token.

**Gas cost** = gas of the transaction × T0 gas price, in the native coin, valued in the output token:

- 1:1 when the output token is the native coin or its wrapped token;
- otherwise at the price of the most recent round that sold the native coin (or its wrapped token) for the output
  token, or the output token for it, at most 6 hours before the round. That round's price is the median, over
  its successful T0 simulations, of output per input (or input per output). With an even count it is the lower
  of the two middle values;
- otherwise through the input token: the round's own rate (the same median of output per input) times the input
  token's price, found the same way. Every configured pair has a token that can be priced so.

The window is long because a chain gets only its share of the rounds; a price a few hours old moves the gas cost
of every aggregator in the round alike, and their ranking only by that price error times their difference in gas.

The benchmark makes these prices itself; no price feed is used.

### 3.2 Ranked rounds

A round is **ranked** when at least two of its quotes succeeded at T0 and its gas could be valued. Every
aggregator asked in a ranked round is judged by it, including those whose quote failed or never came.

### 3.3 Metrics

| Metric | Definition | Over |
|---|---|---|
| Win | share of ranked rounds with the highest net. Exact ties split the win, so the wins of a round add up to 1. | ranked rounds |
| Within 1 bp / 5 bp | share of ranked rounds where (best net − net) ≤ best net × 0.0001 / 0.0005, wins included | ranked rounds |
| Accuracy T0 | mean of min((output at T0 − quoted output) / quoted output, 0), in bp: how far the output fell short of the quote. 0 is best. | successful T0 simulations |
| Accuracy T30 | the same with the output at T30 | successful T30 simulations |
| Reverts | share of transactions that reverted at T0, or at T30 after succeeding at T0 | every transaction an aggregator returned |
| Latency | p50 and p90 of the latency (section 1), a timeout counting as 5 s | every request |
| Availability | share of requests answered with a transaction (status `ok`), whatever its simulations then did | every request |

Accuracy is a mean, not a median: a median stays at 0 as long as most quotes are exact, however far the others are
off. Each quote's accuracy is capped at 0: delivering more than quoted counts as exact. Otherwise an aggregator could
make up for over-promising with surplus it hands out on other quotes, for instance by not capturing positive
slippage for the benchmark, and an inaccurate quote would look accurate.

Every quote is counted in exactly one of the two failure metrics: an error, a timeout or no route (also on a chain
the aggregator does not serve) in availability, a transaction that reverts in reverts. Either way the aggregator
loses the round under win, so answering only easy trades does not pay.

The leaderboard shows win, accuracy T30, reverts and latency p50; the JSON API has every metric. By default it
covers all chains together: every round of every chain counts once. Since rounds are spread over the chains by
volume, this is the benchmark of the average aggregator trade. Every aggregator is judged by the rounds of every
chain, including the chains it does not serve. It can be narrowed to one chain, a period (24 h, 7 days, 30 days),
a size bucket and a collector region. The metrics are recomputed every 5 minutes.

## 4. Verification

- **Round explorer:** every round with every quote, transaction, simulation and revert reason, and how it ranked.
- **Export:** the leaderboard's quotes as CSV, one row each, with the numbers it ranked by. Anyone can recompute
  the leaderboard from it.
- **resim:** re-runs any stored round's simulations at its own blocks and reports every difference
  (`cmd/resim`; an archive node for rounds older than a full node's state).
- **Your own collector:** `docker compose up` runs the whole benchmark with your RPC, from your region.

The public database is written only by the operators' collectors. The operators also run the Nordstern
aggregator and the token slots API. Nordstern's adapter follows the same rules as every other one. A token's
storage slots are the same for every aggregator, so the slots API cannot favor one.

## Changelog

### 0.2 (2026-11-04)

- Several chains. A round's chain, pair and size are picked by their share of real aggregator volume (section 1.1),
  derived by `cmd/distribution`, instead of by hand-set weights with every size equally often.
- Size buckets $10 and $1m added.
- Every aggregator is asked on every chain; a chain it does not serve counts against it.
- Gas is valued with a price up to 6 hours old instead of 15 minutes, or through the input token (section 3.1).
- Accuracy is the mean instead of the median, each quote capped at 0. Reverts count reverts at T0 too;
  availability counts only answers without a transaction (errors, timeouts, no route), not reverts (section 3.3).

### 0.1 (2026-11-04)

First version.
