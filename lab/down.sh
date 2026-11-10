#!/usr/bin/env bash
# Removes everything lab/up.sh created. Run ON the KVM host.
# Touches only libvirt objects and files prefixed "vspike".
set -uo pipefail
V="sudo -n virsh -c qemu:///system"
for d in $($V list --all --name | grep '^vspike-n'); do
  $V destroy "$d" 2>/dev/null
  $V undefine "$d" --nvram 2>/dev/null || $V undefine "$d"
done
$V net-destroy vspike 2>/dev/null
$V net-undefine vspike 2>/dev/null
sudo -n rm -rf /var/lib/libvirt/images/vspike
