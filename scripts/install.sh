#!/bin/sh
# Install a virticle release package (.deb or .rpm) from a releases URL.
#
#   VIRTICLE_RELEASES_URL=https://HOST/PATH sh install.sh --version 1.2.3
#
# The releases URL must contain one directory per release, named v<version>,
# with the packages and checksums.txt inside (what goreleaser produces):
#   $VIRTICLE_RELEASES_URL/v1.2.3/virticle_1.2.3_amd64.deb
#   $VIRTICLE_RELEASES_URL/v1.2.3/checksums.txt
#
# Nothing is piped into a shell. The package is downloaded to a temporary
# directory, its SHA-256 is checked against checksums.txt (or against
# --sha256 when you got the hash from somewhere else), and only then handed to
# apt-get or dnf. checksums.txt comes from the same server as the package, so
# it detects damaged downloads; use --sha256 with a hash obtained over another
# channel when you need to detect a tampered server.
#
# Run it as root (or through sudo). Read it before you run it.

set -eu

usage() {
	cat <<'EOF'
Usage: install.sh --version VERSION [--dry-run] [--sha256 HASH] [--url URL]

  --version VERSION  release to install, without the leading v (required)
  --dry-run          print what would be done; download and install nothing
  --sha256 HASH      expected SHA-256 of the package (default: checksums.txt)
  --url URL          releases URL (default: $VIRTICLE_RELEASES_URL)
  -h, --help         show this text
EOF
}

die() {
	echo "install.sh: $*" >&2
	exit 1
}

version=
dry=0
want_sha=
url=${VIRTICLE_RELEASES_URL:-}

while [ $# -gt 0 ]; do
	case $1 in
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		version=$2
		shift 2
		;;
	--sha256)
		[ $# -ge 2 ] || die "--sha256 needs a value"
		want_sha=$2
		shift 2
		;;
	--url)
		[ $# -ge 2 ] || die "--url needs a value"
		url=$2
		shift 2
		;;
	--dry-run)
		dry=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument: $1"
		;;
	esac
done

[ -n "$version" ] || die "--version is required"
[ -n "$url" ] || die "set VIRTICLE_RELEASES_URL (or use --url) to the releases location"
case $version in
v*) die "give the version without the leading v" ;;
*[!0-9A-Za-z.+~_-]*) die "invalid version: $version" ;;
esac
case $url in
https://*) ;;
*) die "the releases URL must start with https://" ;;
esac
url=${url%/}

# Distro family: debian-like uses .deb, rhel-like uses .rpm.
family=
if [ -r /etc/os-release ]; then
	# shellcheck disable=SC1091
	. /etc/os-release
	for id in ${ID:-} ${ID_LIKE:-}; do
		case $id in
		debian | ubuntu)
			family=deb
			break
			;;
		rhel | fedora | centos | rocky | almalinux)
			family=rpm
			break
			;;
		esac
	done
fi
[ -n "$family" ] || die "unsupported distribution (need a Debian-like or RHEL-like system); use the tar.gz archive instead, see docs/install.md"

machine=$(uname -m)
case $family:$machine in
deb:x86_64) file=virticle_${version}_amd64.deb ;;
deb:aarch64 | deb:arm64) file=virticle_${version}_arm64.deb ;;
rpm:x86_64) file=virticle-${version}-1.x86_64.rpm ;;
rpm:aarch64 | rpm:arm64) file=virticle-${version}-1.aarch64.rpm ;;
*) die "unsupported architecture: $machine (amd64 and arm64 are built)" ;;
esac

base=$url/v$version
if [ "$family" = deb ]; then
	install_cmd="apt-get install -y ./$file"
else
	install_cmd="dnf install -y ./$file"
fi

if [ "$dry" -eq 1 ]; then
	echo "family:    $family"
	echo "machine:   $machine"
	echo "package:   $base/$file"
	if [ -n "$want_sha" ]; then
		echo "checksum:  $want_sha (from --sha256)"
	else
		echo "checksum:  $base/checksums.txt"
	fi
	echo "install:   $install_cmd"
	echo "dry run: nothing downloaded or installed"
	exit 0
fi

[ "$(id -u)" -eq 0 ] || die "run as root (or with sudo)"

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q --https-only -O "$2" "$1"; }
else
	die "curl or wget is needed"
fi
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is needed"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
chmod 755 "$tmp" # apt runs its download sandbox as another user and must read the package

echo "downloading $base/$file"
fetch "$base/$file" "$tmp/$file" || die "download failed: $base/$file"

if [ -z "$want_sha" ]; then
	fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"
	want_sha=$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
	[ -n "$want_sha" ] || die "$file is not listed in checksums.txt"
fi
got_sha=$(sha256sum "$tmp/$file" | awk '{ print $1 }')
if [ "$got_sha" != "$want_sha" ]; then
	die "checksum mismatch for $file: expected $want_sha, got $got_sha; nothing installed"
fi
echo "checksum ok: $got_sha"

cd "$tmp"
if [ "$family" = deb ]; then
	apt-get install -y "./$file"
else
	dnf install -y "./$file"
fi
echo "installed virticle $version"
