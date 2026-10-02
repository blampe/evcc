# Optimizer goldens

- `cases/` are `test_cases/*.json` of github.com/evcc-io/optimizer (branch
  `blampe/optimizer-redesign`, e2b5f03).
- `golden/<case>.json` is the response the Python service returned for the case's request, and
  `mps.sha256` the SHA-256 of every MPS file PuLP handed to CBC while solving it, in solve order.
- `scenarios.sha256` holds the same hashes for the solves of the ported scenario tests
  (`tests/*.py`), keyed by the Python test id.

Recorded with pulp 3.3.2 and cbc-go's `cmd/cbc` (github.com/blampe/cbc, cedb61d) as the service's CBC,
through cbc-go's `scripts/pulp_shim.sh`, which keeps a copy of every model:

    go build -o /tmp/cbc-go ./cmd/cbc                       # in the cbc-go checkout
    mkdir -p /tmp/shim && cp scripts/pulp_shim.sh /tmp/shim/cbc
    cd <optimizer checkout>
    SCRATCH=/tmp CBC_SHIM_REAL=/tmp/cbc-go PATH=/tmp/shim:$PATH PYTHONPATH=src \
        python <evcc>/core/optimizer/testdata/record.py /tmp/py-cbcgo

`record.py` writes `<out>/<case>/response.json` and the captured models under `<out>/<case>/cap`.
