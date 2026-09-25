#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
    cat <<'EOF'
Usage:
  orangepi-image-console.sh -h VM_HOST -u VM_USER -i IMAGE [options]

Options:
  -h HOST       VM hostname or IP address
  -u USER       SSH user in the VM
  -i IMAGE      Image path as seen inside the VM
  -p PORT       SSH port (default: 22)
  -r PARTITION  Root partition number (default: 2)
  -b PARTITION  Boot partition number; omit if there is no separate boot partition
  -k            Keep mounts and loop device after the shell exits
  --help        Show this help
EOF
}

host=""
user=""
image=""
port=22
root_partition=2
boot_partition=""
keep_mounts=0

while (($#)); do
    case "$1" in
        -h) host="${2:?missing value for -h}"; shift 2 ;;
        -u) user="${2:?missing value for -u}"; shift 2 ;;
        -i) image="${2:?missing value for -i}"; shift 2 ;;
        -p) port="${2:?missing value for -p}"; shift 2 ;;
        -r) root_partition="${2:?missing value for -r}"; shift 2 ;;
        -b) boot_partition="${2:?missing value for -b}"; shift 2 ;;
        -k) keep_mounts=1; shift ;;
        --help) usage; exit 0 ;;
        *) usage >&2; exit 2 ;;
    esac
done

if [[ -z "$host" || -z "$user" || -z "$image" ]]; then
    usage >&2
    exit 2
fi

ssh_target="${user}@${host}"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
remote_script="/tmp/orangepi-image-console-$$.sh"

scp -q -P "$port" "$script_dir/orangepi-image-console-remote.sh" \
    "$ssh_target:$remote_script"

cleanup_remote_script() {
    ssh -p "$port" "$ssh_target" "rm -f $(printf '%q' "$remote_script")" >/dev/null 2>&1 || true
}
trap cleanup_remote_script EXIT

printf 'Connecting to %s. Enter the VM sudo password when prompted.\n' "$ssh_target"
printf -v remote_command 'sudo bash %q %q %q %q %q; status=$?; rm -f %q; exit $status' \
    "$remote_script" "$image" "$root_partition" "$boot_partition" "$keep_mounts" "$remote_script"
ssh -tt -p "$port" "$ssh_target" "$remote_command"
