# X27-Linux Homelab

Minimal headless Fedora 44 server images, built with [BlueBuild](https://blue-build.org/)
on the official [`quay.io/fedora/fedora-bootc`](https://quay.io/repository/fedora/fedora-bootc) image.

- Bare metal: `ghcr.io/gamerx27/x27-linux-homelab`: [recipe.yml](recipes/recipe.yml)
- VM: `ghcr.io/gamerx27/x27-linux-homelab-vm`: [recipe-vm.yml](recipes/recipe-vm.yml)

Both share [common.yml](recipes/common.yml).

## What's in it

- No desktop, no extras: Fedora bootc plus the items below
- Docker CE from Docker's official repo (`docker-ce`, buildx, compose plugin), enabled at boot
- Podman removed
- SSH (`sshd`) enabled
- `htop`
- `dnf` disabled on the installed system (see [Installing software](#installing-software))
- **Bare metal:** `smartmontools` (`smartd` enabled), `nvme-cli`, and `nvtop`
- **VM:** `qemu-guest-agent` enabled; hardware firmware and CPU microcode removed
- `/etc/os-release` names the image and build, e.g. `X27-Linux Homelab 44 (2026-09-24)`

## Install

**Fresh machine:** run the `build-iso` workflow (Actions → build-iso → Run workflow) and
download the ISO from the run's artifacts.

**Already on Fedora bootc / an image-based Fedora:**

```
sudo bootc switch ghcr.io/gamerx27/x27-linux-homelab:44
systemctl reboot
```

Use `x27-linux-homelab-vm` in place of `x27-linux-homelab` for a VM.

To let your user run Docker without sudo: `sudo usermod -aG docker $USER`, then log in again.

### Image tags

- `:44`: follows Fedora 44 and is rebuilt weekly. Use this one.
- `:<date>-44` (e.g. `:20260924-44`): one fixed build, for rolling back.
- `:latest`: the newest build, whatever Fedora version that is.

## Installing software

The OS image is read-only and replaced on every update:

- Services: run them in Docker (`docker compose`)
- System packages: add them to a recipe here and let CI rebuild
- Updates: `sudo bootc upgrade`

## Notes

- `bootc` hard-requires `podman`, so Podman is removed with `rpm -e --nodeps`, which leaves
  bootc installed. Updates and switches still work (they pull through skopeo/ostree). The
  podman-only features (`bootc image …`, logically bound images) do not.
- Images are signed with cosign (`cosign.pub`). The repo needs the private key as the
  `SIGNING_SECRET` Actions secret.
