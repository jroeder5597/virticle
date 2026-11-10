#!/usr/bin/env bash
# Adds one Rocky Linux node (SELinux, modular libvirt daemons) to the lab that
# lab/up.sh built. Run ON the KVM host:
#   bash up-rocky.sh <controller.pub> <cluster> <cluster.pub>
# Creates only objects prefixed "vspike"; lab/down.sh removes them.
set -euo pipefail

CONTROLLER_PUB=$(cat "$1")
CLUSTER_KEY=$(sed 's/^/      /' "$2")
CLUSTER_PUB=$(cat "$3")

I=${NODE_INDEX:-3}
name=vspike-n$I
NET=vspike
SUBNET=192.168.150
MAC=52:54:00:5a:00:0$I
DIR=/var/lib/libvirt/images/vspike
BASE_URL=https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2
BASE=$DIR/rocky-10-base.qcow2
V="sudo -n virsh -c qemu:///system"

if $V dominfo "$name" >/dev/null 2>&1; then echo "$name already exists"; exit 0; fi

if ! $V net-dumpxml $NET | grep -q "$MAC"; then
  $V net-update $NET add ip-dhcp-host "<host mac='$MAC' name='$name' ip='$SUBNET.1$I'/>" --live --config
fi
if ! sudo -n test -s "$BASE"; then
  sudo -n curl -fsSL -o "$BASE.part" "$BASE_URL"
  sudo -n mv "$BASE.part" "$BASE"
fi
sudo -n qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$DIR/$name.qcow2" "${DISK_GB:-20}G"

ud=$(mktemp) md=$(mktemp)
printf 'instance-id: %s\nlocal-hostname: %s\n' "$name" "$name" >"$md"
cat >"$ud" <<UD
#cloud-config
hostname: $name
disable_root: false
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
  - path: /root/.ssh/config
    permissions: '0600'
    content: |
      Host vspike-n* $SUBNET.*
        StrictHostKeyChecking accept-new
runcmd:
  - [ sh, -c, 'dnf install -y qemu-kvm libvirt-daemon-kvm libvirt-daemon-config-network libvirt-client qemu-img edk2-ovmf swtpm swtpm-tools nmap-ncat curl tar; echo \$? > /root/vspike-setup.rc' ]
  # virtproxyd serves the classic /run/libvirt/libvirt-sock in front of the modular daemons.
  - [ sh, -c, 'systemctl enable --now virtqemud.socket virtstoraged.socket virtnetworkd.socket virtnodedevd.socket virtproxyd.socket' ]
  - [ sh, -c, 'virsh net-autostart default; virsh net-start default; true' ]
  - [ sh, -c, 'mkdir -p /var/lib/libvirt/images && virsh pool-define-as default dir --target /var/lib/libvirt/images && virsh pool-autostart default && virsh pool-start default; true' ]
  - [ touch, /root/vspike-ready ]
UD
sudo -n virt-install --connect qemu:///system --name "$name" \
  --memory "${RAM_MB:-3072}" --vcpus "${VCPUS:-2}" --cpu host-passthrough \
  --import --disk "path=$DIR/$name.qcow2,bus=virtio" \
  --network "network=$NET,model=virtio,mac=$MAC" \
  --osinfo rocky10 --graphics none --noautoconsole \
  --cloud-init "user-data=$ud,meta-data=$md"
rm -f "$ud" "$md"
$V list --all | grep -E 'Name|vspike'
