#!/bin/sh
# update-formula.sh — point Formula/cli-to-api.rb at a published
# release.
#
#   packaging/homebrew/update-formula.sh v0.2.0
#
# It downloads that release's checksums.txt from GitHub, pulls out the four
# sha256 values for the darwin/linux archives, and rewrites the `version` and
# `sha256` stanzas of the formula in place. Nothing is pushed anywhere — copy
# the result into the tap repo yourself (see docs/PACKAGING.md).
#
# This is deliberately a LOCAL, human-run script: publishing to the tap repo
# needs a cross-repo push token that this project does not have configured.
#
# Requirements: curl or wget, awk, mktemp.

set -eu

REPO="baydisng13/cli-to-api"

# Resolve the formula next to this script, so the cwd does not matter.
SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# The formula lives at the repository root, not next to this script: a
# Homebrew tap only finds formulae in a top-level Formula/ directory, and
# keeping it there is what lets this repo be its own tap instead of needing a
# separate homebrew-tap repository.
FORMULA="${SCRIPT_DIR}/../../Formula/cli-to-api.rb"

TMPDIR_FORMULA=""

cleanup() {
	if [ -n "$TMPDIR_FORMULA" ] && [ -d "$TMPDIR_FORMULA" ]; then
		rm -rf "$TMPDIR_FORMULA"
	fi
}
trap cleanup EXIT HUP INT TERM

info() { printf '%s\n' "$*"; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

usage() {
	printf '%s\n' "usage: $0 <tag>    e.g. $0 v0.2.0" >&2
	exit 2
}

[ "$#" -eq 1 ] || usage
TAG="$1"

# The tag is interpolated into a URL and into the formula, so validate it hard
# before it is used for anything.
case "$TAG" in
v[0-9]*) ;;
*) die "tag must start with 'v' followed by a digit, got: '${TAG}'" ;;
esac
case "$TAG" in
*[!A-Za-z0-9.+_-]*) die "tag contains unexpected characters: '${TAG}'" ;;
esac

# Homebrew wants the bare semver; the release assets keep the leading "v".
BARE_VERSION="${TAG#v}"
[ -n "$BARE_VERSION" ] || die "empty version after stripping 'v' from '${TAG}'"

[ -f "$FORMULA" ] || die "formula not found: ${FORMULA}"

# download URL DEST
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --proto '=https' --tlsv1.2 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "need either curl or wget"
	fi
}

TMPDIR_FORMULA="$(mktemp -d "${TMPDIR:-/tmp}/cli-to-api-formula.XXXXXX")" ||
	die "could not create a temporary directory"

SUMS="${TMPDIR_FORMULA}/checksums.txt"
SUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/checksums.txt"

info "Fetching ${SUMS_URL}"
download "$SUMS_URL" "$SUMS" ||
	die "could not download checksums.txt for ${TAG}. Does that release exist, and did the release workflow finish?"
[ -s "$SUMS" ] || die "downloaded checksums.txt is empty"

# sha_for <archive-basename> -> sha256 on stdout
sha_for() {
	_name="$1"
	_sha="$(
		awk -v name="$_name" '
			{
				f = $NF
				sub(/^\*/, "", f)       # sha256sum binary-mode marker
				sub(/^.*\//, "", f)     # defensive: strip any directory part
				if (f == name) { print $1; exit }
			}
		' "$SUMS"
	)"
	[ -n "$_sha" ] || die "no checksum entry for ${_name} in checksums.txt for ${TAG}"
	# A sha256 is exactly 64 lowercase hex characters. Anything else means the
	# file is not what we think it is; refuse rather than writing it into a
	# formula that Homebrew will trust.
	case "$_sha" in
	*[!0-9a-f]*) die "checksum for ${_name} is not lowercase hex: '${_sha}'" ;;
	esac
	[ "${#_sha}" -eq 64 ] || die "checksum for ${_name} is not 64 chars: '${_sha}'"
	printf '%s' "$_sha"
}

SHA_DARWIN_ARM64="$(sha_for "cli-to-api_${TAG}_darwin_arm64.tar.gz")"
SHA_DARWIN_AMD64="$(sha_for "cli-to-api_${TAG}_darwin_amd64.tar.gz")"
SHA_LINUX_ARM64="$(sha_for "cli-to-api_${TAG}_linux_arm64.tar.gz")"
SHA_LINUX_AMD64="$(sha_for "cli-to-api_${TAG}_linux_amd64.tar.gz")"

info "  darwin_arm64 ${SHA_DARWIN_ARM64}"
info "  darwin_amd64 ${SHA_DARWIN_AMD64}"
info "  linux_arm64  ${SHA_LINUX_ARM64}"
info "  linux_amd64  ${SHA_LINUX_AMD64}"

# Rewrite the formula. Each `sha256 "..."` line is matched to the archive named
# on the nearest preceding `url` line, so this is idempotent and does not
# depend on the order of the on_macos/on_linux blocks.
OUT="${TMPDIR_FORMULA}/cli-to-api.rb"

awk \
	-v version="$BARE_VERSION" \
	-v d_arm="$SHA_DARWIN_ARM64" \
	-v d_amd="$SHA_DARWIN_AMD64" \
	-v l_arm="$SHA_LINUX_ARM64" \
	-v l_amd="$SHA_LINUX_AMD64" \
	'
	/^[[:space:]]*version[[:space:]]+"/ {
		match($0, /^[[:space:]]*/)
		indent = substr($0, 1, RLENGTH)
		print indent "version \"" version "\""
		next
	}
	/^[[:space:]]*url[[:space:]]+"/ {
		slot = ""
		if ($0 ~ /_darwin_arm64\.tar\.gz/) slot = d_arm
		else if ($0 ~ /_darwin_amd64\.tar\.gz/) slot = d_amd
		else if ($0 ~ /_linux_arm64\.tar\.gz/)  slot = l_arm
		else if ($0 ~ /_linux_amd64\.tar\.gz/)  slot = l_amd
		pending = slot
		print
		next
	}
	/^[[:space:]]*sha256[[:space:]]+"/ {
		if (pending != "") {
			match($0, /^[[:space:]]*/)
			indent = substr($0, 1, RLENGTH)
			print indent "sha256 \"" pending "\""
			pending = ""
			next
		}
		print
		next
	}
	{ print }
	' "$FORMULA" >"$OUT" || die "failed to rewrite the formula"

# Refuse to install a formula that still has an unreplaced placeholder in it.
if grep -q 'REPLACE_ME' "$OUT"; then
	die "the rewritten formula still contains a REPLACE_ME placeholder; check that the url lines match the expected archive names"
fi

if command -v ruby >/dev/null 2>&1; then
	ruby -c "$OUT" >/dev/null || die "the rewritten formula is not valid Ruby; leaving ${FORMULA} untouched"
fi

# Only now clobber the original.
cp "$OUT" "$FORMULA" || die "could not write ${FORMULA}"

info ""
info "Updated ${FORMULA} to ${TAG}."
info ""
info "Next (see docs/PACKAGING.md):"
info "  git add Formula/cli-to-api.rb"
info "  git commit -m 'cli-to-api ${TAG}' && git push"
