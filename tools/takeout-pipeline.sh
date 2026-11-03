#!/usr/bin/env bash
# Imports the downloaded Google Takeout parts into PiFoto one after another, as soon as each
# part has finished downloading (takeout-fetch.sh renames NAME.zip.part to NAME.zip).
# Resumable: takeout-import remembers finished files, so re-running is always safe.
# Pause: touch ~/.cache/pifoto-takeout/PAUSE (and kill the running takeout-import);
# remove the file to continue. Usage: takeout-pipeline.sh DIR PART...
# Needs TAKEOUT_PREFIX (see takeout-fetch.sh) and PIFOTO_PASSWORD_FILE; PIFOTO_SERVER defaults to
# http://raspberrypi.fritz.box.
set -u
DIR="$1"; shift
C="$HOME/.cache/pifoto-takeout"
IMPORT="$(cd "$(dirname "$0")/.." && pwd)/takeout-import"
PREFIX="${TAKEOUT_PREFIX:?set TAKEOUT_PREFIX}"
SERVER="${PIFOTO_SERVER:-http://raspberrypi.fritz.box}"
PWFILE="${PIFOTO_PASSWORD_FILE:?set PIFOTO_PASSWORD_FILE}"
log() { echo "$(date '+%F %T') $*"; }

for n in "$@"; do
  f="$DIR/$(printf '%s-%03d.zip' "$PREFIX" "$n")"
  [ -f "$C/imported-$n" ] && { log "part $n already imported"; continue; }
  until [ -f "$f" ]; do sleep 60; done
  [ -f "$C/dryrun-part$n.log" ] || { "$IMPORT" -dry-run -server "$SERVER" -password-file "$PWFILE" "$f" > "$C/dryrun-part$n.log" 2>&1; log "part $n dry run:"; tail -6 "$C/dryrun-part$n.log" | sed 's/^/    /'; }
  for try in 1 2 3 4 5 6; do
    while [ -f "$C/PAUSE" ]; do sleep 60; done
    log "part $n: import run $try"
    "$IMPORT" -server "$SERVER" -password-file "$PWFILE" "$f" > "$C/import-part$n-run$try.log" 2>&1
    rc=$?
    tail -4 "$C/import-part$n-run$try.log" | sed 's/^/    /'
    if [ $rc = 0 ]; then touch "$C/imported-$n"; log "part $n: done"; break; fi
    log "part $n: run $try ended with code $rc, retrying in 10 min"
    sleep 600
  done
  [ -f "$C/imported-$n" ] || log "part $n: still has failures after 6 runs, moving on (zip kept)"
done
log "pipeline finished"
