-- Every dashboard metric is defined here (METHODOLOGY.md, 3), as one row per quote;
-- the leaderboard (stats.go) only adds those rows up. web runs this file at
-- every start and refreshes the view every 5 minutes.
--
-- Amounts are tokenOut base units, gas prices wei.

-- One web at a time: the file runs as one batch, in one transaction.
SELECT pg_advisory_xact_lock(8453002);

DROP MATERIALIZED VIEW IF EXISTS quote_metrics;

CREATE MATERIALIZED VIEW quote_metrics AS
WITH
-- A round that sold the native coin for token X, or X for it, prices X: the
-- median over its successful T0 simulations of X base units per wei. Wrapped
-- native counts as native.
prices AS (
  SELECT r.id, r.chain_id, r.started_at,
         CASE WHEN r.token_in_native THEN r.token_out ELSE r.token_in END AS token,
         percentile_disc(0.5) WITHIN GROUP (ORDER BY
           CASE WHEN r.token_in_native THEN q.t0_out / r.amount_in ELSE r.amount_in / q.t0_out END) AS per_wei
  FROM rounds r JOIN quotes q ON q.round_id = r.id
  WHERE r.token_in_native <> r.token_out_native AND q.t0_success AND q.t0_out > 0
  GROUP BY r.id
),
-- What the round's own quotes sold tokenIn for: the median over its
-- successful T0 simulations of tokenOut base units per tokenIn base unit.
rates AS (
  SELECT q.round_id, percentile_disc(0.5) WITHIN GROUP (ORDER BY q.t0_out / r.amount_in) AS out_per_in
  FROM rounds r JOIN quotes q ON q.round_id = r.id
  WHERE q.t0_success AND q.t0_out > 0
  GROUP BY q.round_id
),
-- Gas is valued in tokenOut: 1:1 when tokenOut counts as native; otherwise at
-- the price of the most recent round that priced tokenOut, at most 6 hours
-- before; otherwise through tokenIn: the round's own rate times the most recent
-- price of tokenIn. NULL when there is none: the round is then not ranked.
rounds_priced AS (
  SELECT r.*,
         CASE WHEN r.token_out_native THEN 1::numeric ELSE coalesce(
           (SELECT p.per_wei FROM prices p
            WHERE p.chain_id = r.chain_id AND p.token = r.token_out
              AND p.started_at <= r.started_at AND p.started_at > r.started_at - interval '6 hours'
            ORDER BY p.started_at DESC LIMIT 1),
           (SELECT a.out_per_in FROM rates a WHERE a.round_id = r.id) *
           CASE WHEN r.token_in_native THEN 1::numeric ELSE (
             SELECT p.per_wei FROM prices p
             WHERE p.chain_id = r.chain_id AND p.token = r.token_in
               AND p.started_at <= r.started_at AND p.started_at > r.started_at - interval '6 hours'
             ORDER BY p.started_at DESC LIMIT 1) END
         ) END AS out_per_wei
  FROM rounds r
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
       CASE WHEN ranked THEN CASE WHEN net = best_net THEN 1.0 / winners ELSE 0 END END AS win_share,
       CASE WHEN ranked THEN coalesce(best_net - net <= best_net * 0.0001, false) END AS within_1bp,
       CASE WHEN ranked THEN coalesce(best_net - net <= best_net * 0.0005, false) END AS within_5bp,
       accuracy_t0_bps, accuracy_t30_bps
FROM ranked;

-- REFRESH ... CONCURRENTLY needs a unique index.
CREATE UNIQUE INDEX quote_metrics_key ON quote_metrics (round_id, aggregator);
CREATE INDEX quote_metrics_chain_started ON quote_metrics (chain_id, started_at);
