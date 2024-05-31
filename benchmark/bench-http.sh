#!/bin/bash

program_name=$0

function print_usage {
    echo ""
    echo "Usage: $program_name [connections] [duration]"
    echo ""
    echo "connections:  Connections to keep open to the destinations"
    echo "duration:     Exit after the specified amount of time"
    echo ""
    echo "--- EXAMPLE ---"
    echo ""
    echo "$program_name 1000 30"
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

server_cpu_num=8
client_cpu_num=8

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

eval "$(pkill -9 http-std || printf "")"
eval "$(pkill -9 http-ghttp || printf "")"
eval "$(pkill -9 http-nbio || printf "")"
eval "$(pkill -9 http-uio || printf "")"

conn_num=$1
test_duration=$2
#packet_size=$3
#packet=$(LC_ALL=C bash -c "< /dev/urandom tr -dc a-zA-Z0-9 | fold -w $packet_size | head -n 1")

#echo "--- ECHO PACKET ---"
#echo "$packet"
#echo ""

function go_bench() {
  echo "--- HTTP BENCH ---"
  echo "--- $1 ---"
  echo ""

  echo "compiling server...$1"
  go build -gcflags="-l=4" -ldflags="-s -w" -o "$2" "$3"

  echo "starting server...$1"
  $limit_cpu_server $2 --port "$4" --loops "$5" &

  # waiting for server startup...
  sleep 1

  echo "--- BENCHMARK START ---"
  printf "*** %d connections, %d seconds, server cpu core: 0-%d, client cpu core: %d-%d\n" "$conn_num" "$test_duration" "$server_cpu_num" "$client_cpu_num" "$((total_cpu_num - 1))"
  echo ""
  
  $limit_cpu_client wrk -H 'Host: 127.0.0.1' -H 'Accept: text/plain,text/html;q=0.9,application/xhtml+xml;q=0.9,application/xml;q=0.8,*/*;q=0.7' -H 'Connection: keep-alive' --latency -d $test_duration -c $conn_num -t $client_cpu_num http://127.0.0.1:$4/
  echo ""
  echo "--- BENCHMARK DONE ---"
  echo ""

  echo "kill server...$1"
  eval "$(pkill -9 "$1" || printf "")"

  # waiting for server cleanup...
  sleep 1
}

go_bench "http-std" bin/http-std-server http-std.go 7001 $server_cpu_num
go_bench "http-ghttp" bin/http-ghttp-server http-ghttp.go 7002 $server_cpu_num
go_bench "http-nbio" bin/http-nbio-server http-nbio.go 7003 $server_cpu_num
go_bench "http-uio" bin/http-uio-server http-uio.go 7004 $server_cpu_num
