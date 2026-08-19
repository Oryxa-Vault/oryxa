#!/bin/sh
# Installs oryxa.
#
#   curl -fsSL https://oryxa.in/install.sh | sh
#
# It downloads one released binary, checks it against the published checksums,
# and puts it in ~/.local/bin. Nothing is compiled, no package manager is
# involved, and it never asks for sudo — if it cannot write where it is pointed,
# it says so and stops rather than escalating.
#
#   ORYXA_VERSION   install this tag instead of the latest release
#   ORYXA_BIN_DIR   install here instead of ~/.local/bin
set -eu

REPO="Oryxa-Vault/oryxa"
BIN="oryxa"

main() {
    need curl
    need tar

    target=$(target)
    version=${ORYXA_VERSION:-$(latest)}
    dir=${ORYXA_BIN_DIR:-$HOME/.local/bin}
    archive="$BIN-$version-$target.tar.gz"
    base="https://github.com/$REPO/releases/download/$version"

    say "oryxa $version for $target"

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT INT TERM

    fetch "$base/$archive" "$tmp/$archive" ||
        die "no build for $target in $version.
  The release page lists what was built: https://github.com/$REPO/releases
  Building from source works everywhere: cargo install --git https://github.com/$REPO oryxa"
    fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" ||
        die "release $version has no SHA256SUMS, so the download cannot be checked"

    verify "$tmp" "$archive"
    tar -xzf "$tmp/$archive" -C "$tmp"
    [ -f "$tmp/$BIN" ] || die "$archive did not contain $BIN"

    mkdir -p "$dir" 2>/dev/null || die "cannot create $dir — set ORYXA_BIN_DIR to somewhere you can write"
    [ -w "$dir" ] || die "cannot write to $dir — set ORYXA_BIN_DIR to somewhere you can write"
    # Moved into place rather than written in place: replacing a running binary
    # by overwriting it corrupts the copy that is running.
    chmod +x "$tmp/$BIN"
    mv -f "$tmp/$BIN" "$dir/$BIN"

    say ""
    say "  installed  $dir/$BIN"
    case ":$PATH:" in
        *":$dir:"*)
            say "  run        oryxa"
            ;;
        *)
            say "  $dir is not on your PATH. Add it:"
            say ""
            say "    echo 'export PATH=\"$dir:\$PATH\"' >> $(profile)"
            say ""
            say "  or run it in full: $dir/$BIN"
            ;;
    esac
    say ""
}

# The release asset names, which are Rust target triples.
target() {
    os=$(uname -s)
    arch=$(uname -m)
    case "$os" in
        Darwin) os=apple-darwin ;;
        Linux) os=unknown-linux-musl ;;
        *) die "$os is not supported by this installer.
  Building from source works everywhere: cargo install --git https://github.com/$REPO oryxa" ;;
    esac
    case "$arch" in
        x86_64 | amd64) arch=x86_64 ;;
        arm64 | aarch64) arch=aarch64 ;;
        *) die "$arch is not supported by this installer" ;;
    esac
    echo "$arch-$os"
}

# The newest release tag.
#
# Read out of the redirect rather than the JSON API: the API is rate-limited per
# IP, which on a shared or corporate address fails for reasons that have nothing
# to do with the person running this.
latest() {
    url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/$REPO/releases/latest" 2>/dev/null) ||
        die "cannot reach GitHub to find the latest release"
    tag=${url##*/}
    case "$tag" in
        "" | releases | latest)
            die "$REPO has no releases yet.
  Building from source works today: cargo install --git https://github.com/$REPO oryxa" ;;
    esac
    echo "$tag"
}

verify() {
    tmp=$1
    archive=$2
    expected=$(grep " $archive\$" "$tmp/SHA256SUMS" | cut -d' ' -f1) ||
        die "$archive is not listed in SHA256SUMS"
    [ -n "$expected" ] || die "$archive is not listed in SHA256SUMS"

    if have sha256sum; then
        actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
    elif have shasum; then
        actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
    else
        die "neither sha256sum nor shasum is installed, so the download cannot be checked"
    fi

    [ "$actual" = "$expected" ] ||
        die "checksum mismatch for $archive
  expected $expected
  got      $actual
  Not installing. Report this: https://github.com/$REPO/issues"
}

# Quiet on failure, including curl's own message: every call site already says
# what could not be fetched, in terms of what the person was trying to do.
fetch() {
    curl -fsL --proto '=https' --tlsv1.2 -o "$2" "$1" 2>/dev/null
}

# Where to suggest putting the PATH line. The shell's own file, because writing
# it to the wrong one is a confusing morning.
profile() {
    case "${SHELL:-}" in
        */zsh) echo "$HOME/.zshrc" ;;
        */bash) echo "$HOME/.bashrc" ;;
        */fish) echo "$HOME/.config/fish/config.fish" ;;
        *) echo "your shell's startup file" ;;
    esac
}

have() { command -v "$1" >/dev/null 2>&1; }
need() { have "$1" || die "$1 is required and was not found"; }
say() { printf '%s\n' "$*"; }
die() {
    printf '\n  %s\n\n' "$*" >&2
    exit 1
}

main "$@"
