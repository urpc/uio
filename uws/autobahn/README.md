# Autobahn tests

This directory contains the reproducible Autobahn Testsuite setup used to
validate UWS against RFC 6455 and RFC 7692. Generated reports are intentionally
kept outside the repository because the complete HTML and JSON output is large.

## Latest result

The full suite was run on 2026-09-04 against UIO revision
`f0def13d9dcdc18ed2e36df6ec7e287e1021adc4`:

- 517 total cases
- 510 `OK`
- 4 `NON-STRICT`: `6.4.1` through `6.4.4`
- 3 `INFORMATIONAL`: `7.1.6`, `7.13.1`, and `7.13.2`
- 0 `FAILED` or `UNIMPLEMENTED`
- Compression enabled for all 216 RFC 7692 cases

The run took 1429 seconds using Go 1.26.2 on Linux 6.8.0 x86-64, Docker
28.3.3, and the pinned Autobahn image documented below.

## Run the suite

The supported validation environment is Linux with Go and Docker installed.
Docker host networking is required because the test client connects to the
UWS server on `127.0.0.1:19701`. Allow at least 8 GiB of free memory: the
Autobahn/PyPy client itself can use several GiB while generating and recording
the largest compression cases. A complete run can take tens of minutes; do
not treat quiet, CPU-active compression cases as a stalled test.

From the repository root, run:

```sh
./uws/autobahn/run.sh
```

The script builds the dedicated echo server, enables `permessage-deflate`,
runs every Autobahn case, and replaces the output only after the suite
completes. By default the report is written to
`${TMPDIR:-/tmp}/uws-autobahn-report`. Set `AUTOBAHN_REPORT_DIR` to an absolute
path when the report must be retained elsewhere:

```sh
AUTOBAHN_REPORT_DIR=/var/tmp/uws-report ./uws/autobahn/run.sh
```

The default image is pinned to
`crossbario/autobahn-testsuite@sha256:519915fb568b04c9383f70a1c405ae3ff44ab9e35835b085239c258b6fac3074`.
Override it with another pinned or locally mirrored image when needed:

```sh
AUTOBAHN_IMAGE=crossbario/autobahn-testsuite:0.8.2 ./uws/autobahn/run.sh
```

The image digest, UIO commit and tracked-source state, Go version, Linux
kernel, Docker version, and run time are written to the generated report
metadata. Neither the image nor generated report is stored in this repository.

## Reading results

- `OK` is a conforming result.
- `NON-STRICT` is accepted by Autobahn but indicates behavior more permissive
  than its strict recommendation.
- `INFORMATIONAL` cases report behavior without a pass/fail requirement.
- `UNCLEAN` means the TCP connection ended without completing the close
  handshake; inspect the individual case result before treating it as a
  protocol failure.
- `FAILED` is a conformance failure and must be investigated.
- `UNIMPLEMENTED` compression cases usually mean the server was run without
  `EnableCompression`; the supplied server always enables it.

The individual JSON files for invalid UTF-8 cases `6.3.1` and `6.20.1` through
`6.20.4` contain the deliberately malformed surrogate data recorded by
Autobahn. Strict JSON tools such as `jq` may reject those five wirelogs; use
the generated HTML or `index.json` for machine-readable result status.

Autobahn is a protocol conformance suite, not a load or resource-exhaustion
benchmark. UWS backpressure, slow-client, executor, and high-connection-count
behavior is covered separately by unit and Linux performance tests.
