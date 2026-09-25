# X27-Linux Homelab

Docker-first, minimal headless Fedora 44 server images, built with
[BlueBuild](https://blue-build.org/) on the official
[`quay.io/fedora/fedora-bootc`](https://quay.io/repository/fedora/fedora-bootc) image.

**Docker first:** the OS is a small, read-only base that updates itself. Everything you run
on it (services, apps, tools) goes in Docker containers, managed with `docker compose`.

- Bare metal: `ghcr.io/gamerx27/x27-linux-homelab`: [recipe.yml](recipes/recipe.yml)
- VM: `ghcr.io/gamerx27/x27-linux-homelab-vm`: [recipe-vm.yml](recipes/recipe-vm.yml)

Both share [common.yml](recipes/common.yml).

## What's in it

- No desktop, no extras: Fedora bootc plus the items below
- Docker CE from Docker's official repo (`docker-ce`, buildx, compose plugin), enabled at boot
- `docker-compose-update`: check your containers for newer images and pick which to update (see
  [Updating containers](#updating-containers))
- Podman removed
- SSH (`sshd`) enabled
- fish as the default login shell (console and SSH), `htop`, `git`, `wget`, `lspci` (pciutils), `ncdu`, `zip`, `unzip`
- `autoupdate`: turn automatic updates on or off, on a schedule of your choice (see
  [Automatic updates](#automatic-updates)), with optional [Gotify](https://gotify.net) messages
- Port 53 free for DNS containers (AdGuard Home, Pi-hole): systemd-resolved's stub
  listener is off and `/etc/resolv.conf` points at the upstream servers; UDP buffers raised
  for DNS-over-QUIC
- Quiet login screen: the console only shows kernel errors (the rest is in `journalctl -k`)
- `dnf` disabled on the installed system (see [Installing software](#installing-software))
- **Bare metal:** `smartmontools` (`smartd` enabled), `nvme-cli`, `nvtop`, and `lm_sensors`
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

The installer defaults to ext4 (on LVM) for automatic partitioning instead of Fedora Server's
XFS (`iso/ext4.tmpl`). You can still pick another filesystem in custom partitioning.

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

fish is the login shell (console and SSH) for every regular user. New users get it from
`/etc/default/useradd`. Existing users who still have bash are switched once at boot by
`fish-default-shell.service`. To go back to bash: `sudo chsh -s /bin/bash $USER`, and it stays
that way.

### Image tags

- `:44`: follows Fedora 44 and is rebuilt weekly (Sundays, 02:00 UTC). Use this one.
- `:<date>-44` (e.g. `:20260924-44`): one fixed build, for rolling back.
- `:latest`: the newest build, whatever Fedora version that is.

## Installing software

The OS image is read-only and replaced on every update:

- Services: run them in Docker (`docker compose`)
- System packages: add them to a recipe here and let CI rebuild
- Updates: `sudo rpm-ostree upgrade`, then reboot. Not `bootc upgrade` (see Notes). Or turn
  on [automatic updates](#automatic-updates).

## Automatic updates

Off by default. `autoupdate` turns them on at the time you pick. When a new image is
available, it's installed and the system reboots. When there's nothing new, nothing happens.

```
autoupdate                           # interactive menu
autoupdate status                    # on/off, schedule, next and last run
autoupdate on daily 04:00
autoupdate on weekly sun 3:30am
autoupdate on monthly 15 11pm        # days 29-31 skip months that don't have them
autoupdate off
```

Times can be 24-hour (`04:00`, `23:30`) or 12-hour (`4am`, `3:30 PM`, `12am` = midnight).
`autoupdate on` writes `/etc/systemd/system/autoupdate.timer`, which runs `autoupdate.service`
(`rpm-ostree upgrade`, then a reboot only if an update was staged). It lives in `/etc`, so the
schedule survives updates. If the machine was off at the scheduled time, the update runs at the
next boot.

### Gotify messages

Optional. With a [Gotify](https://gotify.net) server set up, you get a message just before the
machine reboots into an update (with the old and new version), and one when an update fails.
Nothing is sent when there's no update. Create an app in Gotify for its token, then:

```
autoupdate gotify https://gotify.example.com   # asks for the app token, sends a test message
autoupdate gotify test                         # send another test message
autoupdate gotify off
```

The URL and token are stored in `/etc/autoupdate.conf` (root only). Nothing is saved if the
test message fails.

## Updating containers

`docker-compose-update` checks every image your containers use against its registry and shows
the ones with a newer version in a menu, so you pick exactly what gets updated.

```
docker-compose-update                 # scans ~/docker for compose files
docker-compose-update /srv/stacks     # or another directory
```

Keys: `↑/↓` (or `k/j`) move, `Space` toggles, `a` selects all, `Enter` updates, `q` quits.

It finds images in four places, and updates each accordingly:

- Compose files under the scanned directory: pulled, then `docker compose up -d`
- Compose files elsewhere on the host (from running containers): same
- Portainer / external stacks: pulled; redeploy them in Portainer to apply
- `docker run` containers: pulled; optionally recreated with the same settings (via
  [runlike](https://github.com/lavie/runlike))

Afterwards it offers to remove the old images. Only what you select is changed. Set
`COMPOSE_ROOT` to scan somewhere other than `~/docker` by default.

## Releases

Every image build publishes a [GitHub Release](../../releases) that lists what changed: key
versions (kernel, systemd, Docker, ...), repo commits, and the packages updated, added or
removed in each image. The package lists are attached to each release.

- `vYYYY.MM.DD` (e.g. `v2026.09.27`): the weekly rebuild
- `vYYYY.MM.DD.N` (e.g. `v2026.09.24.1`): minor releases for changes pushed in between

## Notes

- `bootc` hard-requires `podman`, so Podman is removed with `rpm -e --nodeps`, which leaves
  bootc installed. `bootc upgrade`/`bootc switch` fail without Podman ("Creating imgstorage:
  ... No such file or directory"), so updates and rebases use `rpm-ostree`, which doesn't
  need Podman. `bootc status` still works. bootc's automatic update timer is masked.
- Images are signed with cosign (`cosign.pub`). The repo needs the private key as the
  `SIGNING_SECRET` Actions secret.
