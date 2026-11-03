#!/usr/bin/env bash
# Resumable download of Google Takeout parts with the browser session copied to
# ~/.cache/pifoto-takeout/cookies.txt (private, mode 600; delete it when done).
# Usage: takeout-fetch.sh OUTDIR PART...   e.g. takeout-fetch.sh /mnt/x 1 3 5
# Needs TAKEOUT_JOB, TAKEOUT_USER and TAKEOUT_PREFIX, copied from a download link on the Takeout page:
#   .../download/<TAKEOUT_PREFIX>-001.zip?j=<TAKEOUT_JOB>&i=0&user=<TAKEOUT_USER>&authuser=0
# A part is written as NAME.zip.part and renamed to NAME.zip once its size matches.
set -u
JAR="$HOME/.cache/pifoto-takeout/cookies.txt"
OUT="$1"; shift
UA='Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36'
JOB="${TAKEOUT_JOB:?set TAKEOUT_JOB}"
GUSER="${TAKEOUT_USER:?set TAKEOUT_USER}"
PREFIX="${TAKEOUT_PREFIX:?set TAKEOUT_PREFIX}"
mkdir -p "$OUT"

for n in "$@"; do
  f=$(printf '%s-%03d.zip' "$PREFIX" "$n")
  url="https://takeout-download.usercontent.google.com/download/$f?j=$JOB&i=$((n-1))&user=$GUSER&authuser=0"
  if [ -f "$OUT/$f" ]; then echo "$(date +%T) part $n already complete"; continue; fi
  total=$(curl -sS -A "$UA" -b "$JAR" -r 0-0 -D - -o /dev/null "$url" | tr -d '\r' | awk -F/ 'tolower($0) ~ /^content-range:/ {print $2}')
  if [ -z "$total" ]; then echo "$(date +%T) part $n: no size from Google (session expired?)"; exit 2; fi
  echo "$(date +%T) part $n: $f, $total bytes"
  fails=0
  while :; do
    have=$(stat -c %s "$OUT/$f.part" 2>/dev/null || echo 0)
    if [ "$have" = "$total" ]; then mv "$OUT/$f.part" "$OUT/$f"; echo "$(date +%T) part $n complete"; break; fi
    if [ "$have" -gt "$total" ]; then echo "$(date +%T) part $n: file larger than expected, restarting"; rm -f "$OUT/$f.part"; continue; fi
    code=$(curl -sS -f -A "$UA" -b "$JAR" -C - --speed-limit 20000 --speed-time 120 -w '%{http_code}' -o "$OUT/$f.part" "$url")
    if [ "$code" = 206 ] || { [ "$code" = 200 ] && [ "$have" = 0 ]; }; then
      fails=0
    else
      # anything else (e.g. a redirect to a login page) must not stay appended to the zip
      case "$code" in 2*) ;; *) truncate -s "$have" "$OUT/$f.part" 2>/dev/null ;; esac
      [ "$code" = 200 ] && rm -f "$OUT/$f.part" # server ignored the range: start over
      fails=$((fails+1)); echo "$(date +%T) part $n: HTTP $code, interrupted at $(stat -c %s "$OUT/$f.part" 2>/dev/null) bytes (try $fails)"
      [ "$fails" -ge 20 ] && { echo "$(date +%T) part $n: giving up"; exit 1; }
      sleep $((fails*15))
    fi
  done
done
