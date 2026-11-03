# PiFoto

An extremely lightweight, self-hosted photo library: a minimal Google Photos alternative that
runs on a Raspberry Pi 1 (ARMv6, 512 MB RAM).

- **Tiny:** one 10 MB binary with the web UI built in, about 45 MB of RAM for a library of
  25,000 photos and videos. No database and no dependencies (Go standard library only).
- **Family accounts:** everyone has their own private library, plus a shared Family album.
  Admins invite people by link and can set storage limits.
- **Android app:** backs up new photos and videos automatically, at home and away, and updates
  itself from your server.
- **Google Takeout import,** with duplicates stored only once.

## Quick start

Needs Go and `djpeg` (Debian/Raspberry Pi OS: `apt install libjpeg-turbo-progs`).

```sh
# create the first (admin) account; the password is read from stdin
echo 'a long password' | go run . -data ./library -set-password -user Jonas
go run . -data ./library -addr :8080      # then open http://localhost:8080
```

## On a Raspberry Pi

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags='-s -w' -o pifoto-armv6 .
```

Copy the binary to `/opt/pifoto/pifoto` and install `deploy/pifoto.service`, a sandboxed systemd
unit; its comments say where your own settings go. To use PiFoto away from home, give it a
public HTTPS address (for example with Tailscale Funnel) and pass that with `-remote-url`.

## Android app

Put your server's public address in `android/local.properties`:

```properties
pifoto.publicServer=https://your-pi.example.ts.net
```

Then build with `cd android && ./gradlew assembleRelease`. Release signing reads
`~/.config/pifoto/release-signing.properties` (`storeFile`, `storePassword`, `keyAlias`,
`keyPassword`). Put the APK in `<data>/app/` (see `tools/publish-app.sh`): phones download it
from `/app`, and installed apps update themselves.

## Import from Google Takeout

```sh
go build -o takeout-import ./tools/takeout-import
./takeout-import -server http://your-pi -password-file pw.txt -dry-run takeout-*.zip
```

It reads the ZIPs directly, skips duplicates, and can be stopped and run again. Drop `-dry-run`
to upload.

## More

- API and security rules: `docs/family-api.md`
- Tests: `go test ./...`; synthetic test photos: `go run ./tools/mkphotos OUTDIR [count]`
