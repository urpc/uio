#!/bin/bash

program_name=$0

function print_usage {
    echo ""
    echo "Usage: $program_name [connections] [duration] [size]"
    echo ""
    echo "connections:  Connections to keep open to the destinations"
    echo "duration:     Exit after the specified amount of time"
    echo "size:         single packet size for benchmark"
    echo ""
    echo "--- EXAMPLE ---"
    echo ""
    echo "$program_name 1000 30 1024"
    echo ""
    exit 1
}

# if less than two arguments supplied, display usage
if [  $# -le 1 ]; then
  print_usage
  exit 1
fi

# check whether user had supplied -h or --help . If yes display usage
if [[ ( $# == "--help") ||  $# == "-h" ]]; then
  print_usage
  exit 0
fi

set -e

if [ $(which taskset) ]; then
    total_cpu_num=$(getconf _NPROCESSORS_ONLN)
    #server_cpu_num=$((total_cpu_num >= 16 ? 7 : total_cpu_num / 2 - 1))
    server_cpu_num=$((total_cpu_num / 2 - 1))
    client_cpu_num=$((server_cpu_num + 1))
    limit_cpu_server="taskset -c 0-${server_cpu_num}"
    limit_cpu_client="taskset -c ${client_cpu_num}-$((total_cpu_num - 1))"
fi

echo ""
echo "--- BENCH ECHO START ---"
echo ""

cd "$(dirname "${BASH_SOURCE[0]}")"
function cleanup() {
  echo "--- BENCH ECHO DONE ---"
  # shellcheck disable=SC2046
  kill -9 $(jobs -rp)
  # shellcheck disable=SC2046
  wait $(jobs -rp) 2>/dev/null
}
trap cleanup EXIT

mkdir -p bin

eval "$(pkill -9 echo-evio || printf "")"
eval "$(pkill -9 echo-gnet || printf "")"
eval "$(pkill -9 echo-uio || printf "")"
eval "$(pkill -9 echo-nbio || printf "")"

conn_num=$1
test_duration=$2
packet_size=$3
packet=$(LC_ALL=C bash -c "< /dev/urandom tr -dc a-zA-Z0-9 | fold -w $packet_size | head -n 1")

echo "--- ECHO PACKET ---"
echo "$packet"
echo ""

function go_bench() {
  echo "--- $1 ---"
  echo ""
  if [[ "$1" == "GNET" ]]; then
    go build -tags=poll_opt -gcflags="-l=4" -ldflags="-s -w" -o "$2" "$3"
  else
    go build -gcflags="-l=4" -ldflags="-s -w" -o "$2" "$3"
  fi

  $limit_cpu_server $2 --port "$4" --loops "$5" &

  # waiting for server startup...
  sleep 1

  echo "--- BENCHMARK START ---"
  printf "*** %d connections, %d seconds, packet size: %d bytes, server cpu core: 0-%d, client cpu core: %d-%d\n" "$conn_num" "$test_duration" "$packet_size" "$server_cpu_num" "${client_cpu_num}" "$((total_cpu_num - 1))"
  echo ""
  
  $limit_cpu_client tcpkali --workers "$client_cpu_num" --connections "$conn_num" --connect-rate "$conn_num" --duration "$test_duration"'s' -m "$packet" 127.0.0.1:"$4"
  echo ""
  echo "--- BENCHMARK DONE ---"
  echo ""
}

go_bench "EVIO" bin/echo-evio-server echo-evio.go 7001 8
go_bench "GNET" bin/echo-gnet-server echo-gnet.go 7002 8
go_bench "NBIO" bin/echo-nbio-server echo-nbio.go 7003 8
go_bench "UIO" bin/echo-uio-server echo-uio.go 7004 8
