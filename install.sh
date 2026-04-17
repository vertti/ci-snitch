#!/bin/sh
set -eu

REPO="vertti/ci-snitch"
BINARY="ci-snitch"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

get_latest_version() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
    grep '"tag_name"' | sed -E 's/.*"v([^"]+)".*/\1/'
}

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
    *) echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
}

main() {
  VERSION="${1:-$(get_latest_version)}"
  OS="$(detect_os)"
  ARCH="$(detect_arch)"

  if [ -z "$VERSION" ]; then
    echo "Error: could not determine latest version" >&2
    exit 1
  fi

  EXT="tar.gz"
  if [ "$OS" = "windows" ]; then
    EXT="zip"
  fi

  FILENAME="${BINARY}_${VERSION}_${OS}_${ARCH}.${EXT}"
  URL="https://github.com/${REPO}/releases/download/v${VERSION}/${FILENAME}"

  CHECKSUMS_URL="https://github.com/${REPO}/releases/download/v${VERSION}/checksums.txt"

  TMPDIR="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR"' EXIT

  echo "Downloading ${BINARY} v${VERSION} for ${OS}/${ARCH}..."
  curl -fsSL "$URL" -o "${TMPDIR}/${FILENAME}"
  curl -fsSL "$CHECKSUMS_URL" -o "${TMPDIR}/checksums.txt"

  echo "Verifying checksum..."
  cd "$TMPDIR"
  grep "$FILENAME" checksums.txt | shasum -a 256 -c - >/dev/null 2>&1 || {
    echo "Error: checksum verification failed for ${FILENAME}" >&2
    exit 1
  }
  cd - >/dev/null

  echo "Extracting..."
  if [ "$EXT" = "zip" ]; then
    unzip -q "${TMPDIR}/${FILENAME}" -d "$TMPDIR"
  else
    tar -xzf "${TMPDIR}/${FILENAME}" -C "$TMPDIR"
  fi

  echo "Installing to ${INSTALL_DIR}/${BINARY}..."
  if [ -w "$INSTALL_DIR" ]; then
    mv "${TMPDIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
  else
    sudo mv "${TMPDIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
  fi
  chmod +x "${INSTALL_DIR}/${BINARY}"

  echo "Installed ${BINARY} v${VERSION} to ${INSTALL_DIR}/${BINARY}"
}

main "$@"
