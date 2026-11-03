#!/usr/bin/env bash
# Keeps private things out of the public repo. It runs as the pre-commit and pre-push hook
# (tools/git-hooks; turn on with `git config core.hooksPath tools/git-hooks`), or by hand:
#   tools/check-public.sh          checks what is staged for the next commit
#   tools/check-public.sh REV...   checks the whole tree of each of those commits
#
# It refuses:
#   - files that never belong in the repo: builds and APKs, keys, local settings, data files
#   - files over 2 MB, and compiled programs
#   - things that look like keys or tokens
#   - your private strings, listed outside the repo in ~/.config/pifoto/private-strings.txt
#     (one per line, any case; # starts a comment). A line `secretfile:PATH` means every line
#     of that file is a secret, such as a password file; those are never printed.
#   - the release-signing passwords (from ~/.config/pifoto/release-signing.properties)
#
# To skip it once on purpose: git commit --no-verify (or git push --no-verify).
set -u
cd "$(git rev-parse --show-toplevel)" || exit 2

CONF="${PIFOTO_PRIVATE_STRINGS:-$HOME/.config/pifoto/private-strings.txt}"
SIGNING="$HOME/.config/pifoto/release-signing.properties"
MAX_BYTES=$((2 * 1024 * 1024))
FORBIDDEN='(^|/)(CLAUDE\.md|local\.properties|release-signing\.properties|cookies\.txt|users\.json|sessions\.json|links\.json|index\.json|device\.key|\.env(\..*)?)$|\.(apk|aab|jks|keystore|p12|pfx|pem|zip)$|(^|/)install-qr\.png$|^deploy/local/|^(pifoto|pifoto-armv6|pifoto-amd64|takeout-import)$|(^|/)(build|\.gradle|\.kotlin)/'
TOKENS='-----BEGIN [A-Z ]*PRIVATE KEY-----|tskey-[a-z]+-[A-Za-z0-9]{8,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|xox[abpr]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{30,}'

# Private strings and secrets, kept in temporary files only this user can read.
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT; chmod 700 "$tmp"
: > "$tmp/private"; : > "$tmp/secret"
if [ -f "$CONF" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    case "$line" in
      ''|'#'*) ;;
      secretfile:*)
        f="${line#secretfile:}"; f="${f/#\~/$HOME}"
        if [ -r "$f" ]; then tr -d '\r' < "$f" | grep -v '^$' >> "$tmp/secret"
        else echo "check-public: can't read $f (from $CONF)" >&2; fi ;;
      *) printf '%s\n' "$line" >> "$tmp/private" ;;
    esac
  done < "$CONF"
else
  echo "check-public: warning: $CONF is missing, so your private strings are not checked" >&2
fi
if [ -r "$SIGNING" ]; then
  sed -n 's/^\(storePassword\|keyPassword\)=//p' "$SIGNING" | tr -d '\r' | grep -v '^$' >> "$tmp/secret"
fi

fail=0
problem() { echo "  $*" >&2; fail=1; }

# check WHERE: WHERE is --cached (the staged files) or a commit.
check() {
  local where="$1" list
  if [ "$where" = --cached ]; then
    list="$(git ls-files -s | awk '{ sub(/^[^\t]*\t/, "", $0); print }')"
  else
    list="$(git ls-tree -r --name-only "$where")"
  fi
  [ -n "$list" ] || return 0

  while IFS= read -r p; do problem "not allowed in the repo: $p"; done < <(grep -E "$FORBIDDEN" <<<"$list")

  # sizes and compiled programs (ELF, Windows .exe, Mach-O)
  local spec
  while IFS=$'\t' read -r size p; do
    [ "$size" -gt "$MAX_BYTES" ] && problem "too large ($((size / 1024)) KB): $p"
    if [ "$where" = --cached ]; then spec=":$p"; else spec="$where:$p"; fi
    case "$(git cat-file blob "$spec" 2>/dev/null | head -c 4 | od -An -tx1 | tr -d ' \n')" in
      7f454c46|4d5a*|cffaedfe|cefaedfe|feedface|feedfacf) problem "compiled program: $p" ;;
    esac
  done < <(
    if [ "$where" = --cached ]; then
      git ls-files -s | while IFS=$' \t' read -r _ obj _ p; do printf '%s\t%s\n' "$(git cat-file -s "$obj")" "$p"; done
    else
      git ls-tree -r -l "$where" | awk -F'\t' '{ split($1, a, " "); print a[4] "\t" $2 }'
    fi
  )

  local hits
  hits="$(git grep -I -l -E -e "$TOKENS" "$where" -- . 2>/dev/null)"
  [ -n "$hits" ] && while IFS= read -r p; do problem "looks like a key or token: ${p#"$where":}"; done <<<"$hits"
  if [ -s "$tmp/private" ]; then
    hits="$(git grep -I -n -i -F -f "$tmp/private" "$where" -- . 2>/dev/null)"
    [ -n "$hits" ] && while IFS= read -r h; do problem "private string: $(cut -c1-160 <<<"${h#"$where":}")"; done <<<"$hits"
  fi
  if [ -s "$tmp/secret" ]; then
    hits="$(git grep -a -l -F -f "$tmp/secret" "$where" -- . 2>/dev/null)"
    [ -n "$hits" ] && while IFS= read -r p; do problem "contains a password (not shown): ${p#"$where":}"; done <<<"$hits"
  fi
}

if [ $# -eq 0 ]; then
  check --cached
  what="this commit"
else
  for rev in "$@"; do check "$rev"; done
  what="this push"
fi

if [ $fail -ne 0 ]; then
  echo "check-public: stopped $what: the files above must not go into the public repo." >&2
  echo "  Unstage with: git restore --staged <file> (and add it to .gitignore). A commit that is already made: git commit --amend, or ask for help before pushing." >&2
  exit 1
fi
exit 0
