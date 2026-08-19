#!/bin/sh
# The TechBeaver CLI installer. Served at https://techbeaver.io/install.sh
#
#   curl -fsSL https://techbeaver.io/install.sh | sh
#
# Prefer not to pipe a script into a shell? Download the binary from the
# releases page, verify its checksum, chmod +x it and put it on your PATH. This
# script is a convenience over that, never the only way in.
set -eu

REPO="techbeaver/beaver-cli"
BIN="beaver"

info()  { printf '%s\n' "$*" >&2; }
fatal() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || fatal "this installer needs $1"
}

detect_platform() {
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "$os" in
    linux|darwin) ;;
    *) fatal "unsupported operating system: $os. Windows users: see https://techbeaver.io/install.ps1" ;;
  esac
  case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fatal "unsupported architecture: $arch" ;;
  esac
  printf '%s_%s' "$os" "$arch"
}

# choose_dir picks somewhere already on PATH, because a binary that is not on
# PATH is the thing a download page leaves the user to fix. See ADR 0012.
choose_dir() {
  if [ -n "${BEAVER_INSTALL_DIR:-}" ]; then
    printf '%s' "$BEAVER_INSTALL_DIR"; return
  fi
  for candidate in "$HOME/.local/bin" "$HOME/bin"; do
    case ":$PATH:" in *":$candidate:"*) printf '%s' "$candidate"; return ;; esac
  done
  if [ -w /usr/local/bin ] 2>/dev/null; then
    printf '%s' /usr/local/bin; return
  fi
  printf '%s' "$HOME/.local/bin"
}

main() {
  need curl
  need tar

  platform=$(detect_platform)
  version=${BEAVER_VERSION:-}
  if [ -z "$version" ]; then
    info "Finding the latest release..."
    version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
    [ -n "$version" ] || fatal "could not determine the latest version. Set BEAVER_VERSION to install a specific one."
  fi
  number=${version#v}

  archive="${BIN}_${number}_${platform}.tar.gz"
  base="https://github.com/$REPO/releases/download/$version"

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT INT TERM

  info "Downloading $BIN $version for $platform..."
  curl -fsSL "$base/$archive" -o "$tmp/$archive" || fatal "could not download $archive"

  # Verify before unpacking. A corrupted or substituted archive should fail
  # here rather than after it is on PATH.
  if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
    expected=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
    if [ -n "$expected" ]; then
      if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
      elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
      fi
      if [ -n "${actual:-}" ] && [ "$actual" != "$expected" ]; then
        fatal "checksum mismatch. Expected $expected, got $actual. Not installing."
      fi
      [ -n "${actual:-}" ] && info "Checksum verified."
    fi
  else
    info "Warning: could not fetch checksums.txt; continuing without verification."
  fi

  tar -xzf "$tmp/$archive" -C "$tmp" || fatal "could not unpack $archive"
  [ -f "$tmp/$BIN" ] || fatal "the archive did not contain $BIN"

  dir=$(choose_dir)
  mkdir -p "$dir"
  chmod +x "$tmp/$BIN"

  # macOS marks anything downloaded as quarantined, and a quarantined binary
  # refuses to run. Releases are notarised, but the attribute still has to go.
  if [ "$(uname -s)" = "Darwin" ] && command -v xattr >/dev/null 2>&1; then
    xattr -d com.apple.quarantine "$tmp/$BIN" 2>/dev/null || true
  fi

  if ! mv "$tmp/$BIN" "$dir/$BIN" 2>/dev/null; then
    info "Need elevated permission to write to $dir."
    sudo mv "$tmp/$BIN" "$dir/$BIN" || fatal "could not install to $dir"
  fi

  info ""
  info "Installed $BIN $version to $dir/$BIN"

  case ":$PATH:" in
    *":$dir:"*) info "Run: beaver auth login" ;;
    *)
      info ""
      info "$dir is not on your PATH. Add this to your shell profile:"
      info ""
      info "  export PATH=\"$dir:\$PATH\""
      ;;
  esac
}

main "$@"
