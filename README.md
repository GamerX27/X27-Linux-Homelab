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
- Port 53 free for DNS containers (AdGuard Home, Pi-hole): systemd-resolved's stub
  listener is off and `/etc/resolv.conf` points at the upstream servers; UDP buffers raised
  for DNS-over-QUIC
- `dnf` disabled on the installed system (see [Installing software](#installing-software))
- **Bare metal:** `smartmontools` (`smartd` enabled), `nvme-cli`, and `nvtop`
- **VM:** `qemu-guest-agent` enabled; hardware firmware and CPU microcode removed
- `/etc/os-release` names the image and build, e.g. `X27-Linux Homelab 44 (2026-09-24)`

## Install

**Fresh machine:** run the `build-iso` workflow (Actions → build-iso → Run workflow) and
download the ISO from the run's artifacts. Or build it locally (needs Docker and sudo,
20 GB+ free):

```
./scripts/build-iso.sh        # bare metal → iso-out/x27-linux-homelab.iso
./scripts/build-iso.sh vm     # VM         → iso-out/x27-linux-homelab-vm.iso
```

**Already on Fedora bootc / an image-based Fedora:** rebase onto this image.

```
sudo rpm-ostree rebase ostree-unverified-registry:ghcr.io/gamerx27/x27-linux-homelab:44
systemctl reboot
```

Then switch to verified pulls:

```
sudo rpm-ostree rebase ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:44
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
- Updates: `sudo rpm-ostree upgrade`, then reboot. Not `bootc upgrade` (see Notes). There are
  no automatic updates.

## Notes

- `bootc` hard-requires `podman`, so Podman is removed with `rpm -e --nodeps`, which leaves
  bootc installed. `bootc upgrade`/`bootc switch` fail without Podman ("Creating imgstorage:
  ... No such file or directory"), so updates and rebases use `rpm-ostree`, which doesn't
  need Podman. `bootc status` still works. bootc's automatic update timer is masked.
- Images are signed with cosign (`cosign.pub`). The repo needs the private key as the
  `SIGNING_SECRET` Actions secret.
