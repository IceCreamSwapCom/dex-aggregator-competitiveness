-- Every dashboard metric is defined here (METHODOLOGY.md, 3), as one row per quote;
-- the leaderboard (stats.go) only adds those rows up.
--
-- A round's metrics depend only on the round and the rounds that started before it,
-- and stored rounds never change. So each round is settled once: settle_metrics adds
-- its rows when every round that started before it is in the database. The collectors
-- call it after every round they write (stats.Settle).
--
-- stats.Create runs this file when it differs from the one that settled the stored
-- metrics, after dropping them: then every round is settled anew by this file.
--
-- Amounts are tokenOut base units, gas prices wei.

-- A round that sold the native coin for token X, or X for it, prices X: the
-- median over its successful T0 simulations of X base units per wei. Wrapped
-- native counts as native.
CREATE TABLE round_prices (
  round_id   uuid PRIMARY KEY,
  chain_id   bigint NOT NULL,
  token      text NOT NULL,
  started_at timestamptz NOT NULL,
  per_wei    numeric NOT NULL
);
CREATE INDEX round_prices_latest ON round_prices (chain_id, token, started_at, round_id);

CREATE TABLE quote_metrics (
  round_id         uuid NOT NULL,
  aggregator       text NOT NULL,
  chain_id         bigint NOT NULL,
  region           text NOT NULL,
  started_at       timestamptz NOT NULL,
  size_bucket      text NOT NULL,
  status           text NOT NULL,
  latency_ms       int NOT NULL,
  t0_success       boolean,
  t30_success      boolean,
  gas_cost         numeric,  -- NULL when the gas could not be valued
  net              numeric,  -- NULL unless the T0 simulation succeeded
  ranked           boolean NOT NULL,
  win_share        numeric,  -- this and the next two are NULL unless ranked
  within_1bp       boolean,
  within_5bp       boolean,
  accuracy_t0_bps  float8,
  accuracy_t30_bps float8,
  PRIMARY KEY (round_id, aggregator)
);
CREATE INDEX quote_metrics_chain_started ON quote_metrics (chain_id, started_at);

-- settle_metrics settles the rounds that started before until and are not settled
-- yet. Settles run one at a time: the advisory lock is stats.Create's as well.
CREATE FUNCTION settle_metrics(until timestamptz) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  since timestamptz; -- every round that started before since is settled
BEGIN
  PERFORM pg_advisory_xact_lock(8453002);
  SELECT settled_before INTO STRICT since FROM metrics_state;
  IF until <= since THEN
    RETURN;
  END IF;

  INSERT INTO round_prices (round_id, chain_id, token, started_at, per_wei)
  SELECT r.id, r.chain_id, CASE WHEN r.token_in_native THEN r.token_out ELSE r.token_in END, r.started_at,
         percentile_disc(0.5) WITHIN GROUP (ORDER BY
           CASE WHEN r.token_in_native THEN q.t0_out / r.amount_in ELSE r.amount_in / q.t0_out END)
  FROM rounds r JOIN quotes q ON q.round_id = r.id
  WHERE r.started_at >= since AND r.started_at < until
    AND r.token_in_native <> r.token_out_native AND q.t0_success AND q.t0_out > 0
  GROUP BY r.id;

  INSERT INTO quote_metrics (round_id, aggregator, chain_id, region, started_at, size_bucket, status, latency_ms,
                             t0_success, t30_success, gas_cost, net, ranked, win_share, within_1bp, within_5bp,
                             accuracy_t0_bps, accuracy_t30_bps)
  WITH
  -- Gas is valued in tokenOut: 1:1 when tokenOut counts as native; otherwise at
  -- the price of the most recent round that priced tokenOut, at most 6 hours
  -- before; otherwise through tokenIn: the round's own rate (the median over its
  -- successful T0 simulations of tokenOut base units per tokenIn base unit) times
  -- the most recent price of tokenIn. NULL when there is none: the round is then
  -- not ranked.
  rounds_priced AS MATERIALIZED (
    SELECT r.*,
           CASE WHEN r.token_out_native THEN 1::numeric ELSE coalesce(
             (SELECT p.per_wei FROM round_prices p
              WHERE p.chain_id = r.chain_id AND p.token = r.token_out
                AND p.started_at <= r.started_at AND p.started_at > r.started_at - interval '6 hours'
              ORDER BY p.started_at DESC, p.round_id DESC LIMIT 1),
             (SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY q.t0_out / r.amount_in)
              FROM quotes q WHERE q.round_id = r.id AND q.t0_success AND q.t0_out > 0) *
             CASE WHEN r.token_in_native THEN 1::numeric ELSE (
               SELECT p.per_wei FROM round_prices p
               WHERE p.chain_id = r.chain_id AND p.token = r.token_in
                 AND p.started_at <= r.started_at AND p.started_at > r.started_at - interval '6 hours'
               ORDER BY p.started_at DESC, p.round_id DESC LIMIT 1) END
           ) END AS out_per_wei
    FROM rounds r
    WHERE r.started_at >= since AND r.started_at < until
  ),
  -- net = what the taker receives at T0 minus what the transaction's gas costs, in tokenOut.
  nets AS (
    SELECT q.round_id, q.aggregator, r.chain_id, r.region, r.started_at, r.size_bucket,
           q.status, q.latency_ms, q.t0_success, q.t30_success,
           r.out_per_wei IS NOT NULL AS priced,
           q.t0_gas * r.t0_gas_price * r.out_per_wei AS gas_cost,
           CASE WHEN q.t0_success THEN q.t0_out - q.t0_gas * r.t0_gas_price * r.out_per_wei END AS net,
           -- Accuracy is capped at 0: delivering more than quoted counts as exact, so that
           -- surplus on some quotes cannot make up for over-promising on others.
           CASE WHEN q.t0_success AND q.quoted_out > 0
             THEN least((q.t0_out - q.quoted_out) / q.quoted_out * 10000, 0)::float8 END AS accuracy_t0_bps,
           CASE WHEN q.t30_success AND q.quoted_out > 0
             THEN least((q.t30_out - q.quoted_out) / q.quoted_out * 10000, 0)::float8 END AS accuracy_t30_bps
    FROM quotes q JOIN rounds_priced r ON r.id = q.round_id
  ),
  best AS (
    SELECT n.*, max(net) OVER r AS best_net, count(net) OVER r AS successes
    FROM nets n WINDOW r AS (PARTITION BY round_id)
  ),
  -- A round is ranked when its gas could be valued and at least two of its
  -- quotes succeeded at T0. Every aggregator asked in a ranked round is judged
  -- by it, including those without a successful quote.
  ranked AS (
    SELECT b.*, priced AND successes >= 2 AND best_net > 0 AS ranked,
           count(*) FILTER (WHERE net = best_net) OVER (PARTITION BY round_id) AS winners
    FROM best b
  )
  SELECT round_id, aggregator, chain_id, region, started_at, size_bucket, status, latency_ms,
         t0_success, t30_success, gas_cost, net, ranked,
         -- Exact ties split the win, so the win shares of a round add up to 1.
         CASE WHEN ranked THEN CASE WHEN net = best_net THEN 1.0 / winners ELSE 0 END END,
         CASE WHEN ranked THEN coalesce(best_net - net <= best_net * 0.0001, false) END,
         CASE WHEN ranked THEN coalesce(best_net - net <= best_net * 0.0005, false) END,
         accuracy_t0_bps, accuracy_t30_bps
  FROM ranked;

  UPDATE metrics_state SET settled_before = until;
END
$$;
