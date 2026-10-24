#!/bin/sh
# Install aic: https://github.com/ai4-labs/aic
#
#   curl -fsSL https://raw.githubusercontent.com/ai4-labs/aic/main/install.sh | sh
#
# Environment:
#   AIC_VERSION      release to install, e.g. v0.1.0 (default: latest)
#   AIC_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   AIC_DOWNLOAD_URL base URL of the release assets (for mirrors and testing)
set -eu

repo="ai4-labs/aic"
version="${AIC_VERSION:-latest}"
install_dir="${AIC_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*" >&2; }
fail() { say "aic install: $*"; exit 1; }

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "unsupported OS $(uname -s); aic runs on macOS and Linux" ;;
esac
case "$(uname -m)" in
arm64 | aarch64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*) fail "unsupported CPU architecture $(uname -m)" ;;
esac
if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
	fail "macOS on Intel is not supported yet (aic's build VM needs Apple silicon)"
fi

if [ -n "${AIC_DOWNLOAD_URL:-}" ]; then
	base="$AIC_DOWNLOAD_URL"
elif [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	base="https://github.com/$repo/releases/download/$version"
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	fail "curl or wget is required"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	fail "sha256sum or shasum is required to verify the download"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

asset="aic_${os}_${arch}.tar.gz"
say "Downloading $asset ($version)..."
fetch "$base/$asset" "$tmp/$asset" || fail "download failed: $base/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

want="$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "no checksum for $asset in checksums.txt"
got="$(sha256 "$tmp/$asset")"
[ "$got" = "$want" ] || fail "checksum mismatch for $asset: got $got, want $want"

tar -xzf "$tmp/$asset" -C "$tmp" aic
mkdir -p "$install_dir"
# Install via a temporary name so a running aic is never half-written.
cp "$tmp/aic" "$install_dir/.aic.new"
chmod 0755 "$install_dir/.aic.new"
mv -f "$install_dir/.aic.new" "$install_dir/aic"

say "Installed $("$install_dir/aic" version | head -n 1) to $install_dir/aic"
case ":$PATH:" in
*":$install_dir:"*) ;;
*) say "Add $install_dir to your PATH, e.g.: echo 'export PATH=\"$install_dir:\$PATH\"' >> ~/.profile" ;;
esac
if [ "$os" = linux ]; then
	say "RUN needs unprivileged user namespaces; for users other than root, install newuidmap/newgidmap and add /etc/subuid and /etc/subgid entries."
fi
