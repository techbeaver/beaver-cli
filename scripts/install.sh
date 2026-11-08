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

# What a genuine release is signed as. The certificate identity is the exact
# release workflow on the exact tag being installed, so a signature from any
# other workflow, or from another tag, is refused. The issuer is GitHub Actions.
SIGNER_WORKFLOW='https://github.com/techbeaver/beaver-cli/.github/workflows/release.yml@refs/tags/'
SIGNER_ISSUER='https://token.actions.githubusercontent.com'

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

# sha256_of prints the SHA-256 of a file, or fails if this machine has no tool
# that can compute one. Failing is deliberate: an installer that cannot verify
# does not install.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    return 1
  fi
}

# expected_sum prints the one checksum listed for a file, and fails unless the
# file is listed exactly once with a well-formed SHA-256.
expected_sum() {
  matches=$(awk -v f="$2" '$2 == f || $2 == "*" f { print $1 }' "$1")
  count=$(printf '%s\n' "$matches" | grep -c . || true)
  [ "$count" -eq 1 ] || return 1
  case "$matches" in
    *[!0-9a-f]*) return 1 ;;
  esac
  [ "${#matches}" -eq 64 ] || return 1
  printf '%s' "$matches"
}

# verify_signature checks that checksums.txt was produced by this repository's
# release workflow. Without cosign it says so loudly and carries on only if the
# caller has not demanded a signature: BEAVER_REQUIRE_SIGNATURE=1 makes a
# missing cosign fatal, which is what an automated install should set.
verify_signature() {
  if ! command -v cosign >/dev/null 2>&1; then
    if [ "${BEAVER_REQUIRE_SIGNATURE:-}" = "1" ]; then
      fatal "BEAVER_REQUIRE_SIGNATURE=1 is set but cosign is not installed, so the release signature cannot be verified. Install cosign (https://docs.sigstore.dev/) and run this again."
    fi
    info ""
    info "WARNING: cosign is not installed, so the release signature was NOT verified."
    info "The checksum below proves the download is intact. It does not prove who built it."
    info "Install cosign to check the signature, or set BEAVER_REQUIRE_SIGNATURE=1 to refuse to continue without it."
    info ""
    return 0
  fi

  curl -fsSL "$base/checksums.txt.sig" -o "$tmp/checksums.txt.sig" || fatal "could not download checksums.txt.sig. Not installing."
  curl -fsSL "$base/checksums.txt.pem" -o "$tmp/checksums.txt.pem" || fatal "could not download checksums.txt.pem. Not installing."
  cosign verify-blob \
    --certificate "$tmp/checksums.txt.pem" \
    --signature "$tmp/checksums.txt.sig" \
    --certificate-identity "${SIGNER_WORKFLOW}${version}" \
    --certificate-oidc-issuer "$SIGNER_ISSUER" \
    "$tmp/checksums.txt" >/dev/null \
    || fatal "the signature on checksums.txt did not verify against this repository's release workflow. Not installing."
  info "Signature verified."
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
  case "$version" in v*) ;; *) version="v$version" ;; esac
  # The version goes into a URL and a file name, so it is checked rather than trusted.
  printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' \
    || fatal "that is not a release version: $version"
  number=${version#v}

  archive="${BIN}_${number}_${platform}.tar.gz"
  base="https://github.com/$REPO/releases/download/$version"

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT INT TERM

  info "Downloading $BIN $version for $platform..."
  curl -fsSL "$base/$archive" -o "$tmp/$archive" || fatal "could not download $archive"
  curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || fatal "could not download checksums.txt, so the download cannot be verified. Not installing."

  verify_signature

  # Verify before unpacking. A corrupted or substituted archive should fail
  # here rather than after it is on PATH, and a missing checksum is a failure,
  # never a reason to carry on.
  expected=$(expected_sum "$tmp/checksums.txt" "$archive") \
    || fatal "checksums.txt does not list $archive exactly once with a valid SHA-256. Not installing."
  actual=$(sha256_of "$tmp/$archive") \
    || fatal "this machine has neither sha256sum nor shasum, so the download cannot be verified. Not installing."
  [ -n "$actual" ] || fatal "could not compute the checksum of $archive. Not installing."
  [ "$actual" = "$expected" ] || fatal "checksum mismatch. Expected $expected, got $actual. Not installing."
  info "Checksum verified."

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
