#!/bin/sh
# Compiles SimulationProxy.sol with the pinned compiler and writes its runtime
# bytecode to SimulationProxy.bin-runtime. go test ./internal/sim checks that
# file against the bytecode the simulation uses.
set -eu
cd "$(dirname "$0")"
docker run --rm -v "$PWD:/src" -w /src ethereum/solc:0.8.26 \
  --optimize --optimize-runs 200 --evm-version paris --metadata-hash none \
  --bin-runtime SimulationProxy.sol | tail -n 1 > SimulationProxy.bin-runtime
echo "wrote contracts/SimulationProxy.bin-runtime"
