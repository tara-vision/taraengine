#!/bin/bash
#
# taraengine installer: the binary of the latest release, checked against the release's checksums.txt, and the
# service that runs it (a LaunchAgent on macOS, a systemd system service on Linux). On Linux it first installs libgomp,
# the OpenMP library that llama.cpp needs, with the system's package manager when the loader cannot find it.
# Usage: curl -fsSL https://engine.tara.vision/install.sh | bash
#
# Environment:
#   TARAENGINE_VERSION     the release to install, such as v0.3.0 (default: the latest)
#   INSTALL_DIR            where the binary goes (default: /usr/local/bin)
#   TARAENGINE_NO_SERVICE  set to 1 to install the binary only
#   TARAENGINE_RELEASES    the release page, or a mirror of it (default: https://github.com/tara-vision/taraengine/releases)
#
# Running it again updates taraengine.

set -euo pipefail

RELEASES="${TARAENGINE_RELEASES:-https://github.com/tara-vision/taraengine/releases}"
API_URL="${TARAENGINE_API:-https://api.github.com/repos/tara-vision/taraengine/releases/latest}"
SITE_URL="${TARAENGINE_SITE:-https://engine.tara.vision/api/version}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BINARY_NAME="taraengine"

info() { echo "==> $1"; }
warn() { echo "Warning: $1"; }
error() {
    echo "Error: $1" >&2
    exit 1
}

# detect_platform prints the release's name for this system: darwin-arm64, linux-amd64 or linux-arm64.
detect_platform() {
    local os arch
    case "$(uname -s)" in
        Darwin*) os="darwin" ;;
        Linux*) os="linux" ;;
        *) error "Unsupported operating system: $(uname -s); taraengine runs on Apple Silicon Macs and Linux" ;;
    esac
    case "$(uname -m)" in
        x86_64 | amd64) arch="amd64" ;;
        arm64 | aarch64) arch="arm64" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac
    if [ "$os" = "darwin" ] && [ "$arch" = "amd64" ]; then
        error "taraengine runs on Apple Silicon Macs; this Mac has an Intel processor"
    fi
    echo "${os}-${arch}"
}

# get_latest_version prints the latest release's tag. The release page's redirect needs no API call, so no rate
# limit stops it; the GitHub API and engine.tara.vision's version route are the fallbacks. It prints nothing when
# none of them answers.
get_latest_version() {
    local url tag
    url=$(curl -fsSIL -o /dev/null -w '%{url_effective}' "${RELEASES}/latest" 2>/dev/null || true)
    tag="${url##*/tag/}"
    if [ -n "$tag" ] && [ "$tag" != "$url" ]; then
        printf '%s\n' "$tag"
        return 0
    fi
    tag=$(curl -fsSL "$API_URL" 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' || true)
    if [ -n "$tag" ]; then
        printf '%s\n' "$tag"
        return 0
    fi
    tag=$(curl -fsSL "$SITE_URL" 2>/dev/null | grep -o '"version":"[^"]*"' | sed -E 's/.*:"([^"]+)"/\1/' || true)
    [ -n "$tag" ] && printf 'v%s\n' "${tag#v}"
    return 0
}

# sha256_of prints the SHA-256 of a file.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        error "Need sha256sum or shasum to verify the download"
    fi
}

# verify_checksum <file> <asset> <checksums-file> exits with an error unless the file's SHA-256 is the asset's line.
verify_checksum() {
    local file="$1" asset="$2" sums="$3" expected actual
    expected=$(grep -E "^[0-9a-f]{64}  ${asset}\$" "$sums" || true)
    expected=${expected%%[[:space:]]*}
    if [ -z "$expected" ]; then
        error "No checksum for ${asset} in checksums.txt; refusing to install a binary that cannot be verified"
    fi
    actual=$(sha256_of "$file" || true)
    if [ "$expected" != "$actual" ]; then
        error "Checksum mismatch for ${asset}: expected ${expected}, got ${actual}"
    fi
}

# before_0_3 reports whether a tag such as v0.2.0 names a release before v0.3.0, the first with the service commands. A
# tag it cannot read is left to the download.
before_0_3() {
    local v="${1#v}" major minor
    major="${v%%.*}"
    v="${v#*.}"
    minor="${v%%.*}"
    [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ ]] || return 1
    [ "$major" -eq 0 ] && [ "$minor" -lt 3 ]
}

# check_service_dir stops before anything is installed when the Linux service could not run the binary: its unit hides
# /home, /root and /run/user from the engine.
check_service_dir() {
    [ "${TARAENGINE_NO_SERVICE:-}" = "1" ] && return 0
    case "${INSTALL_DIR%/}/" in
        /home/* | /root/* | /run/user/*)
            error "the service could not run ${INSTALL_DIR%/}/${BINARY_NAME}: its unit hides /home, /root and /run/user. Choose another INSTALL_DIR, such as /usr/local/bin, or set TARAENGINE_NO_SERVICE=1 for the binary alone"
            ;;
    esac
}

# has_openmp reports whether the loader finds libgomp.so.1, the OpenMP library of GCC that llama.cpp's Linux builds
# link. ldconfig is not on every user's PATH. grep reads the whole listing: one that stopped at the match would end
# ldconfig with SIGPIPE, which pipefail takes for a missing library.
has_openmp() {
    local ldconfig
    ldconfig=$(command -v ldconfig || echo /sbin/ldconfig)
    "$ldconfig" -p 2>/dev/null | grep 'libgomp\.so\.1' >/dev/null
}

# ensure_openmp installs libgomp with the system's package manager when the loader cannot find it. It runs before
# anything else is installed, so a system that cannot get it is left as it was. A package manager that fails after it
# installed the library, as zypper does when another repository cannot be refreshed, lets the installation go on; apt
# waits up to two minutes for a lock that unattended upgrades hold on a fresh machine.
ensure_openmp() {
    local package
    has_openmp && return 0
    if command -v apt-get >/dev/null 2>&1; then
        package="libgomp1"
        info "Installing ${package}, the OpenMP library that llama.cpp needs..."
        as_root apt-get -o DPkg::Lock::Timeout=120 update -qq || true
        as_root env DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=120 install -y -qq "$package" ||
            has_openmp || openmp_failed "$package"
    elif command -v dnf >/dev/null 2>&1; then
        package="libgomp"
        info "Installing ${package}, the OpenMP library that llama.cpp needs..."
        as_root dnf install -y -q "$package" || has_openmp || openmp_failed "$package"
    elif command -v zypper >/dev/null 2>&1; then
        package="libgomp1"
        info "Installing ${package}, the OpenMP library that llama.cpp needs..."
        as_root zypper --non-interactive install "$package" || has_openmp || openmp_failed "$package"
    else
        error "taraengine needs libgomp.so.1, the OpenMP library of GCC, and this script knows no package manager on this system: install it, then run this script again"
    fi
}

openmp_failed() {
    error "Could not install $1, the OpenMP library that llama.cpp needs; install it, then run this script again"
}

# as_root runs a command as root: directly for root, through sudo otherwise.
as_root() {
    if [ "$(id -u)" = "0" ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# install_binary downloads the asset of version, checks it and puts it at INSTALL_DIR/taraengine.
install_binary() {
    local version="$1" asset="$2" base tmp staged
    base="${RELEASES}/download/${version}"
    tmp=$(mktemp -d)
    # The path is baked into the trap now: the local variable is gone by the time EXIT fires.
    trap "rm -rf '${tmp}'" EXIT
    info "Downloading ${asset} ${version}..."
    curl -fsSL "${base}/${asset}" -o "${tmp}/${BINARY_NAME}" || error "Failed to download ${base}/${asset}"
    curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" ||
        error "This release has no checksums.txt; refusing to install a binary that cannot be verified"
    info "Verifying the checksum..."
    verify_checksum "${tmp}/${BINARY_NAME}" "$asset" "${tmp}/checksums.txt"
    info "Installing to ${INSTALL_DIR}/${BINARY_NAME}..."
    if [ ! -d "$INSTALL_DIR" ]; then
        as_root mkdir -p "$INSTALL_DIR"
    fi
    # A copy with mode 0755, whatever the umask, lands next to the binary and is renamed over it, so the binary changes
    # in one step. Where root writes, root owns the copy: root and the service run this file.
    staged="${INSTALL_DIR}/.${BINARY_NAME}.new"
    if [ -w "$INSTALL_DIR" ]; then
        install -m 0755 "${tmp}/${BINARY_NAME}" "$staged"
        mv -f "$staged" "${INSTALL_DIR}/${BINARY_NAME}"
    else
        warn "Need root to write ${INSTALL_DIR}"
        as_root install -m 0755 "${tmp}/${BINARY_NAME}" "$staged"
        as_root mv -f "$staged" "${INSTALL_DIR}/${BINARY_NAME}"
    fi
    info "Installed: $("${INSTALL_DIR}/${BINARY_NAME}" version 2>/dev/null | head -1 || echo unknown)"
    if ! command -v "$BINARY_NAME" >/dev/null 2>&1; then
        warn "taraengine is not in PATH. Add ${INSTALL_DIR} to your PATH."
    elif [ "$(command -v "$BINARY_NAME")" != "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        warn "Another taraengine is first in PATH: $(command -v "$BINARY_NAME"). Run ${INSTALL_DIR}/${BINARY_NAME} or fix your PATH."
    fi
}

# install_service has the binary install its service: as the user on macOS, where the agent belongs to the user, and as
# root on Linux, where the service belongs to the machine.
install_service() {
    local os="$1" bin="${INSTALL_DIR}/${BINARY_NAME}"
    if [ "${TARAENGINE_NO_SERVICE:-}" = "1" ]; then
        info "Skipping the service: run \`taraengine serve\` in a terminal, or \`taraengine service install\` later"
        return 0
    fi
    info "Installing the service..."
    if [ "$os" = "darwin" ]; then
        if [ "$(id -u)" != "0" ]; then
            "$bin" service install || service_failed
        elif [ -n "${SUDO_USER:-}" ]; then
            sudo -u "$SUDO_USER" "$bin" service install || service_failed
        else
            warn "The service belongs to a user on macOS: run \`taraengine service install\` as that user"
        fi
        return 0
    fi
    as_root "$bin" service install || service_failed
}

service_failed() {
    error "the binary is installed, but the service is not; \`taraengine service install\` tries again"
}

main() {
    local platform version
    platform=$(detect_platform)
    info "Detected: ${platform}"
    if [ "${platform%-*}" = "linux" ]; then
        check_service_dir
    fi
    version="${TARAENGINE_VERSION:-}"
    if [ -z "$version" ]; then
        info "Looking up the latest release..."
        version=$(get_latest_version || true)
    fi
    [ -n "$version" ] || error "Could not determine the version to install"
    info "Version: ${version}"
    if [ "${TARAENGINE_NO_SERVICE:-}" != "1" ] && before_0_3 "$version"; then
        error "taraengine ${version} has no service commands, which came with v0.3.0: set TARAENGINE_VERSION to v0.3.0 or later, or TARAENGINE_NO_SERVICE=1 for the binary alone"
    fi
    if [ "${platform%-*}" = "linux" ]; then
        ensure_openmp
    fi
    install_binary "$version" "taraengine-${platform}"
    install_service "${platform%-*}"
    echo ""
    info "Done. Next, pull a model:"
    echo "    taraengine pull gemma4:12b"
    echo ""
    echo "  Documentation: https://engine.tara.vision"
}

main "$@"
