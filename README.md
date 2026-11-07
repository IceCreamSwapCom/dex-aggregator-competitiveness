# DEX Aggregator Competitiveness

An open-source benchmark of DEX aggregators. A collector asks every aggregator for the same trade at the same
moment. It simulates every answer with `eth_call` at the block they were asked at (T0), and the same
transactions again at the head block 30 s later (T30). All of it is stored in Postgres. The results are
public at https://benchmark.nordstern.finance.

Every rule it measures by is in [METHODOLOGY.md](METHODOLOGY.md); the dashboard's `/methodology` is its short
version and links it.

> Disclosure: the operators of this benchmark also run the Nordstern aggregator and the token slots API.
> Nordstern's adapter uses the same public API and rules as every other one; every aggregator is asked the way
> that gets its best offer ([METHODOLOGY.md](METHODOLOGY.md), section 1).

## Run

Needs Go 1.26+ and a Postgres the collector may create its tables in.

```sh
REGION=local \
DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres \
SLOTS_API_URL=https://api.nordstern.finance/slots \
RPC_8453=https://mainnet.base.org RPC_1=… RPC_56=… RPC_4663=… RPC_137=… RPC_42161=… RPC_43114=… \
go run ./cmd/collector
```

| Variable | |
|---|---|
| `REGION` | label on every round this collector writes |
| `DATABASE_URL` | Postgres; the collector exits when it cannot write a round |
| `SLOTS_API_URL` | token slots API, called as `{SLOTS_API_URL}/{chainId}?token=&holderAddress=&spenderAddress=` |
| `RPC_<chainId>` | one per chain in the config; a full node is enough, it needs `eth_call` with state overrides |
| `ZEROX_API_KEY`, `LIFI_API_KEY`, `ENSO_API_KEY` | API keys of 0x, LI.FI and Enso; required while the config lists them. In docker compose, put them in a `.env` file next to `docker-compose.yml` (git ignores it) |

`config/benchmark.json` is the benchmark itself: the aggregators (asked on every chain), the chains, tokens,
pairs and sizes with their weights, timeouts, slippage and the gas limit rule. It is public on purpose. Change it
through a pull request.

### Derive the weights: distribution

The chains, pairs and sizes are weighted by what users really trade (METHODOLOGY.md, 1.1). `distribution`
derives them from public data only: DefiLlama's 30-day aggregator volume per chain, and 7 days of the swap events
the LI.FI and KyberSwap routers log on each chain, read with `eth_getLogs`.

```sh
RPC_1=… RPC_56=… RPC_8453=… RPC_4663=… RPC_137=… RPC_42161=… RPC_43114=… \
SLOTS_API_URL=https://api.nordstern.finance/slots \
go run ./cmd/distribution -write          # or -reproduce: the blocks the config records
```

It keeps the config's chains and the names of its tokens; a new chain needs an entry with its native coin, its
wrapped native token and a stablecoin to price trades in. It prints what it left out and why.

The RPCs must serve `eth_getLogs` for the whole week. Not every free node does, and some return nothing instead of
an error: the shipped config was derived with `gateway.tenderly.co/public/mainnet` (Ethereum),
`public-bsc-mainnet.fastnode.io` (BSC), `arb1.arbitrum.io/rpc` (Arbitrum), `rpc.mainnet.chain.robinhood.com` and
publicnode for the others. Each chain records how many trades it read, and `-reproduce` warns when a node returns
a different number. `-chain <id>` derives one chain again and keeps the others.

The dashboard and its JSON API read the same database (`DATABASE_URL`, and `ADDR`, default `:8080`):

```sh
DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres go run ./cmd/web
```

| Path | |
|---|---|
| `/` | leaderboard of all chains or one, by period, size and region: best rate, quote accuracy (T30), revert rate and latency (p50) |
| `/aggregators/{id}` | one aggregator: its place in each column, its metrics by trade size and chain, its answers, failures and latest rounds |
| `/rounds`, `/rounds/{id}` | round explorer: every quote, simulation and transaction, and the `resim` command for the round |
| `/methodology` | the methodology in short; it links METHODOLOGY.md |
| `GET /api/v1/leaderboard?chain=&period=&size=&region=` | the leaderboard, of all chains unless `chain` names one; `period` is `24h`, `7d` (default) or `30d` |
| `GET /api/v1/export.csv?chain=&period=&size=&region=&aggregator=` | the quotes behind that leaderboard (or one aggregator's) as CSV, one row each, with gas cost, net and win share; linked under the leaderboard and on each aggregator page. Two exports stream at a time; a third gets `429` and `Retry-After` |
| `GET /api/v1/rounds?chain=&before=&limit=` | rounds that started before `before` (RFC 3339, default now), newest first; `limit` 1–1000, default 100 |
| `GET /api/v1/rounds/{id}` | one round with every quote, transaction and simulation, in the form of `internal/api` |

Every metric is defined in one file, [internal/stats/metrics.sql](internal/stats/metrics.sql): a materialized
view with one row per quote that `web` recreates at start and refreshes every 5 minutes. The leaderboard only
adds its rows up. Periods count back from the last refresh, and `web` keeps every answer until the next one
([internal/web/guard.go](internal/web/guard.go)).

### Your own copy

Our collectors run in three regions: `eu-central` (Helsinki), `us-east` (Virginia) and `ap-southeast`
(Singapore). They run the image `ghcr.io/icecreamswapcom/dex-aggregator-competitiveness`, which
[CI](.github/workflows/ci.yml) builds from this repository, and each round stores the commit it was built from.

`docker compose up --build` runs Postgres, a collector and the dashboard on http://localhost:8080. It uses free
public RPCs for the configured chains unless you set your own `RPC_<chainId>`; a chain you add to the config needs
one. Run it to check our data with your own collector, from your own region.

## Check a round: resim

`resim` re-runs a stored round's simulations at the round's own blocks and prints every one that comes out
differently. It needs no database: it reads the round from the round API, or from a JSON file in the same form.

```sh
RPC_8453=https://… SLOTS_API_URL=https://api.nordstern.finance/slots \
go run ./cmd/resim -api http://localhost:8080 <round id>...
```

The node has to have the state of the round's blocks. A full node keeps only the last ~128 blocks, so older
rounds need an archive node. T0 finds the gas limit again from a probe. T30 runs with the stored limit, the
same way the collector ran it. Exit status: 0 if everything matches, 1 if something differs, 2 on errors.

## How a round works

1. Pick a chain, then a pair and a size bucket, at random by the config's weights (their share of real
   volume), and a direction. Jitter the amount by ×[0.8, 1.25] and draw a fresh random taker address.
2. Pin T0 with one `eth_feeHistory` call: the latest block and its gas price (base fee + median tip of the last
   20 blocks).
3. Ask every aggregator at once, with the same request and a 5 s timeout. Every aggregator is asked on every
   chain; one that does not serve the chain fails, and that counts against it.
4. Simulate every usable answer at T0:
   1. a probe at the chain's per-transaction gas cap gives the gas the swap uses;
   2. the measured run uses a wallet-like gas limit of 1.5× that;
5. 30 s after T0, pin T30 and simulate the same transactions with the same gas limits.
6. Write the round in one transaction.

If an RPC or slots API call fails, the whole round is dropped: our failures never count against an aggregator.

A round makes 3 + 3N RPC calls for N usable quotes (18 for 5 aggregators):
- two pins;
- one balance read of the output token (none for native output);
- per quote: a probe, the measured run and the T30 run.

The simulation (`internal/sim`) places `contracts/SimulationProxy.sol` at the taker's address with a state
override. That way the swap runs with `msg.sender == tx.origin == taker`, and the output is measured as the
taker's balance change. The taker holds exactly the amount it sells and approves exactly that amount.

## Test

```sh
go test ./...                     # no network
go vet ./... && gofmt -l .
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres go test ./internal/store ./internal/stats
RPC_8453=https://mainnet.base.org SLOTS_API_URL=https://api.nordstern.finance/slots go test -tags live ./internal/sim
sh contracts/build.sh             # rebuilds the proxy bytecode with solc 0.8.26 (Docker)
```

The live test checks the gas accounting against a real node. It simulates a USDC transfer through the proxy
and compares the result with the node's own `eth_estimateGas` for the same transfer sent from a plain account.

## Add an aggregator

An adapter is a package in `internal/aggregator/` that implements `aggregator.Aggregator`, registered in
`cmd/collector/aggregators.go` and listed in `config/benchmark.json`. Open a pull request with it.

- It calls the aggregator's public, documented API. An API key, integrator id or Referer that gets the
  aggregator's best offer is allowed; it is set in `aggregators.go`, where everyone can see it.
- It sends the request of METHODOLOGY.md, section 1, and nothing else non-default without a comment saying why.
  If the API can skip its balance checks (the taker holds nothing until the simulation), it does.
- It returns the quote or a classified `*aggregator.Error`, and never retries.
- Its tests answer from fixtures cut from real responses (`internal/aggregator/aggtest`): a quote, no route and
  an error.

## Layout

```
cmd/collector/          startup checks, scheduler; aggregators.go lists the adapters
cmd/web/                dashboard and JSON API server; refreshes the metrics
cmd/resim/              standalone re-simulation of stored rounds
cmd/distribution/       derives the chain, pair and size weights from router events
METHODOLOGY.md          every rule, with its changelog; a config test checks it describes the configured version
config/benchmark.json   the benchmark
contracts/              SimulationProxy.sol and its build
internal/aggregator/    the adapter interface and one package per aggregator
internal/api/           the public JSON: rounds and the leaderboard
internal/config/        loads and checks config/benchmark.json
internal/round/         one round, start to finish; Swaps is shared with resim
internal/sim/           eth_call simulation, gas price and gas limit rules
internal/rpc/           minimal JSON-RPC client
internal/slots/         token slots API client
internal/store/         schema.sql, the round writer and reader
internal/upstream/      the shared HTTP client for aggregator APIs
internal/stats/         metrics.sql (every metric) and the leaderboard queries
internal/web/           pages, templates and API handlers
docker-compose.yml      Postgres + collector + web, for your own copy
.github/workflows/ci.yml  checks, and builds and publishes the image
```

## License

[PolyForm Noncommercial 1.0.0](LICENSE.md): free to use, change and share for any noncommercial purpose, such as
checking the results, research or running your own copy. Commercial use needs a separate license from us.

The aggregators' logos in `internal/web/static/logos` belong to their owners and only name them in the ranking.
Each comes from the aggregator's own website, cut to a square tile: 0x (0x.org), KyberSwap (kyberswap.com),
Velora (velora.xyz), Sushi (sushi.com), Fly (fly.trade), LI.FI (li.fi), Enso (enso.build) and Nordstern
(nordstern.finance). An aggregator without a logo is shown with its initial.
