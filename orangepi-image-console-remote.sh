#!/usr/bin/env bash
set -Eeuo pipefail

image="$1"
root_partition="$2"
boot_partition="$3"
keep_mounts="$4"
mount_root="/mnt/orangepi-image"
loop_device=""
root_mounted=0
boot_mounted=0

cleanup() {
    status=$?
    if (( keep_mounts == 1 )); then
        printf '\nMounts kept at %s. Loop device: %s\n' "$mount_root" "$loop_device"
        exit "$status"
    fi
    set +e
    if (( boot_mounted )); then umount "$mount_root/boot"; fi
    if (( root_mounted )); then umount -R "$mount_root"; fi
    if [[ -n "$loop_device" ]]; then losetup -d "$loop_device"; fi
    exit "$status"
}
trap cleanup EXIT INT TERM

[[ -f "$image" ]] || { printf 'Image does not exist: %s\n' "$image" >&2; exit 1; }
command -v losetup >/dev/null || { echo "losetup is required" >&2; exit 1; }
command -v mount >/dev/null || { echo "mount is required" >&2; exit 1; }
command -v chroot >/dev/null || { echo "chroot is required" >&2; exit 1; }

if [[ ! -x /usr/bin/qemu-aarch64-static && ! -x /usr/bin/qemu-aarch64 ]]; then
    echo "Installing ARM64 chroot support in the VM..."
    apt-get update
    apt-get install -y qemu-user-binfmt binfmt-support
fi

loop_device="$(losetup --find --show --partscan "$image")"
root_device="${loop_device}p${root_partition}"
boot_device=""
[[ -n "$boot_partition" ]] && boot_device="${loop_device}p${boot_partition}"
if [[ ! -b "$root_device" ]]; then
    detected_root="$(lsblk -lnpo NAME,TYPE "$loop_device" | awk '$2 == "part" { print $1; exit }')"
    [[ -n "$detected_root" ]] || {
        echo "No partition was found in the image attached as $loop_device" >&2
        lsblk "$loop_device" >&2
        exit 1
    }
    echo "Requested root partition $root_device was not found; using $detected_root"
    root_device="$detected_root"
    if [[ "$boot_device" == "$root_device" ]]; then
        boot_device=""
    fi
fi

mkdir -p "$mount_root"
mount "$root_device" "$mount_root"
root_mounted=1

if [[ -n "$boot_device" ]]; then
    [[ -b "$boot_device" ]] || { echo "Boot partition not found: $boot_device" >&2; exit 1; }
    mkdir -p "$mount_root/boot"
    mount "$boot_device" "$mount_root/boot"
    boot_mounted=1
fi

mkdir -p "$mount_root"/{dev,dev/pts,proc,sys,run}
mount --rbind /dev "$mount_root/dev"
mount --make-rslave "$mount_root/dev"
mount -t proc proc "$mount_root/proc"
mount -t sysfs sys "$mount_root/sys"
mount -t tmpfs tmpfs "$mount_root/run"
qemu_binary="/usr/bin/qemu-aarch64-static"
[[ -x "$qemu_binary" ]] || qemu_binary="/usr/bin/qemu-aarch64"
[[ -x "$qemu_binary" ]] || {
    echo "ARM64 QEMU emulator was not installed" >&2
    exit 1
}
cp "$qemu_binary" "$mount_root/usr/bin/"
cp -L /etc/resolv.conf "$mount_root/etc/resolv.conf"

printf '\nEntering %s. Type exit to return and unmount the image.\n\n' "$mount_root"
chroot "$mount_root" /bin/bash -c '
    export HOME=/root LC_ALL=C
    export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
    exec /bin/bash -i
'
