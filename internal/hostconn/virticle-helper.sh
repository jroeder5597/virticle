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
# Files in the storage pools (the "file verbs"). A path must be absolute, made
# of plain characters, and lie inside an allowed root: the target directory of
# a libvirt storage pool of type dir, fs or netfs, or the UEFI NVRAM directory.
# The parent directory is canonicalised first, so a symbolic link cannot lead
# out of a root. Verbs that change or remove refuse a final component that is a
# symbolic link; verbs that create refuse to replace anything.
#   roots                                    print the allowed roots
#   stat PATH...                             per path: KIND|BLOCKS|BLOCKSIZE|MODE|SIZE or MISSING
#   remove PATH...                           remove files (a missing file is not an error)
#   rmtree DIR                               remove a work directory (.virticle-*) or a directory that holds manifest.json
#   rmdir DIR                                remove an empty directory
#   move SRC DST                             rename a file or directory; DST must not exist
#   copy SRC DST                             copy a file; DST must not exist
#   read PATH MAXBYTES                       print at most MAXBYTES (fails when the file is larger)
#   write PATH MAXBYTES [MODE]               stdin -> new file PATH (600 or 644)
#   mkworkdir DIR TAG                        create DIR/.virticle-TAG.XXXXXX (700) and print it
#   chmod-work MODE DIR                      set 700 or 711 on a work directory
#   qemu-img-info PATH [chain] [FORMAT]      qemu-img info as JSON (a backing chain is checked against the roots first)
#   sha256 PATH                              print the SHA-256 of a file
#   tools-missing NAME...                    names among qemu-img, ssh, tar, sha256sum that are not installed
#
# Node to node transfer with the service account's own key (see docs):
#   peer-key                                 print the account's public peer key
#   peer-allow                               stdin: a peer's public key -> authorized_keys, restricted to peer-recv
#   peer-disallow                            stdin: a peer's public key -> removed again
#   peer-hostkey-set HOST PORT               stdin: host key lines -> the account's peer known_hosts
#   peer-hostkey-unset HOST PORT             forget a host
#   peer-list                                print the allowed peers and the known hosts
#   peer-send HOST PORT USER SRC DST MODE    send SRC over ssh to the peer, which writes DST (new file)
#   peer-ping HOST PORT USER                 check key, host key and helper of a peer
#   peer-recv                                the forced command of a peer key: header line, then the file on stdin
#
# For tests: when not running as root, VIRTICLE_HELPER_ROOT names a directory
# that is prefixed to every path, chown/restorecon are skipped, and
# VIRTICLE_HELPER_POOLS (newline separated), VIRTICLE_HELPER_USER,
# VIRTICLE_HELPER_HOME and VIRTICLE_HELPER_SSH stand in for the libvirt pools,
# the service account and ssh. Running as root, the variables are ignored.

VERSION=2

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

# ---------------------------------------------------------------------------
# File verbs
# ---------------------------------------------------------------------------

HELPER_PATH=/usr/local/libexec/virticle-helper
NVRAM_DIR=/var/lib/libvirt/qemu/nvram

fail() {
	echo "virticle-helper: $*" >&2
	exit 1
}

# A storage pool may point anywhere, and a member of the libvirt group can
# define one. These places are never an allowed root, and neither is a
# directory above one of them. (The NVRAM directory is added separately.)
denied_root() {
	case $1 in
	/ | /bin | /bin/* | /boot | /boot/* | /dev | /dev/* | /etc | /etc/* | /home | /lib | /lib/* | /lib32 | /lib32/* | /lib64 | /lib64/* | /proc | /proc/* | /root | /root/* | /run | /run/* | /sbin | /sbin/* | /sys | /sys/* | /usr | /usr/* | /var | /var/spool | /var/spool/* | /var/log | /var/log/* | /var/lib | /var/lib/dpkg | /var/lib/dpkg/* | /var/lib/rpm | /var/lib/rpm/* | /var/lib/sudo | /var/lib/sudo/* | /var/lib/virticle | /var/lib/virticle/*) return 0 ;;
	/var/lib/libvirt | /var/lib/libvirt/qemu | /var/lib/libvirt/qemu/* | /var/lib/libvirt/secrets | /var/lib/libvirt/secrets/* | /var/lib/libvirt/swtpm | /var/lib/libvirt/swtpm/* | /var/lib/libvirt/network | /var/lib/libvirt/network/* | /var/lib/libvirt/dnsmasq | /var/lib/libvirt/dnsmasq/* | /var/lib/libvirt/lockd | /var/lib/libvirt/lockd/* | /var/lib/libvirt/sanlock | /var/lib/libvirt/sanlock/*) return 0 ;;
	esac
	return 1
}

# pool_dirs: target directories of the dir, fs and netfs pools, one per line.
pool_dirs() {
	if [ "$TEST" = 1 ]; then
		printf '%s\n' "${VIRTICLE_HELPER_POOLS:-}"
		return 0
	fi
	command -v virsh >/dev/null 2>&1 || return 0
	virsh -q --connect qemu:///system pool-list --all --name 2>/dev/null | while IFS= read -r _p; do
		[ -n "$_p" ] || continue
		_x=$(virsh --connect qemu:///system pool-dumpxml --pool "$_p" 2>/dev/null) || continue
		case $_x in
		"<pool type='dir'"* | "<pool type='fs'"* | "<pool type='netfs'"*) ;;
		*) continue ;;
		esac
		printf '%s\n' "$_x" | sed -n 's:^[[:space:]]*<path>\(/[^<]*\)</path>.*:\1:p' | head -n 1
	done
	return 0
}

ROOTS_DONE=0
ROOTS=""
ROOTREAL=""
if [ -n "$ROOT" ]; then ROOTREAL=$(realpath -- "$ROOT" 2>/dev/null || echo "$ROOT"); fi

NL='
'

# canon PATH: the canonical form of an existing PATH (without the test prefix) in RP.
canon() {
	_c=$(realpath -e -- "$ROOT$1" 2>/dev/null) || return 1
	RP=${_c#"$ROOTREAL"}
	[ -n "$RP" ] || RP=/
	return 0
}

# add_root DIR CHECKDENIED: add the canonical form of DIR to ROOTS when it is a
# directory (and, for CHECKDENIED=1, not a denied place).
add_root() {
	abs_ok "$1" || return 0
	canon "$1" || return 0
	[ -d "$ROOT$RP" ] || return 0
	if [ "$2" = 1 ] && denied_root "$RP"; then return 0; fi
	case "$NL$ROOTS" in *"$NL$RP$NL"*) return 0 ;; esac
	ROOTS="$ROOTS$RP$NL"
}

# load_roots: ROOTS = newline separated canonical roots that exist.
load_roots() {
	[ "$ROOTS_DONE" = 0 ] || return 0
	ROOTS_DONE=1
	_keep=${RP:-}
	_l=$(pool_dirs)
	for _d in $_l; do add_root "$_d" 1; done
	add_root "$NVRAM_DIR" 0
	RP=$_keep
	return 0
}

# in_roots CANON: CANON is an allowed root or lies below one.
in_roots() {
	load_roots
	for _r in $ROOTS; do
		case $1 in "$_r" | "$_r"/*) return 0 ;; esac
	done
	return 1
}

# root_or_above CANON: CANON is an allowed root or a directory above one.
root_or_above() {
	load_roots
	for _r in $ROOTS; do
		case $_r in "$1" | "$1"/*) return 0 ;; esac
	done
	return 1
}

# vpath_rd PATH: PATH, existing or not, resolves to a place inside the roots.
# RP is its canonical form. Symbolic links are followed when they stay inside.
vpath_rd() {
	abs_ok "$1" || die "unsupported path"
	if [ -e "$ROOT$1" ] || [ -L "$ROOT$1" ]; then
		canon "$1" || die "cannot resolve $1"
		in_roots "$RP" || die "$1 is not inside a storage pool directory"
		return 0
	fi
	_par=${1%/*}
	[ -n "$_par" ] || die "unsupported path"
	canon "$_par" || die "the directory of $1 does not exist"
	in_roots "$RP" || die "$1 is not inside a storage pool directory"
	RP=${RP%/}/${1##*/}
}

# vpath_new PATH: a name that does not exist yet, in a directory inside the
# roots. RP is the canonical name.
vpath_new() {
	abs_ok "$1" || die "unsupported path"
	_par=${1%/*}
	[ -n "$_par" ] || die "unsupported path"
	canon "$_par" || die "the directory of $1 does not exist"
	in_roots "$RP" || die "$1 is not inside a storage pool directory"
	[ -d "$ROOT$RP" ] || die "the directory of $1 is not a directory"
	RP=${RP%/}/${1##*/}
	if [ -e "$ROOT$RP" ] || [ -L "$ROOT$RP" ]; then
		fail "$1 already exists"
	fi
}

# vpath_mod PATH: a name in a directory inside the roots that is not a
# symbolic link, not a root itself and not a directory above one.
vpath_mod() {
	abs_ok "$1" || die "unsupported path"
	_par=${1%/*}
	[ -n "$_par" ] || die "unsupported path"
	canon "$_par" || die "the directory of $1 does not exist"
	in_roots "$RP" || die "$1 is not inside a storage pool directory"
	RP=${RP%/}/${1##*/}
	[ ! -L "$ROOT$RP" ] || die "$1 is a symbolic link"
	if root_or_above "$RP"; then die "$1 is a storage pool directory"; fi
}

cmd_roots() {
	[ $# -eq 0 ] || die "usage: roots"
	load_roots
	printf '%s' "$ROOTS"
}

cmd_stat() {
	[ $# -ge 1 ] && [ $# -le 200 ] || die "usage: stat PATH..."
	for _a in "$@"; do abs_ok "$_a" || die "unsupported path"; done
	for _a in "$@"; do
		vpath_rd "$_a"
		if [ -e "$ROOT$RP" ]; then
			stat -L -c '%F|%b|%B|%a|%s' -- "$ROOT$RP" || fail "cannot stat $_a"
		else
			echo MISSING
		fi
	done
}

cmd_remove() {
	[ $# -ge 1 ] && [ $# -le 200 ] || die "usage: remove PATH..."
	for _a in "$@"; do abs_ok "$_a" || die "unsupported path"; done
	for _a in "$@"; do
		vpath_mod "$_a"
		if [ -e "$ROOT$RP" ]; then
			[ ! -d "$ROOT$RP" ] || die "$_a is a directory"
			rm -f -- "$ROOT$RP" || fail "cannot remove $_a"
		fi
	done
}

# work_name PATH: the last component is a work directory name.
work_name() {
	case ${1##*/} in .virticle-*) return 0 ;; esac
	return 1
}

cmd_rmtree() {
	[ $# -eq 1 ] || die "usage: rmtree DIR"
	vpath_mod "$1"
	[ -d "$ROOT$RP" ] || die "$1 is not a directory"
	if ! work_name "$RP" && [ ! -f "$ROOT$RP/manifest.json" ]; then
		die "$1 is neither a work directory nor a backup"
	fi
	rm -rf --one-file-system -- "$ROOT$RP" || fail "cannot remove $1"
}

cmd_rmdir() {
	[ $# -eq 1 ] || die "usage: rmdir DIR"
	vpath_mod "$1"
	[ -d "$ROOT$RP" ] || die "$1 is not a directory"
	rmdir -- "$ROOT$RP" || fail "$1 is not empty"
}

cmd_move() {
	[ $# -eq 2 ] || die "usage: move SRC DST"
	vpath_mod "$1"
	_s=$RP
	[ -e "$ROOT$_s" ] || fail "$1 does not exist"
	vpath_new "$2"
	mv -T -n -- "$ROOT$_s" "$ROOT$RP" || fail "cannot move $1"
	if [ -e "$ROOT$_s" ] || [ -L "$ROOT$_s" ]; then fail "$2 appeared meanwhile; nothing was overwritten"; fi
}

cmd_copy() {
	[ $# -eq 2 ] || die "usage: copy SRC DST"
	vpath_rd "$1"
	_s=$RP
	[ -f "$ROOT$_s" ] || fail "$1 is not a regular file"
	vpath_new "$2"
	_d=$RP
	_t=$ROOT$_d.virticle-part.$$
	trap 'rm -f -- "$_t"' EXIT
	cp --preserve=mode -- "$ROOT$_s" "$_t" || fail "cannot copy $1"
	ln -- "$_t" "$ROOT$_d" 2>/dev/null || fail "$2 appeared meanwhile; nothing was overwritten"
	rm -f -- "$_t"
	trap - EXIT
}

num_ok() {
	case $1 in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "${#1}" -le 15 ]
}

limit_ok() { num_ok "$1" && [ "$1" -ge 1 ] && [ "$1" -le 67108864 ]; }

cmd_read() {
	[ $# -eq 2 ] || die "usage: read PATH MAXBYTES"
	limit_ok "$2" || die "unsupported limit"
	vpath_rd "$1"
	[ -f "$ROOT$RP" ] || fail "$1 is not a regular file"
	head -c "$(($2 + 1))" -- "$ROOT$RP" || fail "cannot read $1"
}

cmd_write() {
	[ $# -ge 2 ] && [ $# -le 3 ] || die "usage: write PATH MAXBYTES [MODE]"
	limit_ok "$2" || die "unsupported limit"
	_m=${3:-600}
	case $_m in 600 | 644) ;; *) die "unsupported mode" ;; esac
	vpath_new "$1"
	_d=$RP
	_t=$ROOT$_d.virticle-part.$$
	trap 'rm -f -- "$_t"' EXIT
	(
		set -C
		head -c "$(($2 + 1))" >"$_t"
	) || fail "cannot write $1"
	if [ "$(wc -c <"$_t")" -gt "$2" ]; then
		fail "input larger than $2 bytes"
	fi
	chmod "$_m" "$_t" || fail "chmod failed"
	ln -- "$_t" "$ROOT$_d" 2>/dev/null || fail "$1 appeared meanwhile; nothing was overwritten"
	rm -f -- "$_t"
	trap - EXIT
}

cmd_mkworkdir() {
	[ $# -eq 2 ] || die "usage: mkworkdir DIR TAG"
	case $2 in
	'' | *[!a-z]*) die "unsupported tag" ;;
	esac
	[ "${#2}" -le 12 ] || die "unsupported tag"
	vpath_rd "$1"
	[ -d "$ROOT$RP" ] || fail "$1 is not a directory"
	_w=$(umask 077 && mktemp -d -- "$ROOT$RP/.virticle-$2.XXXXXX") || fail "cannot create a work directory in $1"
	echo "$1/${_w##*/}"
}

cmd_chmod_work() {
	[ $# -eq 2 ] || die "usage: chmod-work MODE DIR"
	case $1 in 700 | 711) ;; *) die "unsupported mode" ;; esac
	vpath_mod "$2"
	[ -d "$ROOT$RP" ] || die "$2 is not a directory"
	work_name "$RP" || die "$2 is not a work directory"
	chmod "$1" -- "$ROOT$RP" || fail "chmod failed"
}

cmd_sha256() {
	[ $# -eq 1 ] || die "usage: sha256 PATH"
	vpath_rd "$1"
	[ -f "$ROOT$RP" ] || fail "$1 is not a regular file"
	sha256sum <"$ROOT$RP" | cut -d' ' -f1
}

cmd_tools_missing() {
	[ $# -ge 1 ] && [ $# -le 8 ] || die "usage: tools-missing NAME..."
	for _t in "$@"; do
		case $_t in qemu-img | ssh | scp | tar | sha256sum) ;; *) die "unsupported tool name" ;; esac
	done
	for _t in "$@"; do
		command -v "$_t" >/dev/null 2>&1 || echo "$_t"
	done
	true
}

# --- qemu-img info, with the backing chain checked against the roots first ---

# header_backing FILE: the backing file name a qcow2 (or qcow) header records;
# nothing for other files. Fails for a header that cannot be followed.
header_backing() {
	_hf=$ROOT$1
	[ "$(head -c 4 -- "$_hf" | od -An -tx1 | tr -d ' \n')" = 514649fb ] || return 0
	_o=$(dd if="$_hf" bs=1 skip=8 count=8 2>/dev/null | od -An -tu1)
	_s=$(dd if="$_hf" bs=1 skip=16 count=4 2>/dev/null | od -An -tu1)
	_off=0
	for _b in $_o; do _off=$((_off * 256 + _b)); done
	_len=0
	for _b in $_s; do _len=$((_len * 256 + _b)); done
	[ "$_off" -ge 0 ] && [ "$_len" -ge 0 ] || return 1
	[ "$_len" -gt 0 ] && [ "$_off" -gt 0 ] || return 0
	[ "$_len" -le 1023 ] && [ "$_off" -le 1048576 ] || return 1
	dd if="$_hf" bs=1 skip="$_off" count="$_len" 2>/dev/null
	return 0
}

# check_chain FILE: every backing file of FILE lies inside the roots.
check_chain() {
	_cf=$1
	_n=0
	while :; do
		_bn=$(header_backing "$_cf") || die "cannot follow the backing chain of $1"
		[ -n "$_bn" ] || return 0
		case $_bn in
		/*) _bp=$_bn ;;
		*) _bp=${_cf%/*}/$_bn ;;
		esac
		abs_ok "$_bp" || die "the backing file of $_cf has an unsupported name"
		[ -f "$ROOT$_bp" ] || fail "the backing file $_bp of $_cf does not exist"
		canon "$_bp" || fail "cannot resolve the backing file $_bp"
		in_roots "$RP" || die "the backing file $_bp of $_cf is outside the storage pool directories"
		_cf=$RP
		_n=$((_n + 1))
		[ "$_n" -le 32 ] || die "backing chain too long"
	done
}

cmd_qemu_img_info() {
	[ $# -ge 1 ] && [ $# -le 3 ] || die "usage: qemu-img-info PATH [chain] [FORMAT]"
	_chain=""
	_fmt=""
	_p=$1
	shift
	for _a in "$@"; do
		case $_a in
		chain) _chain=--backing-chain ;;
		raw | qcow2 | vpc | vhdx | vdi) _fmt=$_a ;;
		*) die "unsupported argument" ;;
		esac
	done
	vpath_rd "$_p"
	[ -f "$ROOT$RP" ] || fail "$_p is not a regular file"
	_f=$RP
	if [ -z "$_fmt" ]; then
		if [ "$(head -c 4 -- "$ROOT$_f" | od -An -tx1 | tr -d ' \n')" = 514649fb ]; then _fmt=qcow2; else _fmt=raw; fi
	fi
	if [ "$_fmt" = qcow2 ]; then check_chain "$_f"; fi
	# shellcheck disable=SC2086
	qemu-img info -U -f "$_fmt" $_chain --output=json -- "$ROOT$_f" 2>&1
}

# ---------------------------------------------------------------------------
# Node to node transfer
# ---------------------------------------------------------------------------

PEER_KEY_NAME=virticle_peer_ed25519
PEER_HOSTS_NAME=virticle_peer_known_hosts
PEER_MARK="virticle-helper peer-recv"

# svc_account: the account that called sudo, in SVC and SVCHOME.
svc_account() {
	if [ "$TEST" = 1 ]; then
		SVC=${VIRTICLE_HELPER_USER:-svc}
		SVCHOME=${VIRTICLE_HELPER_HOME:-$ROOT/home/svc}
		return 0
	fi
	SVC=${SUDO_USER:-}
	[ -n "$SVC" ] && [ "$SVC" != root ] || die "this runs through sudo for the service account"
	case $SVC in *[!a-z0-9_-]*) die "unsupported account" ;; esac
	SVCHOME=$(getent passwd "$SVC" | cut -d: -f6)
	[ "$(getent passwd "$SVC" | cut -d: -f3)" != 0 ] || die "unsupported account"
	abs_ok "$SVCHOME" || die "unsupported home directory"
}

# as_svc CMD...: run CMD as the service account, so that the files it owns are
# read and edited without root's privileges.
as_svc() {
	if [ "$TEST" = 1 ]; then "$@"; else runuser -u "$SVC" -- "$@"; fi
}

# as_svc_verb VERB ARG...: run one of this helper's svc-* verbs as the service
# account (they refuse to run as root).
as_svc_verb() {
	if [ "$TEST" = 1 ]; then sh "$0" "$@"; else runuser -u "$SVC" -- "$HELPER_PATH" "$@"; fi
}

svc_only() {
	[ "$(id -u)" != 0 ] || die "this is for the service account itself"
	abs_ok "$1" || die "unsupported home directory"
	[ -d "$1/.ssh" ] && [ ! -L "$1/.ssh" ] || fail "no usable $1/.ssh"
	if [ "$TEST" = 0 ]; then [ -O "$1/.ssh" ] || die "$1/.ssh is not yours"; fi
}

host_ok() {
	case $1 in
	'' | -* | *[!A-Za-z0-9.:_-]*) return 1 ;;
	esac
	[ "${#1}" -le 253 ]
}
port_ok() { num_ok "$1" && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
user_ok() {
	case $1 in
	'' | -* | [0-9]* | *[!a-z0-9_-]*) return 1 ;;
	esac
	[ "${#1}" -le 32 ]
}

# read_key: one "ssh-ed25519 BLOB [comment]" line on stdin; BLOB in KEYBLOB.
read_key() {
	IFS= read -r _line || true
	case $_line in
	"ssh-ed25519 "*) ;;
	*) die "expected an ssh-ed25519 public key" ;;
	esac
	KEYBLOB=${_line#ssh-ed25519 }
	KEYBLOB=${KEYBLOB%% *}
	case $KEYBLOB in
	'' | *[!A-Za-z0-9+/=]*) die "unsupported key" ;;
	esac
	[ "${#KEYBLOB}" -ge 60 ] && [ "${#KEYBLOB}" -le 100 ] || die "unsupported key"
}

cmd_peer_key() {
	[ $# -eq 0 ] || die "usage: peer-key"
	svc_account
	_o=$(as_svc head -c 400 -- "$SVCHOME/.ssh/$PEER_KEY_NAME.pub" 2>/dev/null) || fail "no peer key (run virticle node prepare again)"
	_o=$(printf '%s\n' "$_o" | head -n 1)
	case $_o in "ssh-ed25519 "*) ;; *) fail "the peer key file is not an ssh-ed25519 key" ;; esac
	# shellcheck disable=SC2086
	set -- $_o
	echo "$1 $2"
}

# svc-allow HOME BLOB LINE MARK: authorized_keys without the peer lines that
# carry BLOB, plus LINE when it is not empty. Runs as the service account.
cmd_svc_allow() {
	[ $# -eq 4 ] || die "usage: svc-allow HOME BLOB LINE MARK"
	svc_only "$1"
	_f=$1/.ssh/authorized_keys
	[ ! -L "$_f" ] || fail "$_f is a symbolic link"
	_t=$(mktemp "$1/.ssh/.authorized_keys.XXXXXX") || fail "cannot create a temporary file"
	trap 'rm -f -- "$_t"' EXIT
	if [ -f "$_f" ]; then B=$2 M=$4 awk 'index($0, ENVIRON["B"]) && index($0, ENVIRON["M"]) {next} {print}' "$_f" >"$_t" || fail "cannot read $_f"; fi
	if [ -n "$3" ]; then echo "$3" >>"$_t"; fi
	if [ -f "$_f" ] && cmp -s "$_t" "$_f"; then return 0; fi
	mv -f -- "$_t" "$_f" || fail "cannot replace $_f"
}

cmd_peer_allow() {
	[ $# -eq 0 ] || die "usage: peer-allow (public key on stdin)"
	read_key
	svc_account
	_line="restrict,command=\"sudo -n $HELPER_PATH peer-recv\" ssh-ed25519 $KEYBLOB virticle-peer"
	as_svc_verb svc-allow "$SVCHOME" "$KEYBLOB" "$_line" "$PEER_MARK" || fail "cannot update authorized_keys"
}

# Only lines that carry the peer-recv restriction are ever removed, so the
# controller's own login key cannot be removed through this.
cmd_peer_disallow() {
	[ $# -eq 0 ] || die "usage: peer-disallow (public key on stdin)"
	read_key
	svc_account
	as_svc_verb svc-allow "$SVCHOME" "$KEYBLOB" "" "$PEER_MARK" || fail "cannot update authorized_keys"
}

# svc-hosts HOME NAME [KEYLINE...]: the peer known_hosts without NAME, plus a
# line per KEYLINE. Runs as the service account.
cmd_svc_hosts() {
	[ $# -ge 2 ] || die "usage: svc-hosts HOME NAME [KEYLINE]..."
	svc_only "$1"
	_h=$1
	_name=$2
	shift 2
	_f=$_h/.ssh/$PEER_HOSTS_NAME
	[ ! -L "$_f" ] || fail "$_f is a symbolic link"
	_t=$(mktemp "$_h/.ssh/.peer_hosts.XXXXXX") || fail "cannot create a temporary file"
	trap 'rm -f -- "$_t"' EXIT
	: >"$_t"
	if [ -f "$_f" ]; then N=$_name awk '$1 != ENVIRON["N"] {print}' "$_f" >"$_t" || fail "cannot read $_f"; fi
	for _k in "$@"; do echo "$_name $_k" >>"$_t"; done
	mv -f -- "$_t" "$_f" || fail "cannot replace $_f"
}

# hosts_field HOST PORT: the host field of a known_hosts line, in KNAME.
hosts_field() {
	if [ "$2" = 22 ]; then KNAME=$1; else KNAME="[$1]:$2"; fi
}

cmd_peer_hostkey_set() {
	[ $# -eq 2 ] || die "usage: peer-hostkey-set HOST PORT (key lines on stdin)"
	host_ok "$1" && port_ok "$2" || die "unsupported host or port"
	_host=$1
	_port=$2
	_keys=""
	_n=0
	while IFS= read -r _l; do
		case $_l in
		"ssh-ed25519 "* | "ecdsa-sha2-nistp256 "* | "ecdsa-sha2-nistp384 "* | "ecdsa-sha2-nistp521 "* | "ssh-rsa "*) ;;
		*) die "expected host key lines" ;;
		esac
		_t=${_l%% *}
		_b=${_l#* }
		_b=${_b%% *}
		case $_b in '' | *[!A-Za-z0-9+/=]*) die "unsupported host key" ;; esac
		[ "${#_b}" -le 1000 ] || die "unsupported host key"
		_keys="$_keys$_t $_b$NL"
		_n=$((_n + 1))
		[ "$_n" -le 4 ] || die "too many keys"
	done
	[ "$_n" -ge 1 ] || die "no host key given"
	svc_account
	hosts_field "$_host" "$_port"
	_name=$KNAME
	# one key per line; no key line contains a space-separated word that a split on newlines would break
	set --
	_save=$IFS
	IFS=$NL
	for _k in $_keys; do set -- "$@" "$_k"; done
	IFS=$_save
	as_svc_verb svc-hosts "$SVCHOME" "$_name" "$@" || fail "cannot update the peer known_hosts"
}

cmd_peer_hostkey_unset() {
	[ $# -eq 2 ] || die "usage: peer-hostkey-unset HOST PORT"
	host_ok "$1" && port_ok "$2" || die "unsupported host or port"
	svc_account
	hosts_field "$1" "$2"
	as_svc_verb svc-hosts "$SVCHOME" "$KNAME" || fail "cannot update the peer known_hosts"
}

# svc-list HOME MARK: the peers and hosts on record. Runs as the service account.
cmd_svc_list() {
	[ $# -eq 2 ] || die "usage: svc-list HOME MARK"
	svc_only "$1"
	_f=$1/.ssh/authorized_keys
	if [ -f "$_f" ]; then
		grep -F -- "$2" "$_f" | sed -n 's/.* ssh-ed25519 \([A-Za-z0-9+\/=]*\) virticle-peer$/allow \1/p'
	fi
	_f=$1/.ssh/$PEER_HOSTS_NAME
	if [ -f "$_f" ]; then
		while read -r _a _b _rest; do echo "host $_a $_b"; done <"$_f"
	fi
	true
}

cmd_peer_list() {
	[ $# -eq 0 ] || die "usage: peer-list"
	svc_account
	as_svc_verb svc-list "$SVCHOME" "$PEER_MARK"
}

# peer_ssh HOST PORT USER ARG...: ssh to the peer as the service account, with
# its own key and the host keys recorded by peer-hostkey-set. Strict checking:
# an unknown or changed host key is an error, never accepted.
peer_ssh() {
	_ph=$1
	_pp=$2
	_pu=$3
	shift 3
	_sshbin=ssh
	if [ "$TEST" = 1 ] && [ -n "${VIRTICLE_HELPER_SSH:-}" ]; then _sshbin=$VIRTICLE_HELPER_SSH; fi
	as_svc "$_sshbin" -F /dev/null -T -o BatchMode=yes -o IdentitiesOnly=yes -i "$SVCHOME/.ssh/$PEER_KEY_NAME" \
		-o UserKnownHostsFile="$SVCHOME/.ssh/$PEER_HOSTS_NAME" -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes \
		-o PasswordAuthentication=no -o ClearAllForwardings=yes -o ForwardAgent=no \
		-o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=8 -o LogLevel=ERROR \
		-p "$_pp" -- "$_pu@$_ph" "$@"
}

peer_args() {
	host_ok "$1" && port_ok "$2" && user_ok "$3" || die "unsupported host, port or user"
}

cmd_peer_ping() {
	[ $# -eq 3 ] || die "usage: peer-ping HOST PORT USER"
	peer_args "$@"
	svc_account
	_r=$(printf 'virticle-peer 1 ping\n' | peer_ssh "$1" "$2" "$3" peer-recv 2>&1) || fail "$(printf '%s' "$_r" | head -n 3 | tr '\n' ' ')"
	case $_r in "@@pong "*) echo "$_r" ;; *) fail "unexpected answer: $(printf '%s' "$_r" | head -n 1)" ;; esac
}

cmd_peer_send() {
	[ $# -eq 6 ] || die "usage: peer-send HOST PORT USER SRC DST MODE"
	peer_args "$1" "$2" "$3"
	abs_ok "$5" || die "unsupported destination path"
	case $6 in [0-7][0-7][0-7]) ;; *) die "unsupported mode" ;; esac
	svc_account
	vpath_rd "$4"
	[ -f "$ROOT$RP" ] || fail "$4 is not a regular file"
	_src=$ROOT$RP
	_size=$(stat -c %s -- "$_src") || fail "cannot stat $4"
	exec 3<"$_src" || fail "cannot open $4"
	(
		while :; do
			sleep 1
			_pos=$(sed -n 's/^pos:[[:space:]]*//p' "/proc/$$/fdinfo/3" 2>/dev/null)
			[ -n "$_pos" ] || break
			echo "@@progress $_pos $_size"
		done
	) &
	_bg=$!
	_res=$(
		{
			printf 'virticle-peer 1 recv %s %s %s\n' "$6" "$_size" "$5"
			cat <&3
		} | peer_ssh "$1" "$2" "$3" peer-recv 2>&1
	)
	_rc=$?
	kill "$_bg" 2>/dev/null
	wait "$_bg" 2>/dev/null
	exec 3<&-
	[ "$_rc" = 0 ] || fail "sending to $3@$1 failed: $(printf '%s' "$_res" | head -n 3 | tr '\n' ' ')"
	case $_res in
	"@@received $_size") echo "@@done $_size" ;;
	*) fail "the peer did not confirm the file: $(printf '%s' "$_res" | head -n 1)" ;;
	esac
}

# peer-recv: the forced command of a peer key. Stdin: one header line, then
# the data. A path is checked like for every other verb, and an existing file
# is never replaced.
cmd_peer_recv() {
	[ $# -eq 0 ] || die "usage: peer-recv"
	IFS= read -r _hdr || die "no header"
	[ "${#_hdr}" -le 400 ] || die "header too long"
	case $_hdr in
	*[!A-Za-z0-9._+=,@/\ -]*) die "unsupported header" ;;
	esac
	# shellcheck disable=SC2086
	set -- $_hdr
	[ "${1:-}" = virticle-peer ] && [ "${2:-}" = 1 ] || die "unsupported header"
	case ${3:-} in
	ping)
		[ $# -eq 3 ] || die "unsupported header"
		echo "@@pong $VERSION"
		;;
	recv)
		[ $# -eq 6 ] || die "unsupported header"
		_mode=$4
		_size=$5
		case $_mode in [0-7][0-7][0-7]) ;; *) die "unsupported mode" ;; esac
		num_ok "$_size" || die "unsupported size"
		vpath_new "$6"
		_d=$RP
		_t=$ROOT$_d.virticle-part.$$
		trap 'rm -f -- "$_t"' EXIT
		(
			set -C
			head -c "$_size" >"$_t"
		) || fail "cannot write $6"
		[ "$(wc -c <"$_t")" = "$_size" ] || fail "the file arrived incomplete"
		chmod "$_mode" "$_t" || fail "chmod failed"
		ln -- "$_t" "$ROOT$_d" 2>/dev/null || fail "$6 appeared meanwhile; nothing was overwritten"
		rm -f -- "$_t"
		trap - EXIT
		echo "@@received $_size"
		;;
	*) die "unsupported header" ;;
	esac
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
roots) cmd_roots "$@" ;;
stat) cmd_stat "$@" ;;
remove) cmd_remove "$@" ;;
rmtree) cmd_rmtree "$@" ;;
rmdir) cmd_rmdir "$@" ;;
move) cmd_move "$@" ;;
copy) cmd_copy "$@" ;;
read) cmd_read "$@" ;;
write) cmd_write "$@" ;;
mkworkdir) cmd_mkworkdir "$@" ;;
chmod-work) cmd_chmod_work "$@" ;;
qemu-img-info) cmd_qemu_img_info "$@" ;;
sha256) cmd_sha256 "$@" ;;
tools-missing) cmd_tools_missing "$@" ;;
peer-key) cmd_peer_key "$@" ;;
peer-allow) cmd_peer_allow "$@" ;;
peer-disallow) cmd_peer_disallow "$@" ;;
peer-hostkey-set) cmd_peer_hostkey_set "$@" ;;
peer-hostkey-unset) cmd_peer_hostkey_unset "$@" ;;
peer-list) cmd_peer_list "$@" ;;
peer-send) cmd_peer_send "$@" ;;
peer-ping) cmd_peer_ping "$@" ;;
peer-recv) cmd_peer_recv "$@" ;;
svc-allow) cmd_svc_allow "$@" ;;
svc-hosts) cmd_svc_hosts "$@" ;;
svc-list) cmd_svc_list "$@" ;;
*) die "unknown subcommand" ;;
esac
