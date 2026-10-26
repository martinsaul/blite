#!/usr/bin/env bash
# Synthetic-peer soak harness. Starts a private signaling server and
# N headless peers (test pattern + test tone), each in its own network
# namespace behind a tc netem link profile, then summarizes their stats logs.
#
# Every peer namespace hangs off a bridge inside a hub namespace, so nothing
# touches host networking (no forwarding, no host routes). Needs sudo -n.
#
#   scripts/soak.sh -p fiber,dsl,4g -t 60m          # three peers, one hour
#   scripts/soak.sh -p 4g,terrible,modem -t 2m -o /tmp/soak
#   scripts/soak.sh -p fiber,dsl -j 'https://blite.example.com/j/CODE#pw'
#   scripts/soak.sh -p fiber,dsl,dsl,4g -r 0        # relay mode: peer 0 relays
#   SOAK_ARGS="-resolution 1080p" scripts/soak.sh -p fiber,mid   # extra peer flags
#
# With -j the peers join an existing room (humans included) over the host's
# real network instead: no namespaces, no shaping.
#
# Profiles (down/up, one-way delay, jitter, loss):
#   fiber    100M/20M    5ms
#   dsl      10M/1M      15ms
#   mid      1.2M/1.2M   20ms   (the test pattern: 1080p does not fit, 720p does)
#   4g       20M/5M      30ms  +-30ms  2% loss
#   terrible 1M/250k     150ms
#   modem    56k/56k     75ms
set -euo pipefail
cd "$(dirname "$0")/.."

profiles=fiber,dsl,4g
dur=2m
out=
link=
port=18190
relay=
while getopts p:t:o:j:P:r: o; do
  case $o in
    p) profiles=$OPTARG ;;
    t) dur=$OPTARG ;;
    o) out=$OPTARG ;;
    j) link=$OPTARG ;;
    P) port=$OPTARG ;;
    r) relay=$OPTARG ;;
    *) sed -n '2,24p' "$0"; exit 2 ;;
  esac
done
out=${out:-$(mktemp -d /tmp/blite-soak.XXXX)}
mkdir -p "$out"
IFS=, read -ra prof <<<"$profiles"

# Queue limits are ~500 ms of 1200-byte packets at the link rate (min 10), a
# typical bloated buffer; netem's default of 1000 would be 40 s on the slow ones.
shape() { # shape <profile> -> "downrate uprate delay jitter loss downlimit uplimit"
  case $1 in
    fiber) echo "100mbit 20mbit 5ms 0ms 0% 5200 1040" ;;
    dsl) echo "10mbit 1mbit 15ms 0ms 0% 520 52" ;;
    mid) echo "1200kbit 1200kbit 20ms 0ms 0% 62 62" ;;
    4g) echo "20mbit 5mbit 30ms 30ms 2% 1040 260" ;;
    terrible) echo "1mbit 250kbit 150ms 0ms 0% 52 13" ;;
    modem) echo "56kbit 56kbit 75ms 0ms 0% 10 10" ;;
    *) echo "unknown profile $1" >&2; exit 2 ;;
  esac
}
for p in "${prof[@]}"; do shape "$p" >/dev/null; done

# Relay mode needs build >= 31 on every peer; soak builds claim a high one.
go build -ldflags "-X main.version=soak -X main.build=9999" -o bin/blite ./cmd/blite
go build -o bin/blite-signal ./cmd/blite-signal

me=$(id -un)
hub=blsoak-hub
shaped=; [ -z "$link" ] && shaped=1
pids=()     # everything we started
peerpids=() # just the peers, which exit on their own after -for
cleanup() {
  for pid in "${pids[@]}"; do sudo -n kill "$pid" 2>/dev/null || true; done
  if [ -n "$shaped" ]; then
    for i in "${!prof[@]}"; do sudo -n ip netns del "blsoak$i" 2>/dev/null || true; done
    sudo -n ip netns del "$hub" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# run <netns|-> <cmd...>: as the invoking user, inside the namespace if given.
run() {
  local ns=$1; shift
  if [ "$ns" = - ]; then "$@"; else sudo -n ip netns exec "$ns" sudo -n -u "$me" -- "$@"; fi
}

peer() { # peer <i> <ns> <joinargs...>
  local i=$1 ns=$2; shift 2
  [ "$i" = "$relay" ] && set -- "$@" -volunteer
  # shellcheck disable=SC2086 # SOAK_ARGS is a flag list
  run "$ns" env HOME="$HOME" bin/blite -headless "$@" -source test -audio test -view count ${SOAK_ARGS:-} \
    -name "syn$i-${prof[$i]}" -json -for "$dur" >"$out/syn$i.out" 2>"$out/syn$i.log" &
  pids+=($!) peerpids+=($!)
}

if [ -n "$shaped" ]; then
  sudo -n ip netns add "$hub"
  sudo -n ip -n "$hub" link add br0 type bridge
  sudo -n ip -n "$hub" addr add 10.201.0.1/24 dev br0
  sudo -n ip -n "$hub" link set br0 up
  sudo -n ip -n "$hub" link set lo up
  for i in "${!prof[@]}"; do
    ns=blsoak$i
    read -r down up delay jitter loss dlim ulim < <(shape "${prof[$i]}")
    sudo -n ip netns add "$ns"
    sudo -n ip -n "$ns" link add eth0 type veth peer name "p$i" netns "$hub"
    sudo -n ip -n "$hub" link set "p$i" master br0 up
    sudo -n ip -n "$ns" addr add "10.201.0.$((i + 10))/24" dev eth0
    sudo -n ip -n "$ns" link set eth0 up
    sudo -n ip -n "$ns" link set lo up
    # Egress of the peer is its uplink; egress of the hub port is its downlink.
    j=; [ "$jitter" != 0ms ] && j="$jitter distribution normal"
    l=; [ "$loss" != 0% ] && l="loss $loss"
    # shellcheck disable=SC2086
    sudo -n ip netns exec "$ns" tc qdisc add dev eth0 root netem rate "$up" delay "$delay" $j $l limit "$ulim"
    # shellcheck disable=SC2086
    sudo -n ip netns exec "$hub" tc qdisc add dev "p$i" root netem rate "$down" delay "$delay" $j $l limit "$dlim"
  done
  run "$hub" env BLITE_ADDR="10.201.0.1:$port" bin/blite-signal >"$out/signal.log" 2>&1 &
  pids+=($!)
  sleep 1
  server=ws://10.201.0.1:$port/ws
  peer 0 blsoak0 -create -server "$server"
  for _ in $(seq 50); do
    link=$(grep -o 'link [^ ]*' "$out/syn0.out" 2>/dev/null | cut -d' ' -f2) && [ -n "$link" ] && break
    sleep 0.2
  done
  [ -n "$link" ] || { echo "room was not created; see $out/syn0.log" >&2; exit 1; }
  for ((i = 1; i < ${#prof[@]}; i++)); do peer "$i" "blsoak$i" -link "$link" -server "$server"; done
else
  for i in "${!prof[@]}"; do peer "$i" - -link "$link"; done
fi

echo "soak: ${#prof[@]} peers (${profiles}) for $dur, logs in $out"
for pid in "${peerpids[@]}"; do wait "$pid" || true; done

python3 -I - "$out" "${#prof[@]}" <<'PY'
import json, sys, statistics as st
out, n = sys.argv[1], int(sys.argv[2])
print(f"{'peer':16} {'cpu%':>6} {'from':16} {'pair':12} {'rtt':>5} {'up':>8} {'down':>8} {'fps':>5} {'loss%':>5} {'frz':>4}  send layers")
for i in range(n):
    me, cpu, peers = None, [], {}
    for line in open(f"{out}/syn{i}.log"):
        try:
            r = json.loads(line)
        except ValueError:
            continue
        if r.get("msg") == "self":
            cpu.append(r["cpu"])
        elif r.get("msg") == "peer" and r.get("state") == "connected":
            peers.setdefault(r["peer"], []).append(r)
        elif r.get("level") == "ERROR":
            print(f"syn{i}: ERROR {r.get('msg')}: {r.get('err')}")
    name = f"syn{i}"
    avg = lambda rs, k: st.mean(r[k] for r in rs) if rs else 0
    first = True
    for p, rs in sorted(peers.items()):
        rs = rs[1:] or rs  # skip the connect interval
        layers = {}
        for r in rs:
            layers[r.get("sendLayer") or "-"] = layers.get(r.get("sendLayer") or "-", 0) + 1
        print(f"{name if first else '':16} {(f'{st.mean(cpu):.0f}' if cpu and first else ''):>6} {p:16} {rs[-1].get('pair',''):12} "
              f"{avg(rs,'rttMs'):5.0f} {avg(rs,'upKbps'):8.0f} {avg(rs,'downKbps'):8.0f} {avg(rs,'fps'):5.1f} "
              f"{avg(rs,'loss'):5.1f} {rs[-1]['freezes']:4d}  {' '.join(f'{k}:{v}' for k, v in sorted(layers.items()))}")
        first = False
    if first:
        print(f"{name:16} never connected to anyone")
PY
