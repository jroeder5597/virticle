#!/bin/sh
# virticle-helper: the only program the controller's service account may run as
# root (sudoers: /etc/sudoers.d/virticle). One subcommand per task, each with
# fixed behaviour and strictly checked arguments. There is deliberately no
# subcommand that runs a command or script given by the caller, and none that
# replaces this file: updating the helper takes an administrator ("virticle
# node prepare"), because a self-update would let the service account install
# any code as root.
#
# Installed at /usr/local/libexec/virticle-helper, root:root 0755. POSIX sh.
# Usage: virticle-helper SUBCOMMAND [ARGS]
#   version                                  print the helper version
#   migtls-state DIR                         print the migration certificate directory
#   migtls-mkdir DIR                         create the certificate directory
#   migtls-put DIR NAME OWNER MODE           stdin -> DIR/NAME.virticle-tmp
#   migtls-discard DIR                       remove the temporary files
#   migtls-commit DIR [backup:NAME]... [rm-legacy] [restorecon]
#                                            move the temporary files into place
#   migtls-remove DIR (rm:NAME|restore:NAME)...
#   migtls-rmdir DIR TARGET                  remove an empty directory the setup created
#   gpu-users PCIADDR                        names of processes holding a graphics card open
#
# For tests: when not running as root, VIRTICLE_HELPER_ROOT names a directory
# that is prefixed to every path, and chown/restorecon are skipped. Running as
# root, the variable is ignored.

VERSION=1

PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
LC_ALL=C
export LC_ALL
unset IFS ENV BASH_ENV CDPATH
umask 077

die() {
	echo "virticle-helper: $*" >&2
	exit 2
}

if [ "$(id -u)" = 0 ]; then
	ROOT=""
	TEST=0
else
	ROOT=${VIRTICLE_HELPER_ROOT:-}
	TEST=1
fi

# abs_ok PATH: absolute, plain characters, no "..", no "//", no trailing slash.
abs_ok() {
	case $1 in
	/*) ;;
	*) return 1 ;;
	esac
	case $1 in
	*[!A-Za-z0-9._+=,@/-]* | *//* | */../* | */.. | */./* | */. | / | */) return 1 ;;
	esac
	[ "${#1}" -le 200 ]
}

# nolinks PATH: no component of PATH is a symbolic link.
nolinks() {
	_p=$1
	while [ -n "$_p" ]; do
		[ -L "$ROOT$_p" ] && return 1
		_p=${_p%/*}
	done
	return 0
}

# conf_value KEY: the last value of KEY in qemu.conf, quoted or not.
conf_value() {
	[ -r "$ROOT/etc/libvirt/qemu.conf" ] || return 0
	sed -n \
		-e "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" \
		-e "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\\([^\"[:space:]#][^[:space:]#]*\\).*/\\1/p" \
		"$ROOT/etc/libvirt/qemu.conf" | tail -n 1
}

# certdir: the directory libvirt hands QEMU for migration.
certdir() {
	_d=$(conf_value migrate_tls_x509_cert_dir)
	[ -n "$_d" ] || _d=$(conf_value default_tls_x509_cert_dir)
	[ -n "$_d" ] || _d=/etc/pki/qemu
	echo "$_d"
}

# need_certdir DIR: DIR must be the configured certificate directory, below
# /etc, reached through no symbolic link.
need_certdir() {
	abs_ok "$1" || die "unsupported directory"
	case $1 in
	/etc/?*) ;;
	*) die "the directory must be below /etc" ;;
	esac
	[ "$1" = "$(certdir)" ] || die "$1 is not the certificate directory named in qemu.conf"
	nolinks "$1" || die "$1 is, or lies below, a symbolic link"
}

# need_dir_exists DIR: a real directory owned by root that nobody else can write to.
need_dir_exists() {
	[ -d "$ROOT$1" ] || die "$1 does not exist"
	if [ "$TEST" = 0 ]; then
		[ "$(stat -c %u "$1")" = 0 ] || die "$1 is not owned by root"
		case $(stat -c %a "$1") in
		*[2367]? | *[2367]) die "$1 is writable by others than root" ;;
		esac
	fi
}

# Files that belong to the migration setup. No other name is ever touched.
FILES="server-key.pem client-key.pem ca-cert.pem server-cert.pem client-cert.pem .virticle-migration.json"
LEGACY=.virt-proj-migration.json
BACKUP_SUFFIX=.virticle-orig
TMP_SUFFIX=.virticle-tmp

# known_name NAME [legacy]: NAME is one of FILES (or the legacy marker).
known_name() {
	for _f in $FILES; do
		[ "$1" = "$_f" ] && return 0
	done
	[ "${2:-}" = legacy ] && [ "$1" = "$LEGACY" ] && return 0
	return 1
}

cmd_migtls_state() {
	[ $# -eq 1 ] || die "usage: migtls-state DIR"
	need_certdir "$1"
	d=$ROOT$1
	anc=""
	p=$1
	while [ -n "$p" ]; do
		anc="$p $anc"
		p=${p%/*}
	done
	for a in $anc; do
		if [ -d "$ROOT$a" ]; then echo "@@dir $a 1"; else echo "@@dir $a 0"; fi
	done
	for f in server-key.pem client-key.pem ca-cert.pem server-cert.pem client-cert.pem .virticle-migration.json .virt-proj-migration.json; do
		p="$d/$f"
		if [ -e "$p" ]; then echo "@@file $f $(sha256sum <"$p" | cut -d' ' -f1) $(stat -c '%U:%G %a' "$p")"; fi
	done
	for c in ca-cert.pem:ca server-cert.pem:server client-cert.pem:client .virticle-migration.json:marker .virt-proj-migration.json:oldmarker; do
		f=${c%%:*}
		n=${c##*:}
		echo "@@content $n"
		cat "$d/$f" 2>/dev/null
	done
	echo "@@content ls"
	ls -A "$d" 2>/dev/null
	echo '@@end'
	true
}

cmd_migtls_mkdir() {
	[ $# -eq 1 ] || die "usage: migtls-mkdir DIR"
	need_certdir "$1"
	(umask 022 && mkdir -p -- "$ROOT$1") || die "cannot create $1"
	nolinks "$1" || die "$1 is, or lies below, a symbolic link"
}

cmd_migtls_put() {
	[ $# -eq 4 ] || die "usage: migtls-put DIR NAME OWNER MODE"
	need_certdir "$1"
	need_dir_exists "$1"
	known_name "$2" || die "unsupported file name"
	case $2 in
	*-key.pem) [ "$4" = 0400 ] || die "private keys must have mode 0400" ;;
	*) [ "$4" = 0644 ] || die "certificates must have mode 0644" ;;
	esac
	case $3 in
	*[!a-z0-9_:-]*) die "unsupported owner" ;;
	0:0 | [a-z_][a-z0-9_-]*:0 | [0-9]*:0) ;;
	*) die "unsupported owner" ;;
	esac
	t=$ROOT$1/$2$TMP_SUFFIX
	[ -L "$t" ] && die "$t is a symbolic link"
	rm -f -- "$t"
	head -c 65537 >"$t" || die "cannot write $t"
	if [ "$(wc -c <"$t")" -gt 65536 ]; then
		rm -f -- "$t"
		die "input too large"
	fi
	if [ "$TEST" = 0 ]; then
		chown "$3" "$t" || {
			rm -f -- "$t"
			die "chown failed"
		}
	fi
	chmod "$4" "$t" || die "chmod failed"
}

cmd_migtls_discard() {
	[ $# -eq 1 ] || die "usage: migtls-discard DIR"
	need_certdir "$1"
	for f in $FILES; do
		rm -f -- "$ROOT$1/$f$TMP_SUFFIX"
	done
	true
}

cmd_migtls_commit() {
	[ $# -ge 1 ] || die "usage: migtls-commit DIR [backup:NAME]... [rm-legacy] [restorecon]"
	need_certdir "$1"
	need_dir_exists "$1"
	d=$ROOT$1
	dir=$1
	shift
	legacy=0
	restore=0
	for a in "$@"; do
		case $a in
		backup:*) known_name "${a#backup:}" legacy || die "unsupported file name" ;;
		rm-legacy) legacy=1 ;;
		restorecon) restore=1 ;;
		*) die "unsupported argument" ;;
		esac
	done
	for f in $FILES; do
		[ -f "$d/$f$TMP_SUFFIX" ] && [ ! -L "$d/$f$TMP_SUFFIX" ] || die "$f$TMP_SUFFIX is missing"
	done
	for a in "$@"; do
		case $a in
		backup:*)
			n=${a#backup:}
			[ ! -e "$d/$n$BACKUP_SUFFIX" ] || die "$n$BACKUP_SUFFIX already exists"
			;;
		esac
	done
	for a in "$@"; do
		case $a in
		backup:*)
			n=${a#backup:}
			if [ -e "$d/$n" ]; then mv -- "$d/$n" "$d/$n$BACKUP_SUFFIX" || die "cannot back up $n"; fi
			;;
		esac
	done
	for f in $FILES; do
		mv -f -- "$d/$f$TMP_SUFFIX" "$d/$f" || die "cannot move $f into place"
	done
	if [ "$legacy" = 1 ]; then rm -f -- "$d/$LEGACY"; fi
	if [ "$restore" = 1 ] && [ "$TEST" = 0 ]; then
		restorecon -R "$dir" >/dev/null 2>&1 || true
	fi
	true
}

cmd_migtls_remove() {
	[ $# -ge 1 ] || die "usage: migtls-remove DIR (rm:NAME|restore:NAME)..."
	need_certdir "$1"
	d=$ROOT$1
	shift
	for a in "$@"; do
		case $a in
		rm:* | restore:*) known_name "${a#*:}" legacy || die "unsupported file name" ;;
		*) die "unsupported argument" ;;
		esac
	done
	for a in "$@"; do
		n=${a#*:}
		case $a in
		rm:*) rm -f -- "$d/$n" ;;
		restore:*)
			[ -e "$d/$n$BACKUP_SUFFIX" ] || die "$n$BACKUP_SUFFIX does not exist"
			mv -- "$d/$n$BACKUP_SUFFIX" "$d/$n" || die "cannot restore $n"
			;;
		esac
	done
	true
}

# migtls-rmdir DIR TARGET: remove TARGET (empty) when it is DIR or one of its
# parents below /etc; the setup creates exactly those.
cmd_migtls_rmdir() {
	[ $# -eq 2 ] || die "usage: migtls-rmdir DIR TARGET"
	need_certdir "$1"
	abs_ok "$2" || die "unsupported directory"
	case $2 in
	/etc/?*) ;;
	*) die "the directory must be below /etc" ;;
	esac
	case $1/ in
	"$2"/*) ;;
	*) die "$2 is not the certificate directory or one of its parents" ;;
	esac
	nolinks "$2" || die "$2 is, or lies below, a symbolic link"
	rmdir -- "$ROOT$2" || die "$2 is not empty"
}

cmd_gpu_users() {
	[ $# -eq 1 ] || die "usage: gpu-users PCIADDR"
	case $1 in
	[0-9a-f][0-9a-f][0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f].[0-7]) ;;
	*) die "unsupported PCI address" ;;
	esac
	a=$1
	pat=""
	for n in /sys/bus/pci/devices/$a/drm/card* /sys/bus/pci/devices/$a/drm/renderD*; do
		[ -e "$n" ] && pat="$pat|/dev/dri/$(basename "$n")\$"
	done
	d=$(basename "$(readlink /sys/bus/pci/devices/$a/driver 2>/dev/null)" 2>/dev/null)
	[ "$d" = nvidia ] && pat="$pat|/dev/nvidia"
	pat=${pat#|}
	if [ -n "$pat" ]; then
		for p in /proc/[0-9]*; do
			ls -l "$p/fd" 2>/dev/null | grep -qE "$pat" && cat "$p/comm" 2>/dev/null
		done
	fi
	echo ok
}

[ $# -ge 1 ] || die "usage: virticle-helper SUBCOMMAND [ARGS]"
sub=$1
shift
case $sub in
version) echo "$VERSION" ;;
migtls-state) cmd_migtls_state "$@" ;;
migtls-mkdir) cmd_migtls_mkdir "$@" ;;
migtls-put) cmd_migtls_put "$@" ;;
migtls-discard) cmd_migtls_discard "$@" ;;
migtls-commit) cmd_migtls_commit "$@" ;;
migtls-remove) cmd_migtls_remove "$@" ;;
migtls-rmdir) cmd_migtls_rmdir "$@" ;;
gpu-users) cmd_gpu_users "$@" ;;
*) die "unknown subcommand" ;;
esac
