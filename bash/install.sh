#!/usr/bin/env bash

set -euo pipefail

REPO_OWNER="${ROBOTNIK_REPO_OWNER:-walrusk}"
REPO_NAME="${ROBOTNIK_REPO_NAME:-robotnik}"
REF="${ROBOTNIK_REF:-main}"
SOURCE_URL="${ROBOTNIK_URL:-https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${REF}/bash/robotnik}"

die() {
  echo "robotnik install: $*" >&2
  exit 1
}

have() {
  command -v "$1" >/dev/null 2>&1
}

path_contains() {
  local dir="$1"
  case ":${PATH:-}:" in
    *":$dir:"*) return 0 ;;
    *) return 1 ;;
  esac
}

first_writable_path_dir() {
  local path_dir
  local old_ifs="$IFS"
  IFS=:
  for path_dir in ${PATH:-}; do
    [[ -n "$path_dir" ]] || continue
    [[ "$path_dir" = "." ]] && continue
    [[ -d "$path_dir" && -w "$path_dir" ]] || continue
    case "$path_dir" in
      /bin|/sbin|/usr/bin|/usr/sbin) continue ;;
    esac
    IFS="$old_ifs"
    printf '%s\n' "$path_dir"
    return 0
  done
  IFS="$old_ifs"
  return 1
}

choose_install_dir() {
  if [[ -n "${ROBOTNIK_INSTALL_DIR:-}" ]]; then
    printf '%s\n' "$ROBOTNIK_INSTALL_DIR"
    return
  fi

  if first_writable_path_dir; then
    return
  fi

  if path_contains "$HOME/.local/bin"; then
    printf '%s\n' "$HOME/.local/bin"
    return
  fi

  if path_contains "$HOME/bin"; then
    printf '%s\n' "$HOME/bin"
    return
  fi

  printf '%s\n' "$HOME/.local/bin"
}

download() {
  local url="$1"
  local output="$2"

  if have curl; then
    curl -fsSL "$url" -o "$output"
    return
  fi

  if have wget; then
    wget -qO "$output" "$url"
    return
  fi

  die "curl or wget is required to download robotnik"
}

install_dir="$(choose_install_dir)"
install_path="$install_dir/robotnik"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/robotnik-install.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT
download_path="$tmp_dir/robotnik"

echo "Downloading robotnik from $SOURCE_URL"
download "$SOURCE_URL" "$download_path"

head -n 1 "$download_path" | grep -q 'bash' || die "downloaded file does not look like the robotnik script"

mkdir -p "$install_dir"
install -m 0755 "$download_path" "$install_path"

echo "Installed robotnik to $install_path"

if ! path_contains "$install_dir"; then
  cat <<EOF

$install_dir is not currently on your PATH.
Add this line to your shell profile, then restart your shell:

  export PATH="$install_dir:\$PATH"
EOF
fi

if ! have jq; then
  cat <<'EOF'

Warning: jq is required to run robotnik.
Install jq with your system package manager before using robotnik.
EOF
fi

if ! have codex && [[ -z "${ROBOTNIK_AI_CMD:-}" ]]; then
  cat <<'EOF'

Warning: codex was not found on PATH.
Install the Codex CLI or set ROBOTNIK_AI_CMD to a custom generator command.
EOF
fi

echo
echo "Try it:"
echo "  robotnik show the largest files under this repo"
