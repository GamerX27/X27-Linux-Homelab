#!/usr/bin/env bash
set -euo pipefail

REGISTRY="ghcr.io/gamerx27"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/iso-out"
MIN_FREE_GB=20
INSTALLER_IMAGE="ghcr.io/jasonn3/build-container-installer:v1.5.0"

usage() {
  echo "Usage: $0 [--usb] [--tag TAG] [bare-metal|vm]"
  echo "  bare-metal  x27-linux-homelab (default)"
  echo "  vm          x27-linux-homelab-vm"
  echo "  --usb       write the ISO to a USB drive after building"
  echo "  --tag TAG   image tag to install and follow (default br-testing-45, the testing"
  echo "              branch's Fedora 45 build)"
}

TARGET=""
USB=0
TAG=""

# Writes $1 to a USB drive picked by the user. Erases the drive.
burn_usb() {
  local iso="$1" dev choice confirm iso_size dev_size mp part
  local -a devs=()

  if [ ! -t 0 ]; then
    echo "--usb needs an interactive terminal." >&2
    return 1
  fi

  mapfile -t devs < <(lsblk -bdpno NAME,TRAN,TYPE,SIZE | awk '$2=="usb" && $3=="disk" && $4>0 {print $1}')
  if [ "${#devs[@]}" -eq 0 ]; then
    echo "No USB drives found." >&2
    return 1
  fi

  echo
  echo "USB drives:"
  for i in "${!devs[@]}"; do
    printf "  %d) %s  %s  %s\n" "$((i + 1))" "${devs[$i]}" \
      "$(lsblk -dno SIZE "${devs[$i]}" | xargs)" \
      "$(lsblk -dno VENDOR,MODEL "${devs[$i]}" | xargs)"
  done
  read -rp "Write ${iso##*/} to which drive? [1-${#devs[@]}, empty to skip] " choice
  if [ -z "$choice" ]; then
    echo "Skipped writing to USB."
    return 0
  fi
  if ! [[ "$choice" =~ ^[0-9]+$ ]] || [ "$choice" -lt 1 ] || [ "$choice" -gt "${#devs[@]}" ]; then
    echo "Invalid choice: $choice" >&2
    return 1
  fi
  dev="${devs[$((choice - 1))]}"

  iso_size=$(stat -c %s "$iso")
  dev_size=$(lsblk -bdno SIZE "$dev")
  if [ "$iso_size" -gt "$dev_size" ]; then
    echo "${dev} is too small: $((dev_size / 1024 / 1024))MB, ISO needs $((iso_size / 1024 / 1024))MB." >&2
    return 1
  fi

  while read -r mp; do
    case "$mp" in
      /|/boot|/boot/efi|/home|/var|/usr|/sysroot|/etc)
        echo "${dev} holds ${mp}, refusing to write to it." >&2
        return 1
        ;;
    esac
  done < <(lsblk -lnpo MOUNTPOINTS "$dev")

  echo "Everything on ${dev} will be erased."
  read -rp "Type ${dev} to confirm: " confirm
  if [ "$confirm" != "$dev" ]; then
    echo "Aborted."
    return 1
  fi

  while read -r part mp; do
    [ -n "$mp" ] && sudo umount "$part"
  done < <(lsblk -lnpo NAME,MOUNTPOINT "$dev")

  echo "Writing ${iso##*/} to ${dev}"
  sudo dd if="$iso" of="$dev" bs=4M status=progress oflag=direct conv=fsync
  sync
  echo "Done: written to ${dev}, it can be removed now."
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --usb)
      USB=1
      ;;
    --tag)
      if [ "$#" -lt 2 ]; then
        echo "--tag needs a value." >&2
        usage >&2
        exit 1
      fi
      TAG="$2"
      shift
      ;;
    --tag=*)
      TAG="${1#--tag=}"
      ;;
    -*)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
    *)
      if [ -n "$TARGET" ]; then
        echo "Unexpected extra argument: $1" >&2
        usage >&2
        exit 1
      fi
      TARGET="$1"
      ;;
  esac
  shift
done
TARGET="${TARGET:-bare-metal}"

case "$TARGET" in
  bare-metal) IMAGE="x27-linux-homelab" ;;
  vm)         IMAGE="x27-linux-homelab-vm" ;;
  *)
    echo "Unknown target: $TARGET" >&2
    usage >&2
    exit 1
    ;;
esac

# The installed system keeps following this tag on updates. This is the testing branch,
# so it defaults to the Fedora 45 testing build, not production's :44.
TAG="${TAG:-br-testing-45}"
if ! [[ "$TAG" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "Invalid tag: $TAG" >&2
  exit 1
fi

ISO_NAME="${IMAGE}.iso"
IMAGE_REF="${REGISTRY}/${IMAGE}:${TAG}"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not found on PATH." >&2
  exit 1
fi

mkdir -p "$OUT_DIR"

# Root-owned files from earlier builds (build-container-installer's -CHECKSUM) need sudo.
echo "Removing old files in ${OUT_DIR}"
sudo find "$OUT_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf {} +

AVAIL_GB=$(( $(df --output=avail -k "$OUT_DIR" | tail -n1) / 1024 / 1024 ))
if [ "$AVAIL_GB" -lt "$MIN_FREE_GB" ]; then
  echo "WARNING: only ${AVAIL_GB}GB free at ${OUT_DIR}, ${MIN_FREE_GB}GB+ recommended."
fi

# Removes the image pulled from GHCR (unless it was already here) on exit, whether the
# build succeeded or not.
PULLED=0
sudo docker image inspect "$IMAGE_REF" >/dev/null 2>&1 || PULLED=1
cleanup() {
  if [ "$PULLED" = 1 ]; then
    echo "Removing ${IMAGE_REF}"
    sudo docker rmi "$IMAGE_REF" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Not `bluebuild generate-iso`: BlueBuild CLI (v0.9.37) pins build-container-installer v1.4.0,
# whose lorax templates strip /usr/sbin/load_policy. Anaconda 44.30 runs it on exit, crashes,
# and hangs at the end-of-install Reboot button. v1.5.0 keeps it. Same args as iso.yml:
# stock Fedora kernel (signed), so no secure boot key enrollment, and no Flatpaks, plus
# iso/ext4.tmpl to make ext4 the installer's default filesystem.
# --network host: Docker's bridge network intermittently refused connections while the host
# reached the same servers fine.
echo "Building ${ISO_NAME} from ${IMAGE_REF}"
sudo docker pull "$IMAGE_REF"
sudo docker run --rm --privileged --network host \
  -v "${OUT_DIR}:/build-container-installer/build" \
  -v dnf-cache:/cache/dnf/ \
  -v "${REPO_ROOT}/iso:/x27-iso:ro" \
  "${INSTALLER_IMAGE}" \
  VARIANT=Server \
  "ISO_NAME=build/${ISO_NAME}" \
  DNF_CACHE=/cache/dnf \
  WEB_UI=false \
  ADDITIONAL_TEMPLATES=/x27-iso/ext4.tmpl \
  "IMAGE_NAME=${IMAGE}" \
  "IMAGE_REPO=${REGISTRY}" \
  "IMAGE_TAG=${TAG}" \
  VERSION=45
cd "$OUT_DIR"
sudo chown "$(id -un):$(id -gn)" "$ISO_NAME"
sudo rm -f "${ISO_NAME}-CHECKSUM"

echo "Generating checksum"
sha256sum "$ISO_NAME" > "${ISO_NAME}.sha256sum"

echo "Done: ${OUT_DIR}/${ISO_NAME}"
echo "      ${OUT_DIR}/${ISO_NAME}.sha256sum"

if [ "$USB" = 1 ]; then
  burn_usb "${OUT_DIR}/${ISO_NAME}"
fi
