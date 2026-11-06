-- Applied by the collector and web at every start; every statement is idempotent.
-- Amounts are base units, gas prices wei. Addresses are lower-case 0x hex.

-- The file runs as one statement batch, and so in one transaction: this lock makes a
-- collector and web that start together create the tables one after the other.
SELECT pg_advisory_xact_lock(8453001);

CREATE TABLE IF NOT EXISTS rounds (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  region              text NOT NULL,
  chain_id            bigint NOT NULL,
  token_in            text NOT NULL,
  token_out           text NOT NULL,
  token_in_native     boolean NOT NULL,     -- the native coin or its wrapped token (gas cost conversion)
  token_out_native    boolean NOT NULL,
  amount_in           numeric(78) NOT NULL,
  size_bucket         text NOT NULL,
  slippage_bps        int NOT NULL,
  taker               text NOT NULL,
  started_at          timestamptz NOT NULL,
  t0_block            bigint NOT NULL,      -- the block the aggregators were asked at
  t0_gas_price        numeric(78) NOT NULL,
  t30_block           bigint NOT NULL,      -- the head block staleDelay later
  t30_gas_price       numeric(78) NOT NULL,
  methodology_version text NOT NULL,
  collector_version   text NOT NULL
);

CREATE INDEX IF NOT EXISTS rounds_chain_started ON rounds (chain_id, started_at);

CREATE TABLE IF NOT EXISTS quotes (
  round_id      uuid NOT NULL REFERENCES rounds,
  aggregator    text NOT NULL,
  status        text NOT NULL,  -- ok | no_route | timeout | error | invalid
  error         text,
  latency_ms    int NOT NULL,
  quoted_out    numeric(78),
  tx_to         text,
  tx_data       text,
  tx_value      numeric(78),
  spender       text,
  raw_response  jsonb,
  gas_limit     bigint,         -- of the measured runs, T0 and T30
  -- the simulations; NULL = not simulated
  t0_success    boolean,
  t0_out        numeric(78),
  t0_gas        bigint,         -- as its own transaction: execution + 21000 + calldata
  t0_revert     text,
  t30_success   boolean,
  t30_out       numeric(78),
  t30_gas       bigint,
  t30_revert    text,
  PRIMARY KEY (round_id, aggregator)
);
