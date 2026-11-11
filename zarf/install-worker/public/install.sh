#!/bin/sh
set -eu

fail() {
  printf 'Borq install: %s\n' "$*" >&2
  exit 1
}

install_borq() {
  tag='__BORQ_TAG__'
  version=${tag#v}
  install_directory=${BORQ_INSTALL_DIR:-"$HOME/.local/bin"}
  [ ! -d "$install_directory/borq" ] || fail "The install destination $install_directory/borq is a directory."

  case "$(uname -s)/$(uname -m)" in
    Linux/x86_64|Linux/amd64) platform=linux_amd64 ;;
    Darwin/arm64|Darwin/aarch64) platform=darwin_arm64 ;;
    *) fail "No release binary is available for $(uname -s)/$(uname -m). Supported platforms: Linux AMD64 and macOS Apple Silicon." ;;
  esac
  for utility in curl tar awk mktemp; do
    command -v "$utility" >/dev/null 2>&1 || fail "$utility is required."
  done
  if command -v sha256sum >/dev/null 2>&1; then
    checksum_command=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    checksum_command=shasum
  else
    fail "sha256sum or shasum is required."
  fi

  temporary_directory=$(mktemp -d)
  staged_binary=''
  trap 'rm -rf "$temporary_directory"; if [ -n "$staged_binary" ]; then rm -f "$staged_binary"; fi' 0
  trap 'exit 1' HUP INT TERM

  archive_name="borq_$version"_"$platform.tar.gz"
  download_base="https://github.com/stefanmcshane/borq/releases/download/$tag"
  printf 'Downloading Borq %s for %s...\n' "$tag" "$platform"
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location \
    --retry 3 --connect-timeout 10 --max-time 120 \
    --output "$temporary_directory/archive.tar.gz" "$download_base/$archive_name"
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location \
    --retry 3 --connect-timeout 10 --max-time 120 \
    --output "$temporary_directory/checksums.txt" "$download_base/checksums.txt"

  expected=$(awk -v filename="$archive_name" '$2 == filename { print $1 }' "$temporary_directory/checksums.txt")
  case "$expected" in
    ''|*[!0-9a-fA-F]*) fail "A valid checksum for $archive_name was not found." ;;
  esac
  [ "${#expected}" -eq 64 ] || fail "A valid checksum for $archive_name was not found."
  if [ "$checksum_command" = sha256sum ]; then
    actual=$(sha256sum "$temporary_directory/archive.tar.gz" | awk '{ print $1 }')
  else
    actual=$(shasum -a 256 "$temporary_directory/archive.tar.gz" | awk '{ print $1 }')
  fi
  [ "$actual" = "$expected" ] || fail "Checksum verification failed; the existing Borq binary was not changed."

  tar -xzf "$temporary_directory/archive.tar.gz" -C "$temporary_directory" borq
  [ -f "$temporary_directory/borq" ] && [ ! -L "$temporary_directory/borq" ] || fail "The release archive does not contain a regular Borq binary."
  mkdir -p "$install_directory"
  staged_binary=$(mktemp "$install_directory/.borq.XXXXXX")
  cp "$temporary_directory/borq" "$staged_binary"
  chmod 755 "$staged_binary"
  mv -f "$staged_binary" "$install_directory/borq"
  staged_binary=''

  printf 'Installed Borq %s to %s/borq\n' "$tag" "$install_directory"
  case ":$PATH:" in
    *":$install_directory:"*) ;;
    *) printf 'Add %s to your PATH to run borq.\n' "$install_directory" ;;
  esac
}

# Parse the whole function before downloading or replacing files when piped to sh.
install_borq
