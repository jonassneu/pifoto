# PiFoto

An extremely lightweight, self-hosted photo library: a minimal Google Photos alternative that runs on a
Raspberry Pi 1 (ARMv6, 512 MB RAM).

## Extremely lightweight

- **One 10 MB binary.** The server, the web UI and its icons are all in it. Nothing else to
  install except `djpeg` for thumbnails.
- **About 45 MB of RAM** while serving a library of about 25,000 photos and videos (170 GB) on a Pi 1.
- **No database and no dependencies.** Go standard library only; the library index is
  a JSON file.
- **No frameworks in the web UI.** The whole app is one HTML file, about 60 KB gzipped.

## What it does

- A web library of your photos and videos, grouped by month, with a viewer and uploads from the browser.
- Family accounts: everyone has their own private library, plus a shared Family album. Admins
  invite people with invite or sign-up links and can set storage limits.
- An Android app (`android/`) that shows the library and backs up new photos and videos by itself.
  It updates itself from the server.
- Duplicates are stored only once.
- An importer for Google Takeout exports (`tools/takeout-import`).

## How it's built

- The server is Go with the standard library only. `web/index.html` is the whole web UI and is
  embedded in the binary.
- Thumbnails are made with `djpeg`, which is fast enough on a Pi 1.
- `deploy/pifoto.service` is a hardened systemd unit for running it.
- The API is documented in `docs/family-api.md`.

## Build and run

```sh
go test ./...
# create the first (admin) account; the password is read from stdin:
go run . -data /path/to/library -set-password -user Jonas
go run . -data /path/to/library -addr :8080
```

For a Raspberry Pi 1:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags='-s -w' -o pifoto-armv6 .
```

To reach the server from outside your home network, pass its public address with
`-remote-url https://...` (for example a Tailscale Funnel address). The Android app reads the
same address at build time from `pifoto.publicServer` in `android/local.properties`.
