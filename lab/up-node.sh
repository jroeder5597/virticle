#!/usr/bin/env bash
# Adds one bare node of a chosen distribution to the lab that lab/up.sh built.
# The node gets only the root SSH keys and a hostname: libvirt and everything
# else is installed by "virticle node prepare", which is what this node is for
# testing. Run ON the KVM host:
#   DISTRO=fedora43 NODE_INDEX=7 bash up-node.sh <controller.pub> <cluster> <cluster.pub>
# DISTRO: debian13 | rocky10 | fedora43 | ubuntu2404
# NICS=2 adds a second network card on the lab network (for host bridge tests).
# Creates only objects prefixed "vspike"; lab/down.sh removes them.
set -euo pipefail

CONTROLLER_PUB=$(cat "$1")
CLUSTER_KEY=$(sed 's/^/      /' "$2")
CLUSTER_PUB=$(cat "$3")

I=${NODE_INDEX:?set NODE_INDEX (5 to 9)}
DISTRO=${DISTRO:?set DISTRO}
name=vspike-n$I
NET=vspike
SUBNET=192.168.150
MAC=52:54:00:5a:00:0$I
DIR=/var/lib/libvirt/images/vspike
V="sudo -n virsh -c qemu:///system"

case "$DISTRO" in
debian13)
  URL=https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
  BASE=$DIR/debian-13-base.qcow2 OSINFO=debian13 ;;
rocky10)
  URL=https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2
  BASE=$DIR/rocky-10-base.qcow2 OSINFO=rocky10 ;;
fedora43)
  # The file name carries a build number: take the first Generic qcow2 listed.
  IDX=https://dl.fedoraproject.org/pub/fedora/linux/releases/43/Cloud/x86_64/images/
  URL=$IDX$(curl -fsSL "$IDX" | grep -o 'Fedora-Cloud-Base-Generic-43-[0-9.]*\.x86_64\.qcow2' | head -n 1)
  BASE=$DIR/fedora-43-base.qcow2 OSINFO=fedora43 ;;
ubuntu2404)
  URL=https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
  BASE=$DIR/ubuntu-2404-base.qcow2 OSINFO=ubuntu24.04 ;;
*) echo "unknown DISTRO $DISTRO"; exit 2 ;;
esac

if $V dominfo "$name" >/dev/null 2>&1; then echo "$name already exists"; exit 0; fi

if ! $V net-dumpxml $NET | grep -q "$MAC"; then
  $V net-update $NET add ip-dhcp-host "<host mac='$MAC' name='$name' ip='$SUBNET.1$I'/>" --live --config
fi
if ! sudo -n test -s "$BASE"; then
  sudo -n curl -fsSL -o "$BASE.part" "$URL"
  sudo -n mv "$BASE.part" "$BASE"
fi
# discard=unmap: space freed inside the node is given back to the host.
sudo -n qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$DIR/$name.qcow2" "${DISK_GB:-20}G"

ud=$(mktemp) md=$(mktemp)
printf 'instance-id: %s\nlocal-hostname: %s\n' "$name" "$name" >"$md"
cat >"$ud" <<UD
#cloud-config
hostname: $name
disable_root: false
ssh_pwauth: false
users:
  - name: root
    ssh_authorized_keys:
      - $CONTROLLER_PUB
      - $CLUSTER_PUB
write_files:
  - path: /root/.ssh/id_ed25519
    permissions: '0600'
    content: |
$CLUSTER_KEY
runcmd:
  - [ sh, -c, 'sed -i "s/^#\\?PermitRootLogin.*/PermitRootLogin prohibit-password/" /etc/ssh/sshd_config; systemctl reload sshd 2>/dev/null || systemctl reload ssh 2>/dev/null; true' ]
  - [ touch, /root/vspike-ready ]
UD
extra=()
if [ "${NICS:-1}" = 2 ]; then extra=(--network "network=$NET,model=virtio"); fi
sudo -n virt-install --connect qemu:///system --name "$name" \
  --memory "${RAM_MB:-2048}" --vcpus "${VCPUS:-2}" --cpu host-passthrough \
  --import --disk "path=$DIR/$name.qcow2,bus=virtio,discard=unmap" \
  --network "network=$NET,model=virtio,mac=$MAC" "${extra[@]}" \
  --osinfo "$OSINFO" --graphics none --noautoconsole \
  --cloud-init "user-data=$ud,meta-data=$md"
rm -f "$ud" "$md"
$V list --all | grep -E 'Name|vspike'
