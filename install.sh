#!/bin/sh
# pips installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/rsbin1178/pips/main/install.sh | sh
#
# Downloads this platform's release archive, verifies its SHA-256 against the
# published SHA256SUMS, installs the binary, and reports the installed version.
# Nothing is installed if the checksum does not match.
#
# Environment:
#   PIPS_VERSION=...         release tag to install (default: the latest release)
#   PIPS_BIN_DIR=...         install directory (default: the first writable one of
#                            /usr/local/bin, /opt/homebrew/bin, ~/.local/bin, ~/bin)
#   PIPS_NO_MODIFY_PATH=1    never edit shell rc files; print the export line instead
#   PIPS_REPO=owner/name     install from a fork (default: rsbin1178/pips)
#   PIPS_API_BASE=...        releases API base (default: https://api.github.com)
#   PIPS_DOWNLOAD_BASE=...   release asset base (default: the GitHub release URL)
#   GH_TOKEN=...             token for the API call, to avoid rate limits
#
# Windows is not supported here. Use WSL2, or extract pips_*_windows_amd64.zip
# from the release manually.

set -eu

REPO="${PIPS_REPO:-rsbin1178/pips}"
API_BASE="${PIPS_API_BASE:-https://api.github.com}"
DOWNLOAD_BASE="${PIPS_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}"
TMP=""

say() { printf '  %s\n' "$*"; }
die() { printf 'pips install: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

need uname
need mktemp
need curl
need tar

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum to verify the download"
fi

os=$(uname -s)
arch=$(uname -m)

case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $os (macOS and Linux only; on Windows use WSL2 or the release .zip)" ;;
esac

case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac

TMP=$(mktemp -d)

token=""
if [ -n "${GH_TOKEN:-}" ]; then
  token="$GH_TOKEN"
elif command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  token=$(gh auth token 2>/dev/null || true)
fi

# api_get fetches a releases API path into $2 and echoes the HTTP status, so the
# caller can tell "no such release" from "rate limited". It never exits non-zero
# for an HTTP status: an unreachable host echoes nothing instead.
api_get() {
  if [ -n "$token" ]; then
    curl -sSL -H "Authorization: Bearer $token" -H "Accept: application/vnd.github+json" \
      -o "$2" -w '%{http_code}' "$1"
  else
    curl -sSL -H "Accept: application/vnd.github+json" -o "$2" -w '%{http_code}' "$1"
  fi
}

fetch() {
  if [ -n "$token" ]; then
    curl -fsSL -H "Authorization: Bearer $token" "$1" -o "$2"
  else
    curl -fsSL "$1" -o "$2"
  fi
}

explain_status() {
  # $1 = status, $2 = what was requested
  case "$1" in
    404)
      die "$2 not found in $REPO"
      ;;
    403|429)
      die "GitHub refused the release lookup (HTTP $1). Retry later, or set GH_TOKEN to a token with public read access."
      ;;
    "")
      die "could not reach the releases API for $REPO"
      ;;
    *)
      die "the releases API returned HTTP $1 for $REPO"
      ;;
  esac
}

version="${PIPS_VERSION:-}"
release_json="$TMP/release.json"

if [ -n "$version" ]; then
  say "Resolving release $version..."
  status=$(api_get "$API_BASE/repos/$REPO/releases/tags/$version" "$release_json" || true)
  [ "$status" = "200" ] || explain_status "$status" "release $version"
else
  say "Resolving the latest release..."
  status=$(api_get "$API_BASE/repos/$REPO/releases/latest" "$release_json" || true)
  [ "$status" = "200" ] || explain_status "$status" "the latest release"

  version=$(tr ',' '\n' < "$release_json" |
    sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' |
    head -1)
  [ -n "$version" ] || die "could not read the tag name from the release response (set PIPS_VERSION)"
fi

asset="pips_${version}_${os}_${arch}.tar.gz"
url="$DOWNLOAD_BASE/$version"

printf '\npips %s — %s %s\n' "$version" "$os" "$arch"

say "Downloading $asset..."
fetch "$url/$asset" "$TMP/$asset" ||
  die "could not download $asset from release $version. That release may have no $os/$arch asset; see https://github.com/$REPO/releases/tag/$version"

say "Downloading SHA256SUMS..."
fetch "$url/SHA256SUMS" "$TMP/SHA256SUMS" ||
  die "could not download SHA256SUMS from $version"

expected=$(grep " $asset\$" "$TMP/SHA256SUMS" | cut -d' ' -f1 | head -1)
[ -n "$expected" ] || die "$version has no checksum for $asset"

actual=$(sha256 "$TMP/$asset")

if [ "$expected" != "$actual" ]; then
  printf '  expected sha256: %s\n  actual   sha256: %s\n' "$expected" "$actual" >&2
  die "CHECKSUM MISMATCH — refusing to install. The download does not match the published checksum."
fi

say "Checksum verified: $actual"

tar -xzf "$TMP/$asset" -C "$TMP" || die "could not extract $asset"

binary="$TMP/${asset%.tar.gz}/pips"
[ -f "$binary" ] || die "the archive did not contain pips"
chmod 755 "$binary"

dest=""
if [ -n "${PIPS_BIN_DIR:-}" ]; then
  mkdir -p "$PIPS_BIN_DIR" 2>/dev/null || true
  dest="$PIPS_BIN_DIR"
else
  for candidate in /usr/local/bin /opt/homebrew/bin "$HOME/.local/bin" "$HOME/bin"; do
    if [ -d "$candidate" ] && [ -w "$candidate" ]; then
      dest="$candidate"
      break
    fi
  done

  if [ -z "$dest" ]; then
    mkdir -p "$HOME/.local/bin" || die "no writable install directory found"
    dest="$HOME/.local/bin"
  fi
fi

if [ -z "$dest" ] || [ ! -w "$dest" ]; then
  die "install directory is not writable: ${dest:-none found}"
fi

if [ -e "$dest/pips" ]; then
  say "Replacing the existing $dest/pips"
fi

mv "$binary" "$dest/pips" || die "could not install into $dest"
say "Installed $dest/pips"

in_path() { case ":$PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac; }

if ! in_path "$dest"; then
  escaped=$(printf '%s' "$dest" | sed "s/'/'\\\\''/g")
  line="export PATH='$escaped':\"\$PATH\""

  if [ "${PIPS_NO_MODIFY_PATH:-}" = "1" ]; then
    say "$dest is not on your PATH; add it yourself with: $line"
  else
    touched=""
    for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.profile"; do
      [ -e "$rc" ] || continue
      if ! grep -qF "$line" "$rc" 2>/dev/null; then
        printf '\n# pips\n%s\n' "$line" >> "$rc"
        touched="$touched $rc"
      fi
    done

    if [ -n "$touched" ]; then
      say "Added $dest to your PATH in:$touched"
      say "Restart your shell, or run now: $line"
    else
      say "$dest is not on your PATH; add it yourself with: $line"
    fi
  fi
fi

printf '\n'
"$dest/pips" version || die "$dest/pips did not run"
printf '\nInstalled pips %s. Run "pips" to start.\n' "$version"

if [ "$os" = "darwin" ]; then
  cat <<'EOF'

macOS notes:
  • The first run may be blocked by Gatekeeper. Allow it in
    System Settings → Privacy & Security, or run:
      xattr -d com.apple.quarantine "$(command -v pips)"
  • Sandboxed execution needs no extra install on macOS.
EOF
fi

if [ "$os" = "linux" ]; then
  cat <<'EOF'

Linux notes:
  • The default workspace-write sandbox needs Bubblewrap 0.8.0 or later.
  • Run `pips doctor` to check sandbox, credentials, and workspace state.
EOF
fi
