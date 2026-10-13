#!/bin/sh
# start.sh — run the bridge in the background with .env exported (the binary reads plain
# env, it never loads .env itself), logging to bridge.log. Refuses when one already listens.
set -e
cd "$(dirname "$0")"

if ss -ltn | grep -q ':8081 '; then
  echo "bridge already listening on :8081" >&2
  exit 1
fi

set -a; . ./.env; set +a
nohup ./bridge >> bridge.log 2>&1 &
echo "bridge pid $!"

for _ in 1 2 3 4 5 6 7 8 9 10; do
  if ss -ltn | grep -q ':8081 '; then
    echo "listening on :8081 (tail -f bridge.log for the session reconnect)"
    exit 0
  fi
  sleep 1
done
echo "not listening after 10s — see bridge.log" >&2
exit 1
