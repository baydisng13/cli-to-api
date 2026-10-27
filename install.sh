#!/bin/sh
# install.sh — install cli-to-api on macOS or Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.sh | sh
#
# Environment variables:
#   CLI_TO_API_VERSION      tag to install (e.g. v0.1.0). Default: latest release.
#   CLI_TO_API_INSTALL_DIR  directory to install into. Default: /usr/local/bin if
#                           writable, otherwise $HOME/.local/bin.
#
# This script never invokes sudo. If it cannot write to /usr/local/bin it falls
# back to a user-owned directory and tells you how to put it on PATH.

set -eu

REPO="baydisng13/cli-to-api"
BINARY="cli-to-api"

TMPDIR_CLI=""

cleanup() {
	# Only ever removes a directory this script created with mktemp.
	if [ -n "$TMPDIR_CLI" ] && [ -d "$TMPDIR_CLI" ]; then
		rm -rf "$TMPDIR_CLI"
	fi
}
trap cleanup EXIT HUP INT TERM

info() { printf '%s\n' "$*"; }
err() { printf 'error: %s\n' "$*" >&2; }

die() {
	err "$*"
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# ---------------------------------------------------------------- platform ---

detect_platform() {
	os_raw="$(uname -s)"
	arch_raw="$(uname -m)"

	case "$os_raw" in
	Darwin) OS="darwin" ;;
	Linux) OS="linux" ;;
	*)
		die "unsupported operating system: ${os_raw}. cli-to-api ships prebuilt binaries for macOS and Linux only; on other systems install with: go install github.com/${REPO}/cmd/${BINARY}@latest"
		;;
	esac

	case "$arch_raw" in
	x86_64 | amd64) ARCH="amd64" ;;
	aarch64 | arm64) ARCH="arm64" ;;
	*)
		die "unsupported architecture: ${arch_raw}. Prebuilt binaries exist for amd64 and arm64 only; on other architectures install with: go install github.com/${REPO}/cmd/${BINARY}@latest"
		;;
	esac
}

# ---------------------------------------------------------------- download ---

# download URL DEST
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --proto '=https' --tlsv1.2 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "need either curl or wget to download files"
	fi
}

# fetch URL -> stdout
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --proto '=https' --tlsv1.2 "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O - "$1"
	else
		die "need either curl or wget to download files"
	fi
}

resolve_version() {
	if [ -n "${CLI_TO_API_VERSION:-}" ]; then
		VERSION="$CLI_TO_API_VERSION"
		info "Using requested version: ${VERSION}"
		return
	fi

	info "Resolving latest release of ${REPO}..."
	api_json="$(fetch "https://api.github.com/repos/${REPO}/releases/latest" || true)"
	VERSION="$(
		printf '%s' "$api_json" |
			sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
			head -n 1
	)"

	if [ -z "$VERSION" ]; then
		die "could not determine the latest release of ${REPO}. Either the repository has no published releases yet, or the GitHub API rate-limited this request. Set CLI_TO_API_VERSION=vX.Y.Z to install a specific tag, or build from source with: go install github.com/${REPO}/cmd/${BINARY}@latest"
	fi

	# Guard against anything surprising coming back from the network before it
	# gets interpolated into a URL or a filename.
	case "$VERSION" in
	v[0-9]*) ;;
	*) die "refusing to use suspicious version string from GitHub API: '${VERSION}'" ;;
	esac
	case "$VERSION" in
	*[!A-Za-z0-9.+_-]*) die "refusing to use version string with unexpected characters: '${VERSION}'" ;;
	esac

	info "Latest release: ${VERSION}"
}

# ------------------------------------------------------------------ verify ---

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "need sha256sum or shasum to verify the download; refusing to install unverified binaries"
	fi
}

verify_checksum() {
	archive_path="$1"
	archive_name="$2"
	sums_path="$3"

	expected="$(
		awk -v name="$archive_name" '
			{
				f = $NF
				sub(/^\*/, "", f)
				sub(/^.*\//, "", f)
				if (f == name) { print $1; exit }
			}
		' "$sums_path"
	)"

	[ -n "$expected" ] || die "no checksum entry for ${archive_name} in checksums.txt"

	actual="$(sha256_of "$archive_path")"

	if [ "$expected" != "$actual" ]; then
		err "CHECKSUM MISMATCH for ${archive_name}"
		err "  expected: ${expected}"
		err "  actual:   ${actual}"
		die "aborting: the downloaded archive does not match the published checksum"
	fi

	info "Checksum OK (sha256 ${actual})"
}

# ----------------------------------------------------------- install dir -----

choose_install_dir() {
	if [ -n "${CLI_TO_API_INSTALL_DIR:-}" ]; then
		INSTALL_DIR="$CLI_TO_API_INSTALL_DIR"
		mkdir -p "$INSTALL_DIR" || die "cannot create install dir: ${INSTALL_DIR}"
		[ -w "$INSTALL_DIR" ] || die "install dir is not writable: ${INSTALL_DIR}"
		return
	fi

	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		INSTALL_DIR="/usr/local/bin"
		return
	fi

	info "/usr/local/bin is not writable by $(id -un) (installing there would need root)."
	info "Falling back to a user-owned directory instead of escalating privileges."
	INSTALL_DIR="${HOME}/.local/bin"
	mkdir -p "$INSTALL_DIR" || die "cannot create install dir: ${INSTALL_DIR}"
}

on_path() {
	case ":${PATH}:" in
	*":$1:"*) return 0 ;;
	*) return 1 ;;
	esac
}

rc_file_hint() {
	case "${SHELL:-}" in
	*/zsh) printf '%s' "${HOME}/.zshrc" ;;
	*/bash) printf '%s' "${HOME}/.bashrc" ;;
	*/fish) printf '%s' "${HOME}/.config/fish/config.fish" ;;
	*) printf '%s' "your shell's startup file" ;;
	esac
}

# -------------------------------------------------------------------- main ---

main() {
	need uname
	need tar
	need mktemp
	need awk

	detect_platform
	resolve_version

	ARCHIVE="${BINARY}_${VERSION}_${OS}_${ARCH}.tar.gz"
	BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

	TMPDIR_CLI="$(mktemp -d "${TMPDIR:-/tmp}/cli-to-api-install.XXXXXX")" ||
		die "could not create a temporary directory"

	info "Downloading ${ARCHIVE}..."
	download "${BASE_URL}/${ARCHIVE}" "${TMPDIR_CLI}/${ARCHIVE}" ||
		die "download failed: ${BASE_URL}/${ARCHIVE} (is there a ${OS}/${ARCH} build for ${VERSION}?)"

	info "Downloading checksums.txt..."
	download "${BASE_URL}/checksums.txt" "${TMPDIR_CLI}/checksums.txt" ||
		die "download failed: ${BASE_URL}/checksums.txt"

	[ -s "${TMPDIR_CLI}/${ARCHIVE}" ] || die "downloaded archive is empty: ${ARCHIVE}"
	[ -s "${TMPDIR_CLI}/checksums.txt" ] || die "downloaded checksums.txt is empty"

	verify_checksum "${TMPDIR_CLI}/${ARCHIVE}" "${ARCHIVE}" "${TMPDIR_CLI}/checksums.txt"

	# Sanity-check that this really is a gzip tarball before handing it to tar.
	if ! tar -tzf "${TMPDIR_CLI}/${ARCHIVE}" >/dev/null 2>&1; then
		die "downloaded file is not a readable .tar.gz archive: ${ARCHIVE}"
	fi

	mkdir -p "${TMPDIR_CLI}/extract"
	tar -xzf "${TMPDIR_CLI}/${ARCHIVE}" -C "${TMPDIR_CLI}/extract" ||
		die "failed to extract ${ARCHIVE}"

	[ -f "${TMPDIR_CLI}/extract/${BINARY}" ] ||
		die "archive did not contain a '${BINARY}' binary"

	chmod +x "${TMPDIR_CLI}/extract/${BINARY}"

	choose_install_dir

	info "Installing to ${INSTALL_DIR}/${BINARY}"
	# cp then mv within the same filesystem keeps the replace close to atomic
	# and avoids ETXTBSY if the old binary is running.
	cp "${TMPDIR_CLI}/extract/${BINARY}" "${INSTALL_DIR}/.${BINARY}.new" ||
		die "could not write to ${INSTALL_DIR}"
	chmod 755 "${INSTALL_DIR}/.${BINARY}.new"

	# The macOS binaries are not signed or notarized. A file fetched by curl
	# does not normally carry the quarantine attribute, but clear it anyway so
	# the first run cannot trip Gatekeeper. Best effort: xattr is absent on
	# Linux and a failure here must not abort a working install.
	if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
		xattr -d com.apple.quarantine "${INSTALL_DIR}/.${BINARY}.new" 2>/dev/null || true
	fi

	mv -f "${INSTALL_DIR}/.${BINARY}.new" "${INSTALL_DIR}/${BINARY}" ||
		die "could not install to ${INSTALL_DIR}/${BINARY}"

	info ""
	info "Installed: ${INSTALL_DIR}/${BINARY}"
	info "Version:   $("${INSTALL_DIR}/${BINARY}" version 2>/dev/null || echo '(could not run binary)')"

	if ! on_path "$INSTALL_DIR"; then
		info ""
		info "${INSTALL_DIR} is not on your PATH. Add this line to $(rc_file_hint):"
		info ""
		info "    export PATH=\"${INSTALL_DIR}:\$PATH\""
		info ""
		info "Then restart your shell, or run it once in this session."
	fi
}

main "$@"
