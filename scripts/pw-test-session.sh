#!/usr/bin/env bash
# Runs a command inside a private PipeWire session with two tone players
# (AppOne 440 Hz, AppTwo 1000 Hz) on a null "speakers" sink. Touches nothing
# outside its own runtime dir and D-Bus session.
#   scripts/pw-test-session.sh go test -run TestShareOnlySelectedApp ./internal/audio
set -euo pipefail
if [ -z "${BLITE_PW_INNER:-}" ]; then
  export BLITE_PW_INNER=1
  exec dbus-run-session -- "$0" "$@"
fi
rt=$(mktemp -d); chmod 700 "$rt"; export XDG_RUNTIME_DIR=$rt
cleanup() { kill $(jobs -p) 2>/dev/null || true; rm -rf "$rt"; }
trap cleanup EXIT
pipewire >"$rt/pw.log" 2>&1 & sleep 0.5
wireplumber >"$rt/wp.log" 2>&1 &
pipewire-pulse >"$rt/pp.log" 2>&1 & sleep 2
pactl load-module module-null-sink sink_name=speakers >/dev/null
pactl set-default-sink speakers
python3 -I - "$rt" <<'PY'
import sys, wave, struct, math
for f, n in ((440, "one"), (1000, "two")):
    w = wave.open(f"{sys.argv[1]}/{n}.wav", "wb"); w.setnchannels(2); w.setsampwidth(2); w.setframerate(48000)
    w.writeframes(b"".join(struct.pack("<hh", *(int(9000*math.sin(2*math.pi*f*i/48000)),)*2) for i in range(48000*60)))
PY
pw-play -P "{ application.name=AppOne }" "$rt/one.wav" &
pw-play -P "{ application.name=AppTwo }" "$rt/two.wav" &
sleep 1.5
BLITE_PW_TEST=1 "$@"
