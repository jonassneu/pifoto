#!/usr/bin/env bash
# Builds the Android app (release) and publishes it on the Pi. Phones and tablets with app 1.4 or
# newer then update themselves within a few hours (see android/.../Updater.kt and appupdate.go);
# new installs get it from <public address>/app (pifoto.publicServer in android/local.properties).
#
#   tools/publish-app.sh                    # build and publish
#   tools/publish-app.sh "What's new"       # ... with a note the phones show after updating
#   NO_BUILD=1 tools/publish-app.sh         # publish the APK that was already built
#
# Before publishing: raise versionCode (and versionName) in android/app/build.gradle.kts, or the
# phones won't see a new version. The script refuses a versionCode that isn't newer than the one
# on the Pi, and any APK that isn't signed with the release key.
set -euo pipefail

NOTES="${1:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APK="$ROOT/android/app/build/outputs/apk/release/app-release.apk"
DEST=/mnt/dietpi_userdata/pifoto/app
PUBLIC="$(sed -n "s/^pifoto.publicServer=//p" "$ROOT/android/local.properties" 2>/dev/null | tr -d "\r")"
RELEASE_CERT=1de2e0ec2f402c94e0fbb9b4d6cf14b48d5bf874fe47bf44f4da1aef0ad35683 # the key the first APKs were signed with

if [ -z "${NO_BUILD:-}" ]; then
  (cd "$ROOT/android" && ./gradlew --no-daemon -q assembleRelease)
fi
[ -f "$APK" ] || { echo "no APK at $APK" >&2; exit 1; }

SDK="${ANDROID_HOME:-$HOME/Android/Sdk}"
BT="$(ls -d "$SDK"/build-tools/*/ | sort -V | tail -1)"
BADGE="$("$BT/aapt2" dump badging "$APK" | awk "NR==1")"   # awk, not head: head would end aapt2 with SIGPIPE
CODE="$(sed -n "s/.*versionCode='\([0-9]*\)'.*/\1/p" <<<"$BADGE")"
NAME="$(sed -n "s/.*versionName='\([^']*\)'.*/\1/p" <<<"$BADGE")"
PKG="$(sed -n "s/^package: name='\([^']*\)'.*/\1/p" <<<"$BADGE")"
CERT="$("$BT/apksigner" verify --print-certs "$APK" | sed -n 's/.*certificate SHA-256 digest: //p' | awk "NR==1")"

[ "$PKG" = app.pifoto ] || { echo "refusing: package is '$PKG', not app.pifoto (a debug or test build?)" >&2; exit 1; }
[ "$CERT" = "$RELEASE_CERT" ] || { echo "refusing: not signed with the release key (got '$CERT')" >&2; exit 1; }
[ -n "$CODE" ] || { echo "can't read the versionCode" >&2; exit 1; }

ONPI="$(ssh pi "cat $DEST/PiFoto.json 2>/dev/null || true" | sed -n 's/.*"versionCode": *\([0-9]*\).*/\1/p')"
if [ -n "$ONPI" ] && [ "$CODE" -le "$ONPI" ] && [ -z "${FORCE:-}" ]; then
  echo "refusing: versionCode $CODE isn't newer than the $ONPI on the Pi. Raise it in android/app/build.gradle.kts (FORCE=1 to publish anyway)." >&2
  exit 1
fi

JSON="$(python3 -c 'import json,sys; print(json.dumps({"versionCode": int(sys.argv[1]), "versionName": sys.argv[2], "notes": sys.argv[3]}))' "$CODE" "$NAME" "$NOTES")"
cp "$APK" "$ROOT/PiFoto.apk"
ssh pi "cat > $DEST/PiFoto.apk.new" < "$APK"
printf '%s\n' "$JSON" | ssh pi "cat > $DEST/PiFoto.json.new"
# The APK first, then the version file: a phone never sees the new version number with the old file.
ssh pi "cd $DEST && chown pifoto:pifoto PiFoto.apk.new PiFoto.json.new && chmod 644 PiFoto.apk.new PiFoto.json.new && mv PiFoto.apk.new PiFoto.apk && mv PiFoto.json.new PiFoto.json"
echo "Published PiFoto $NAME (versionCode $CODE). Apps from 1.4 on update themselves within a few hours;"
echo "new installs: ${PUBLIC:-the Pi}/app"
