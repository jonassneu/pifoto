# PiFoto API: family accounts

The server (`main.go`, `users.go`, `store.go`, `security.go`, `pwpolicy.go`) implements this. Tests: `family_test.go`, `security_test.go`.

## Model
- Every person has their own login (name and password) and their own **library**. A **Family album** is shared with everyone.
- Files are stored once. If two people upload the same bytes, both libraries point to the same file.
- An item a user can see is either in their library (flag `1`), in the Family album (flag `2`), or both (`3`).
- Admins manage people. The first account (the "owner") is an admin. A library from before family accounts became the owner's, and the old password, sessions and logged-in devices keep working.

## Rows
`GET /api/items` returns `{"seq": N, "items": [row, ...]}`. Each row is:

`[id, kind, takenUnix, thumb, ext, name, size, width, height, flags, sharedBy]`

- `flags` is a bitmask: `1` means it's in my library, `2` means it's in the Family album.
- `sharedBy` is the display name of whoever added it to the Family album. It is `""` if the item isn't shared, or if that person was removed.
- The list contains everything the user may see: their library plus the whole Family album.
  - The **Photos** view shows `flags & 1`.
  - The **Family** view shows `flags & 2`.

## Live changes
`GET /api/status?since=SEQ` returns:
- `count`: number of items in my library
- `used`: bytes in my library
- `quota`: my storage limit in bytes, `0` = no limit (see Storage limits)
- `family`: number of items in the Family album
- `queue`, `working`, `free`, `total`, `seq`, `reset`
- `changes`: a list of entries, each either `{"op":"add","row":[...]}` or `{"op":"del","id":"..."}`

How to handle changes:
- **`add` is an upsert.** It inserts a new item, or replaces the existing item with that id. This covers thumbnail-ready updates, sharing changes and ownership changes; there is no separate `thumb` op any more.
- **`del`** means the item is no longer visible to me.
- **`reset: true`** means reload `/api/items`.

## Auth
All requests other than GET need the header `X-PiFoto: 1`. Session cookie: `pf_s`, valid for one year.

### Home or internet
The server treats a request as **public** (from the internet) if it carries the `Tailscale-Funnel-Request` header (Funnel adds it), or if the client IP is not private. Private means loopback, 10/8, 172.16/12, 192.168/16, 169.254/16, fe80::/10, fc00::/7 (which includes the tailnet's fd7a:115c:a1e0::/48), 100.64/10 (Tailscale), or a direct IPv6 client in the server's own /64 (the LAN also has global IPv6 addresses).
- Behind `tailscale serve`/Funnel every request arrives from 127.0.0.1. For loopback requests only, the server takes the client IP from the last `X-Forwarded-For` entry and HTTPS from `X-Forwarded-Proto`.
- So the home network (`http://raspberrypi.fritz.box`) and tailnet devices (through `tailscale serve`, or directly over WireGuard) count as home. Funnel counts as public.
- Over HTTPS the session cookie is `Secure`, and responses carry `Strict-Transport-Security: max-age=31536000`.

### Login
- `POST /api/login {"name": "...", "password": "..."}` returns 200 with `{"id","name","admin","pending","quota"}` and sets the cookie.
- The body must be JSON. Send `Content-Type: application/json` or no `Content-Type`. The form types (`application/x-www-form-urlencoded`, `multipart/form-data`, `text/plain`) get 415. This stops another web site from logging a browser in to an account it chose. The same applies to `POST /api/join`.
- Names are matched case-insensitively and with surrounding spaces trimmed. Names over 64 characters get 400.
- An empty or missing `name` means the owner, but only from home (old clients and the Takeout importer rely on this). From the internet it gets 400 `Enter your name` and counts against nobody.
- Errors:
  - 401 `wrong name or password`.
  - 429 after 5 failures in 5 minutes from one client IP (an IPv6 /64 counts as one IP). A correct login does not reset this.
  - 429 after 10 failures in 15 minutes for one account. This limit only counts and blocks requests from the internet, so nobody out there can lock the family out at home. Devices already logged in keep working.
  - Trusted devices: a successful login or join also sets the `pf_d` cookie (`Path=/api/login`, HttpOnly, SameSite=Strict, Secure over HTTPS, 2 years). Its value is `<user id>.<32 hex nonce>.<HMAC-SHA256(device.key, "pifoto-device:" + id + "." + nonce)>`, where `device.key` is a random key in the data directory. A login from the internet that brings a valid `pf_d` for the account it logs in to skips the account limit above. Its failures count against that device instead: 429 after 5 in 15 minutes. So strangers who know a name can't keep that person from logging in again on a phone they used before, and a stolen `pf_d` still only allows a few guesses. It opens nothing without the password. The Android app stores it and sends it with its logins.
  - 503 `the server is busy` when too many password checks are already running. Requests from the internet can use at most one of the two password-check slots, so logins from home always find one.
- Passwords over 1024 bytes are not checked (401).

### Sessions and passwords
- `POST /api/logout` returns 204 and ends this device's session. `POST /api/logout {"all": true}` ends all of the caller's sessions, on every device.
- `GET /api/me` returns `{"id","name","admin","pending","quota"}`.
- `POST /api/password {"old","new"}` returns 204. It logs the person out on every other device and sets a new session cookie for this one. A running `takeout-import` has to log in again too.
  - 400 if the new password breaks the rules below (the body says why).
  - 403 if the current password is wrong.
  - 429 after 5 wrong current passwords in 15 minutes for the account, or 5 failures from one IP.
  - 503 when too many password checks are already running.

### Password rules
They apply to passwords chosen through the API (`POST /api/join`, `POST /api/password`). The 400 body is the message to show:
- At least 10 characters (Unicode characters, not bytes): `Use at least 10 characters`.
- At most 256 characters: `Use at most 256 characters` (checked first; the pattern checks get slow on very long input).
- Not easy to guess: `That password is too easy to guess. A few random words together work well.` This covers:
  - passwords on a list of common ones (SecLists' 10k most common and its German top 10000, entries of 10+ characters, in `common_passwords.txt`), also with other digits or symbols at the end;
  - one character repeated, keyboard or alphabet runs like `1234567890`, `qwertzuiop` or `abcdef1234`, and their repetitions;
  - a short piece repeated, like `asdfasdfasdf`.
- Not mostly the person's name: `That password is too close to your name. A few random words together work well.` This applies when taking out the name (or a word of it with 3+ letters, case-insensitive) leaves fewer than 6 characters, or only something guessable, or only a few digits and symbols.
- `-set-password` on the command line accepts any non-empty password, but prints a warning if it breaks these rules.

### Hello (is this really our server?)
The app checks a plain-HTTP home address before it sends its session there. It sends the request without any cookie. Only home and tailnet requests get an answer; from the internet (Funnel) it is always 404. Otherwise a fake home address on someone else's Wi-Fi could relay the challenge to the public address and pass the answer back. The public https address doesn't need this: its certificate proves it.

`GET /api/hello?s=<lowercase hex SHA-256 of the session token>&n=<nonce: 32 to 64 lowercase hex characters>`
- 200 `{"mac": "<lowercase hex HMAC-SHA256(key = session token, message = "pifoto-hello:" + n)>"}` if that session exists and has not expired. `Cache-Control: no-store`.
- 404 for an unknown or expired session, 400 for malformed parameters, 429 after 30 misses in 5 minutes from one IP.
- The session token is the value of the `pf_s` cookie. The app compares the mac in constant time.

### Slow clients
A request body must arrive within 60 seconds. Uploads (`POST /api/upload`) have no overall limit, because multi-GB videos over Funnel take hours, but they are cut after 2 minutes without any data.

### Invites
Adding a person, or resetting a forgotten password, creates an invite code. It is valid for 7 days and works once.

- The invite link is `<origin>/#join=<code>`. `<origin>` is the public address from `GET /api/info` (`{"remote": "https://…"}`, set with `-remote-url`), or, if none is set, the address the admin is using (`location.origin`). The code is in the URL fragment, so it never reaches server logs.
- `GET /api/join/{code}` (no login) returns `{"name": "Mama"}`, or 404 `This invite link is no longer valid. Ask for a new one.`
- `POST /api/join {"code","password"}` (no login, JSON as for login) sets the password and logs the person in (sets the cookie). It returns 200 with `{"id","name","admin","pending"}`.
  - 404 if the code is invalid or expired. The code is checked first, before any password hashing. Bad codes count against the client IP (429 after 5 in 5 minutes, shared with `GET /api/join/{code}`).
  - 400 if the password breaks the password rules (the body says why).
  - 503 when too many password checks are already running.

### Sign-up links
An admin can make a link that lets people make their own account: they choose their name and password. It works until it expires or has made `max` accounts, whichever comes first. Accounts made with it are never admins and start with the link's storage limit. Links are in `links.json`; the code is kept as it is (not hashed like invite codes), so admins can copy a link again.

- The link is the same as an invite link: `<origin>/#join=<code>`.
- `GET /api/join/{code}` (no login) returns `{"open": true, "by": "<admin's name>", "quota": bytes}` for a sign-up link. The admin's note is never shown.
- `POST /api/join {"code","name","password"}` (no login) makes the account and logs it in, like an invite.
  - 400 `Enter your name (at most 32 characters)` for an empty or too long name, or 400 with the password rules' message.
  - 409 if the name is already used (case-insensitive). This counts against the client IP like a bad code (429 after 5 in 5 minutes), so a link can't be used to list everyone's login name.
  - 404 if the link has expired, is used up or was deleted. 403 when PiFoto already has 100 accounts.
  - Making the account is atomic: a link for one person never makes two, even with two tries at once.
- The server logs every account made this way, with the link's id and the client IP.

## Storage limits
Every person can have a storage limit (`quota`, bytes; `0` = none). It counts everything in their library (`used` in `/api/status`): their own uploads, files someone else also has, and Family album photos they saved to their library. Nothing is deleted when a limit is lowered below what someone has; they just can't add more.

- An upload that would go over the limit gets **507** `Your storage on PiFoto is full (… of … used). Ask an admin for more space.` (507, not 4xx: the Android app gives up on a file for good on 4xx, and this one can go up once there's room).
  - With a `Content-Length` (browsers), it is refused before the body is read if `used + Content-Length` is over the limit. Without one (chunked, the Android app), it is refused as soon as more than what's left has arrived; bytes of the person's other uploads running at the same time count too. So a limited person can never put more than their free space into `tmp/`. A file they already have is still a `dup` when it fits in what's left; one that doesn't fit is refused like any other.
  - The Android app asks `/api/status` first and doesn't send a file that can't fit; smaller files after it still go. Backup then says "Storage full" and checks again on its hourly run. The web app skips files that don't fit and counts them as "didn't fit".
- Sign-up links stop working when whoever made them is no longer an admin (demoted or removed).
- `POST /api/keep` skips photos that don't fit and returns `"full": true`.

## Android app updates
`tools/publish-app.sh` puts the release APK at `<data>/app/PiFoto.apk` and a version file next to it, `PiFoto.json` (`{"versionCode","versionName","notes"}`). No login needed for:
- `GET /app`: the download page (version, size, date, what's new, how to install; a QR code when not opened on Android). Android's own HTTP client (user agent starting with `Dalvik/`) gets the APK here instead, for app 1.4.2's updater and older links.
- `GET /app/PiFoto.apk`: the APK, named `PiFoto-<version>.apk`, with Range support.
- `GET /app/qr.png`: the QR code for the public `/app` address.

`GET /api/app` (logged in) returns `{"versionCode","versionName","notes","size","sha256","published"}`, or 404 when nothing is published. The app sends `X-PiFoto-App: <versionCode> <versionName>`; the server keeps that on the session (`app`, `appV` in `sessions.json`, written only when it changes), and `GET /api/users` lists it per person as `apps: [{"code","name"}]`, plus the published release as `app`.

App 1.4 and newer ask every 6 hours on Wi-Fi and when opened (at most hourly), download a newer version (checking size and SHA-256), and install it once the app isn't on screen and no backup is uploading. Android checks that it's signed with the same key. Android 12+ installs it silently once the person has allowed "Install unknown apps" for PiFoto; the first time, PiFoto shows Android's install screen when it's opened (or a notification), which offers that setting.

## Photos
`POST /api/upload?name=…&mtime=…[&family=1]` (body is the raw file) returns `{"dup": bool, "row": [...], "similar"?: true}`, or 507 over the storage limit.
- `dup: true` means it was already in my library: the same bytes, or the same photo by name and capture time.
- `family=1` also adds the photo to the Family album.

`DELETE /api/items/{id}` returns 204, or 404 if it isn't in my library.
- It removes the item from **my library**.
- If I was the one who shared it to Family, it also leaves the Family album.
- The file is only moved to the trash when nobody has it any more.
- To take someone else's photo out of Family, use `POST /api/family` instead (admin only).

`POST /api/family {"ids":[...], "on": true|false}` returns `{"done": n, "failed": n}`.
- `on: true` adds photos from my library to Family. Each must be in my library.
- `on: false` removes them from Family. Only the person who shared a photo, or an admin, can do this.

`POST /api/keep {"ids":[...]}` returns `{"done","failed"}`. It adds Family photos to my own library (no extra disk space).

`GET /media/{thumb|preview|orig}/{id}` works for anything I can see; anything else returns 404.

## People (admins only; others get 403)
The web app's admin dashboard (menu → Admin dashboard) uses these.

`GET /api/users` returns `{"me": myId, "users": [{"id","name","admin","pending","quota","count","bytes","share","created","via"?,"inviteExpires"?}], "free", "total", "files", "library", "famOnly"}`.
- `pending: true` means the person has not chosen a password yet.
- `count` and `bytes` describe their library (what the storage limit counts).
- `share` is their part of the stored bytes for the disk chart: a file two people have counts half for each, so the shares plus `famOnly` (Family album photos of removed people) add up to `library` (all stored files once).
- `via` is the id of the sign-up link they joined with. `created` and `inviteExpires` are unix times.
- `free` and `total` are the data disk's bytes; `files` is the number of stored files.

`POST /api/users {"name", "admin"?: false, "quota"?: 0}` returns `{"user": {...}, "code": "<invite code>"}`.
- 409 if the name is already used (case-insensitive).
- 400 if the name is empty or over 32 characters, the limit is negative or over 1 PiB, or PiFoto already has 100 accounts.

`PATCH /api/users/{id} {"name"?, "admin"?, "quota"?}` returns the user. You can't remove your own admin rights (400).

`POST /api/users/{id}/invite` returns `{"code"}`. Use it to make a new link or reset a password.
- Their old password stops working and they are logged out everywhere.
- 400 for yourself; use `POST /api/password` instead.

`GET /api/links` returns `{"links": [{"id","code","label","by","created","expires","max","used","quota","users","live"}]}`, newest first. `users` are the names of accounts made with it that still exist; `used` also counts removed ones. Dead links (expired or used up) stay listed until they're deleted, or 30 days after they expired.

`POST /api/links {"label"?, "hours", "max", "quota"?}` returns the new link. `hours`: 1 to 2160 (90 days); `max`: 1 to 50 accounts; `label`: an optional note, at most 40 characters. 400 otherwise, or when 50 links exist.

`DELETE /api/links/{id}` returns 204: the link stops working at once. Accounts made with it stay.

`DELETE /api/users/{id}` returns `{"trashed": n}`.
- It removes the person and their library.
- Photos they shared stay in Family.
- 400 for yourself.

## Running locally
```
export PATH="$HOME/.local/go/bin:$PATH"
D=$(mktemp -d); echo devpass | go run . -data "$D" -set-password -user Jonas   # warns: too short for the app, set anyway
go run . -data "$D" -addr 127.0.0.1:8090    # log in as Jonas / devpass
```
`go run ./tools/mkphotos` generates synthetic test JPEGs; never use real photos. Upload them with:

```
curl -b cookies -H 'X-PiFoto: 1' --data-binary @x.jpg 'http://127.0.0.1:8090/api/upload?name=x.jpg'
```
