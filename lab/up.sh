#!/usr/bin/env bash
# Builds the nested test lab on a KVM host. Run ON the KVM host, as a user with
# passwordless sudo:   bash up.sh <controller.pub> <cluster> <cluster.pub>
#
# Creates only objects prefixed "vspike": one NAT network, one image directory,
# and N nested libvirt nodes (Debian 13) that the spike migrates VMs between.
set -euo pipefail

CONTROLLER_PUB=$(cat "$1")
CLUSTER_KEY=$(sed 's/^/      /' "$2")
CLUSTER_PUB=$(cat "$3")

NODES=${NODES:-2}
VCPUS=${VCPUS:-2}
RAM_MB=${RAM_MB:-3072}
DISK_GB=${DISK_GB:-20}
NET=vspike
SUBNET=192.168.150
DIR=/var/lib/libvirt/images/vspike
BASE_URL=https://cloud.debian.org/images/cloud/trixie/latest/debian-13-generic-amd64.qcow2
BASE=$DIR/debian-13-base.qcow2
V="sudo -n virsh -c qemu:///system"

sudo -n mkdir -p "$DIR"

if ! $V net-info $NET >/dev/null 2>&1; then
  hosts=""
  for i in $(seq 1 "$NODES"); do
    hosts+="      <host mac='52:54:00:5a:00:0$i' name='vspike-n$i' ip='$SUBNET.1$i'/>"$'\n'
  done
  tmp=$(mktemp)
  cat >"$tmp" <<EOF
<network>
  <name>$NET</name>
  <forward mode='nat'/>
  <bridge name='virbr-vspike' stp='on' delay='0'/>
  <domain name='vspike.lan' localOnly='yes'/>
  <ip address='$SUBNET.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='$SUBNET.100' end='$SUBNET.200'/>
$hosts    </dhcp>
  </ip>
</network>
EOF
  $V net-define "$tmp" && $V net-start $NET
  rm -f "$tmp"
fi

if ! sudo -n test -s "$BASE"; then
  sudo -n curl -fsSL -o "$BASE.part" "$BASE_URL"
  sudo -n mv "$BASE.part" "$BASE"
fi

for i in $(seq 1 "$NODES"); do
  name=vspike-n$i
  if $V dominfo "$name" >/dev/null 2>&1; then
    echo "$name already exists, skipping"
    continue
  fi
  sudo -n qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$DIR/$name.qcow2" "${DISK_GB}G"

  ud=$(mktemp) md=$(mktemp)
  printf 'instance-id: %s\nlocal-hostname: %s\n' "$name" "$name" >"$md"
  cat >"$ud" <<EOF
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
  - [ sh, -c, 'apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients dnsmasq-base netcat-openbsd curl ca-certificates ovmf swtpm swtpm-tools; echo \$? > /root/vspike-setup.rc' ]
  - [ sh, -c, 'virsh net-autostart default; virsh net-start default; true' ]
  - [ sh, -c, 'mkdir -p /var/lib/libvirt/images && virsh pool-define-as default dir --target /var/lib/libvirt/images && virsh pool-autostart default && virsh pool-start default; true' ]
  - [ touch, /root/vspike-ready ]
EOF
  sudo -n virt-install --connect qemu:///system --name "$name" \
    --memory "$RAM_MB" --vcpus "$VCPUS" --cpu host-passthrough \
    --import --disk "path=$DIR/$name.qcow2,bus=virtio" \
    --network "network=$NET,model=virtio,mac=52:54:00:5a:00:0$i" \
    --osinfo debian13 --graphics none --noautoconsole \
    --cloud-init "user-data=$ud,meta-data=$md"
  rm -f "$ud" "$md"
done

$V list --all | grep -E 'Name|vspike'
