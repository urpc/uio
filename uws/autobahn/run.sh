#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd)
CONFIG_FILE="$SCRIPT_DIR/config/fuzzingclient.json"
REPORT_DIR=${AUTOBAHN_REPORT_DIR:-${TMPDIR:-/tmp}/uws-autobahn-report}
AUTOBAHN_IMAGE=${AUTOBAHN_IMAGE:-crossbario/autobahn-testsuite@sha256:519915fb568b04c9383f70a1c405ae3ff44ab9e35835b085239c258b6fac3074}

if [[ $(uname -s) != Linux ]]; then
	echo "Autobahn validation requires Linux Docker host networking" >&2
	exit 1
fi
if [[ "$REPORT_DIR" != /* ]]; then
	echo "AUTOBAHN_REPORT_DIR must be an absolute path" >&2
	exit 1
fi
for command in go git docker; do
	if ! command -v "$command" >/dev/null 2>&1; then
		echo "Required command not found: $command" >&2
		exit 1
	fi
done
START_TIME=$(date +%s)

BUILD_DIR=
REPORT_STAGE=
REPORT_BACKUP=
SERVER_PID=

cleanup() {
	if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
		kill "$SERVER_PID" 2>/dev/null || true
		wait "$SERVER_PID" 2>/dev/null || true
	fi
	if [[ -n "$BUILD_DIR" ]]; then
		rm -rf -- "$BUILD_DIR"
	fi
	if [[ -n "$REPORT_STAGE" ]]; then
		rm -rf -- "$REPORT_STAGE"
	fi
	if [[ -n "$REPORT_BACKUP" ]]; then
		if [[ -e "$REPORT_BACKUP/report" && ! -e "$REPORT_DIR" ]]; then
			mv -- "$REPORT_BACKUP/report" "$REPORT_DIR"
		fi
		rm -rf -- "$REPORT_BACKUP"
	fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

BUILD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/uws-autobahn-build.XXXXXX")
REPORT_STAGE=$(mktemp -d "${TMPDIR:-/tmp}/uws-autobahn-report-stage.XXXXXX")

cd "$REPO_ROOT"
go build -o "$BUILD_DIR/uws-autobahn-server" ./uws/autobahn/server
if (exec 3<>/dev/tcp/127.0.0.1/19701) 2>/dev/null; then
	echo "Port 19701 is already in use" >&2
	exit 1
fi
"$BUILD_DIR/uws-autobahn-server" >"$BUILD_DIR/server.log" 2>&1 &
SERVER_PID=$!

SERVER_READY=false
for ((attempt = 0; attempt < 100; attempt++)); do
	if ! kill -0 "$SERVER_PID" 2>/dev/null; then
		cat "$BUILD_DIR/server.log" >&2
		exit 1
	fi
	if (exec 3<>/dev/tcp/127.0.0.1/19701) 2>/dev/null; then
		SERVER_READY=true
		break
	fi
	sleep 0.1
done
if [[ $SERVER_READY != true ]]; then
	echo "Autobahn server did not listen on 127.0.0.1:19701" >&2
	exit 1
fi

docker run --rm --network host \
	--mount "type=bind,src=$CONFIG_FILE,dst=/config/fuzzingclient.json,readonly" \
	--mount "type=bind,src=$REPORT_STAGE,dst=/reports" \
	"$AUTOBAHN_IMAGE" \
	wstest -m fuzzingclient -s /config/fuzzingclient.json
if [[ ! -s "$REPORT_STAGE/index.json" ]]; then
	echo "Autobahn did not produce a complete index.json" >&2
	exit 1
fi

IMAGE_ID=$(docker image inspect "$AUTOBAHN_IMAGE" --format '{{.Id}}')
COMMIT=$(git rev-parse HEAD)
SOURCE_STATE=clean
if ! git diff --quiet HEAD --; then
	SOURCE_STATE="tracked changes present"
fi
GO_VERSION=$(go version)
DOCKER_VERSION=$(docker version --format '{{.Server.Version}}')
KERNEL=$(uname -srvmo)
RUN_DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
DURATION_SECONDS=$(($(date +%s) - START_TIME))
cat >"$REPORT_STAGE/METADATA.md" <<EOF
# Autobahn report metadata

- UIO commit: \`$COMMIT\`
- Source state: \`$SOURCE_STATE\`
- Generated: \`$RUN_DATE\`
- Duration: \`$DURATION_SECONDS seconds\`
- Go: \`$GO_VERSION\`
- Kernel: \`$KERNEL\`
- Docker server: \`$DOCKER_VERSION\`
- Autobahn image: \`$AUTOBAHN_IMAGE\`
- Image ID: \`$IMAGE_ID\`
- Compression: enabled
- Cases: all
EOF

mkdir -p "$(dirname -- "$REPORT_DIR")"
REPORT_BACKUP=$(mktemp -d "${TMPDIR:-/tmp}/uws-autobahn-report-backup.XXXXXX")
if [[ -e "$REPORT_DIR" ]]; then
	mv -- "$REPORT_DIR" "$REPORT_BACKUP/report"
fi
if ! mv -- "$REPORT_STAGE" "$REPORT_DIR"; then
	if [[ -e "$REPORT_BACKUP/report" ]]; then
		mv -- "$REPORT_BACKUP/report" "$REPORT_DIR"
	fi
	exit 1
fi
REPORT_STAGE=
rm -rf -- "$REPORT_BACKUP"
REPORT_BACKUP=

echo "Autobahn report: $REPORT_DIR/index.html"
