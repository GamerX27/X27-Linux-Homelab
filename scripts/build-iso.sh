#!/usr/bin/env bash
set -euo pipefail

REGISTRY="ghcr.io/gamerx27"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/iso-out"
MIN_FREE_GB=20
INSTALLER_IMAGE="ghcr.io/jasonn3/build-container-installer:v1.5.0"

usage() {
  echo "Usage: $0 [bare-metal|vm]"
  echo "  bare-metal  x27-linux-homelab (default)"
  echo "  vm          x27-linux-homelab-vm"
}

TARGET=""
for arg in "$@"; do
  case "$arg" in
    -h|--help)
      usage
      exit 0
      ;;
    *)
      if [ -n "$TARGET" ]; then
        echo "Unexpected extra argument: $arg" >&2
        usage >&2
        exit 1
      fi
      TARGET="$arg"
      ;;
  esac
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

ISO_NAME="${IMAGE}.iso"
IMAGE_REF="${REGISTRY}/${IMAGE}:latest"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not found on PATH." >&2
  exit 1
fi

mkdir -p "$OUT_DIR"

AVAIL_GB=$(( $(df --output=avail -k "$OUT_DIR" | tail -n1) / 1024 / 1024 ))
if [ "$AVAIL_GB" -lt "$MIN_FREE_GB" ]; then
  echo "WARNING: only ${AVAIL_GB}GB free at ${OUT_DIR}, ${MIN_FREE_GB}GB+ recommended."
fi

# Not `bluebuild generate-iso`: BlueBuild CLI (v0.9.37) pins build-container-installer v1.4.0,
# whose lorax templates strip /usr/sbin/load_policy. Anaconda 44.30 runs it on exit, crashes,
# and hangs at the end-of-install Reboot button. v1.5.0 keeps it. Same args as iso.yml:
# stock Fedora kernel (signed), so no secure boot key enrollment, and no Flatpaks.
echo "Building ${ISO_NAME} from ${IMAGE_REF}"
rm -f "${OUT_DIR}/${ISO_NAME}" "${OUT_DIR}/${ISO_NAME}.sha256sum"
sudo docker pull "$IMAGE_REF"
sudo docker run --rm --privileged \
  -v "${OUT_DIR}:/build-container-installer/build" \
  -v dnf-cache:/cache/dnf/ \
  "${INSTALLER_IMAGE}" \
  VARIANT=Server \
  "ISO_NAME=build/${ISO_NAME}" \
  DNF_CACHE=/cache/dnf \
  WEB_UI=false \
  "IMAGE_NAME=${IMAGE}" \
  "IMAGE_REPO=${REGISTRY}" \
  IMAGE_TAG=latest \
  VERSION=44
cd "$OUT_DIR"
sudo chown "$(id -un):$(id -gn)" "$ISO_NAME"

echo "Generating checksum"
sha256sum "$ISO_NAME" > "${ISO_NAME}.sha256sum"

echo "Done: ${OUT_DIR}/${ISO_NAME}"
echo "      ${OUT_DIR}/${ISO_NAME}.sha256sum"
