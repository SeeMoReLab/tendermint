#!/usr/bin/env bash
set -e

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
TESTNET_DIR="$REPO_DIR/mytestnet"
TENDERMINT="$REPO_DIR/build/tendermint"
MAVERICK="$REPO_DIR/build/maverick"
DELAY_SCHEDULE="$REPO_DIR/delay_schedule.json"
LOAD_DURATION=15
LOAD_RATE=200
LOAD_CONNECTIONS=1
LOAD_SIZE=500
EPOCH_SIZE=1000

# ── Parse arguments ───────────────────────────────────────────────────────────
usage() {
  echo "Usage: $0 <n> <f>"
  echo "  n  total number of nodes"
  echo "  f  number of maverick nodes (indexes 0..f-1); remaining n-f are normal"
  exit 1
}

[ "$#" -eq 2 ] || usage
N="$1"
F="$2"
[[ "$N" =~ ^[0-9]+$ ]] && [[ "$F" =~ ^[0-9]+$ ]] || { echo "Error: n and f must be non-negative integers"; usage; }
[ "$F" -le "$N" ] || { echo "Error: f ($F) cannot exceed n ($N)"; exit 1; }

echo "==> n=$N total nodes, f=$F maverick (nodes 0..$(( F - 1 ))), $(( N - F )) normal (nodes $F..$(( N - 1 )))"

# ── Port layout (per node i) ──────────────────────────────────────────────────
# p2p   = 26656 + i*3
# rpc   = 26657 + i*3
# abci  = 26658 + i*3
# agent = 50000 + i  (adaptive timer gRPC — started separately by operator)
p2p_port()   { echo $(( 26656 + $1 * 3 )); }
rpc_port()   { echo $(( 26657 + $1 * 3 )); }
abci_port()  { echo $(( 26658 + $1 * 3 )); }
agent_port() { echo $(( 50000 + $1 )); }

cleanup() {
  echo ""
  echo "==> Stopping all nodes and kvstore processes..."
  pkill -f "maverick node" 2>/dev/null || true
  pkill -f "tendermint node" 2>/dev/null || true
  pkill -f "abci-cli kvstore" 2>/dev/null || true
  echo "==> Done."
}
trap cleanup EXIT INT TERM

# ── Step 1: Build binaries ───────────────────────────────────────────────────
echo "==> Building tendermint and maverick..."
make build 2>&1 | tail -1
go build -o "$MAVERICK" ./test/maverick/

# ── Step 2: Generate testnet config ─────────────────────────────────────────
if [ ! -d "$TESTNET_DIR/node0" ]; then
  echo "==> Generating testnet config..."
  "$TENDERMINT" testnet \
    --v "$N" \
    --o "$TESTNET_DIR" \
    --populate-persistent-peers \
    --starting-ip-address 127.0.0.1
else
  echo "==> Testnet config already exists, resetting data..."
  for (( i=0; i<N; i++ )); do
    "$TENDERMINT" unsafe_reset_all --home "$TESTNET_DIR/node$i"
  done
fi

# ── Step 3: Patch config.toml for each node ──────────────────────────────────
echo "==> Patching config files..."
for (( i=0; i<N; i++ )); do
  CONFIG_FILE="$TESTNET_DIR/node$i/config/config.toml"

  # Fix persistent_peers: tendermint testnet assigns sequential IPs (127.0.0.1,
  # 127.0.0.2, …) all on port 26656, but we run all nodes on 127.0.0.1 with
  # distinct ports. Remap 127.0.0.{j+1}:26656 → 127.0.0.1:{p2p_port(j)}.
  # Node 0 is already correct (127.0.0.1:26656 stays as-is).
  for (( j=1; j<N; j++ )); do
    sed -i '' "s|@127.0.0.$(( j + 1 )):26656|@127.0.0.1:$(p2p_port $j)|g" "$CONFIG_FILE"
  done

  # Adaptive timer settings.
  AGENT_PORT=$(agent_port $i)
  sed -i '' "s|adaptive_timer_addr = \".*\"|adaptive_timer_addr = \"127.0.0.1:$AGENT_PORT\"|" "$CONFIG_FILE"
  sed -i '' "s|adaptive_timer_node_index = .*|adaptive_timer_node_index = $i|" "$CONFIG_FILE"
  sed -i '' "s|adaptive_timer_epoch_size = .*|adaptive_timer_epoch_size = $EPOCH_SIZE|" "$CONFIG_FILE"
done
echo "    persistent_peers remapped to 127.0.0.1:PORT for all $N nodes"
echo "    adaptive timer ports: 50000..$(( 50000 + N - 1 ))"

# ── Step 4: Start kvstore + tendermint/maverick in separate Terminal windows ──
echo "==> Starting kvstore and nodes in new Terminal windows..."

for (( i=0; i<N; i++ )); do
  ABCI_PORT=$(abci_port $i)
  P2P_PORT=$(p2p_port $i)
  RPC_PORT=$(rpc_port $i)
  NODE_HOME="$TESTNET_DIR/node$i"

  osascript \
    -e "tell application \"Terminal\"" \
    -e "  do script \"echo '=== kvstore node$i ===' && abci-cli kvstore --address tcp://127.0.0.1:$ABCI_PORT\"" \
    -e "end tell"

  if [ "$i" -lt "$F" ]; then
    # Maverick node
    osascript \
      -e "tell application \"Terminal\"" \
      -e "  do script \"echo '=== maverick node$i ===' && $MAVERICK node --home $NODE_HOME --node-index $i --delay-schedule $DELAY_SCHEDULE --proxy_app tcp://127.0.0.1:$ABCI_PORT --p2p.laddr tcp://0.0.0.0:$P2P_PORT --rpc.laddr tcp://0.0.0.0:$RPC_PORT\"" \
      -e "end tell"
  else
    # Normal tendermint node
    osascript \
      -e "tell application \"Terminal\"" \
      -e "  do script \"echo '=== tendermint node$i ===' && $TENDERMINT node --home $NODE_HOME --proxy_app tcp://127.0.0.1:$ABCI_PORT --p2p.laddr tcp://0.0.0.0:$P2P_PORT --rpc.laddr tcp://0.0.0.0:$RPC_PORT\"" \
      -e "end tell"
  fi
done

# ── Step 5: Wait for all nodes to be ready ───────────────────────────────────
echo "==> Waiting for nodes to be ready..."
for (( i=0; i<N; i++ )); do
  RPC_PORT=$(rpc_port $i)
  echo -n "    Waiting for node$i on port $RPC_PORT..."
  for attempt in $(seq 1 30); do
    if curl -sf "http://localhost:$RPC_PORT/status" > /dev/null 2>&1; then
      echo " ready"
      break
    fi
    if [ "$attempt" -eq 30 ]; then
      echo " TIMED OUT"
      exit 1
    fi
    sleep 1
    echo -n "."
  done
done

# ── Step 6: Wait for peers to connect ────────────────────────────────────────
EXPECTED_PEERS=$(( N - 1 ))
echo "==> Waiting for peers to connect (expecting $EXPECTED_PEERS peers on node0)..."
RPC0=$(rpc_port 0)
for attempt in $(seq 1 20); do
  PEERS=$(curl -s "http://localhost:$RPC0/net_info" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['n_peers'])" 2>/dev/null || echo "0")
  if [ "$PEERS" -eq "$EXPECTED_PEERS" ]; then
    echo "    All $N nodes peered (n_peers=$EXPECTED_PEERS)"
    break
  fi
  echo "    node0 sees $PEERS peers, waiting..."
  sleep 2
done

# ── Step 7: Run load test ─────────────────────────────────────────────────────
echo ""
echo "==> Running load test: rate=$LOAD_RATE tx/s, duration=${LOAD_DURATION}s, connections=$LOAD_CONNECTIONS, size=${LOAD_SIZE}B"
load \
  --endpoints "ws://localhost:$RPC0/websocket" \
  --broadcast-tx-method async \
  --connections "$LOAD_CONNECTIONS" \
  --rate "$LOAD_RATE" \
  --time "$LOAD_DURATION" \
  --size "$LOAD_SIZE"

# ── Step 8: Stop nodes before reading blockstore ─────────────────────────────
echo ""
echo "==> Stopping nodes to release blockstore lock..."
pkill -f "maverick node" 2>/dev/null || true
pkill -f "tendermint node" 2>/dev/null || true
sleep 2

# ── Step 9: Generate report ───────────────────────────────────────────────────
echo "==> Generating latency report..."
report \
  --data-dir "$TESTNET_DIR/node0/data" \
  --database-type goleveldb \
  --csv "$REPO_DIR/results.csv"

echo ""
echo "==> Latency report:"
report \
  --data-dir "$TESTNET_DIR/node0/data" \
  --database-type goleveldb

# ── Step 10: Throughput from CSV ──────────────────────────────────────────────
echo ""
echo "==> Throughput:"
awk -F',' '
  NR==1 { next }
  NR==2 { first=$2; last=$2; count=1; next }
  { if ($2+0 > last+0) last=$2; count++ }
  END {
    duration_s = (last - first) / 1e9
    if (duration_s > 0)
      printf "    Total tx: %d\n    Duration: %.2fs\n    Throughput: %.2f tx/sec\n", count, duration_s, count/duration_s
    else
      print "    Not enough data"
  }
' "$REPO_DIR/results.csv"

echo ""
echo "==> Raw CSV saved to: $REPO_DIR/results.csv"
