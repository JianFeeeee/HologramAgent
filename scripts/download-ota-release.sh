#!/usr/bin/env bash
# Download the latest OTA-capable Linux package from the repository GitHub Release.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO="${HOLOGRAM_RELEASE_REPO:-JianFeeeee/HologramAgent}"
API="https://api.github.com/repos/${REPO}/releases/latest"
OUT_DIR="$ROOT/build/package"
mkdir -p "$OUT_DIR"

command -v curl >/dev/null || {
	echo "ERROR: curl is required" >&2
	exit 1
}
command -v jq >/dev/null || {
	echo "ERROR: jq is required" >&2
	exit 1
}

release=$(curl --fail --silent --show-error -H 'Accept: application/vnd.github+json' "$API")
tag=$(printf '%s' "$release" | jq -r '.tag_name // empty')
release_url=$(printf '%s' "$release" | jq -r '.html_url // empty')
asset_name=$(printf '%s' "$release" | jq -r '[.assets[]?.name | select(test("linux_amd64\\.tar\\.gz$"; "i"))][0] // empty')
asset_url=$(printf '%s' "$release" | jq -r --arg name "$asset_name" '.assets[]? | select(.name == $name) | .browser_download_url' | head -1)
sums_url=$(printf '%s' "$release" | jq -r '.assets[]? | select(.name == "SHA256SUMS") | .browser_download_url' | head -1)

if [[ -z "$tag" || -z "$asset_name" || -z "$asset_url" || -z "$sums_url" ]]; then
	echo "ERROR: latest release must contain a Linux amd64 tar.gz asset and SHA256SUMS: $API" >&2
	exit 1
fi

archive="$OUT_DIR/$asset_name"
sums="$OUT_DIR/SHA256SUMS-$tag"
metadata="$OUT_DIR/${asset_name%.tar.gz}.release.txt"

echo "Release: $release_url"
echo "Downloading: $asset_name"
curl --fail --location --silent --show-error "$asset_url" -o "$archive"
curl --fail --location --silent --show-error "$sums_url" -o "$sums"

expected=$(awk -v name="$asset_name" '$2 == name || $2 == "*" name || $2 == "./" name || $2 == "./" name {print $1; exit}' "$sums")
if [[ -z "$expected" ]]; then
	echo "ERROR: $asset_name has no SHA256 entry in the Release checksums" >&2
	exit 1
fi
actual=$(sha256sum "$archive" | awk '{print $1}')
if [[ "$expected" != "$actual" ]]; then
	echo "ERROR: SHA256 mismatch for $asset_name" >&2
	echo "expected: $expected" >&2
	echo "actual:   $actual" >&2
	exit 1
fi

{
	printf 'release_tag=%s\n' "$tag"
	printf 'release_url=%s\n' "$release_url"
	printf 'asset_name=%s\n' "$asset_name"
	printf 'asset_url=%s\n' "$asset_url"
	printf 'sha256=%s\n' "$actual"
} >"$metadata"

printf '%s  %s\n' "$actual" "$(basename "$archive")" >"$archive.sha256"
echo "OTA package: $archive"
echo "SHA256: $actual"
echo "Metadata: $metadata"
