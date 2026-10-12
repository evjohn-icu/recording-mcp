#!/usr/bin/env bash
# asr-health: discriminate why plan-routed ASR (asr-ingest) fails before burning retries.
# Layers: DNS -> TCP/TLS (HTTP reachability) -> WS session probe (init ack) via a 2s tone.
# Verdicts: OK | DNS_FAIL | NETWORK_FAIL | SERVICE_REJECT  (exit 0/2/2/1)
# --wait: re-probe every 5min until OK or deadline (--wait-hours, default 6), then exit 0.
# Key: $VOLC_API_KEY or --key. Requires ffmpeg/ffprobe + ./bin/asr-ingest (repo root cwd or ASR_HOME).
set -u

WAIT=0; HOURS=6; KEY="${ASR_KEY:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --wait) WAIT=1 ;;
    --wait-hours) HOURS="$2"; shift ;;
    --key) KEY="$2"; shift ;;
    *) echo "unknown arg $1" >&2; exit 3 ;;
  esac
  shift
done

HOST=openspeech.bytedance.com
ASR_HOME="${ASR_HOME:-$(pwd)}"
INGEST="$ASR_HOME/bin/asr-ingest"
[ -x "$INGEST" ] || INGEST="$(dirname "$0")/../bin/asr-ingest"

dns_ok() { getent hosts "$HOST" >/dev/null 2>&1; }
tls_ok() { curl -s -o /dev/null --max-time 15 "https://$HOST"; }

ws_probe() {
  # 2s tone -> asr-ingest probe (no --db, throwaway out dir); session-level verdict
  local wav out rc
  wav="$(mktemp /tmp/asr-health-XXXX.mp3)"; out="$(mktemp -d /tmp/asr-health-out-XXXX)"
  ffmpeg -y -v error -f lavfi -i "sine=frequency=440:duration=2" -ac 1 -ar 16000 -b:a 24k "$wav"
  "$INGEST" --audio "$wav" --format auto --segment-ms 60000 --overlap-ms 0 --packet-ms 0 \
    --concurrency 1 --out "$out" > /dev/null 2>&1
  rc=$?
  rm -rf "$out" "$wav"
  [ $rc -eq 0 ]
}

check_once() {
  if ! dns_ok; then echo "DNS_FAIL: cannot resolve $HOST"; return 2; fi
  if ! tls_ok; then echo "NETWORK_FAIL: TCP/TLS to $HOST:443 failed"; return 2; fi
  if [ -z "${VOLC_API_KEY:-}" ] && [ -n "$KEY" ]; then export VOLC_API_KEY="$KEY"; fi
  if [ -z "${VOLC_API_KEY:-}" ]; then echo "CONFIG_ERROR: no VOLC_API_KEY/--key (cannot probe session)"; return 3; fi
  if ws_probe; then echo "OK: session probe completed"; return 0; fi
  echo "SERVICE_REJECT: DNS+TLS fine, WS session did not complete (quota/throttle likely; check Volcano console)"
  return 1
}

if [ "$WAIT" -eq 0 ]; then
  check_once
  exit $?
fi

deadline=$(( $(date +%s) + HOURS * 3600 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  out="$(check_once)"; rc=$?
  echo "$(date -Is) $out"
  [ $rc -eq 0 ] && exit 0
  [ $rc -eq 3 ] && exit 3
  sleep 300
done
echo "SERVICE_REJECT: not recovered within ${HOURS}h"
exit 1
