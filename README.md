# X27-Linux Homelab

> **`testing-alma` branch: AlmaLinux 10.** Builds `x27-linux-homelab-alma-testing` and
> `x27-linux-homelab-vm-alma-testing` (`:10`). Everything below applies; the install commands
> set up `stable`, then `channel testing-alma` moves a machine here. May break.
> AlmaLinux 10 is supported until 2035, against about 13 months for a Fedora release.

A minimal, headless server OS for running Docker. The OS is a small read-only image that
updates itself; everything you run on it goes in containers.

Built with [BlueBuild](https://blue-build.org/) on AlmaLinux's official
[bootc image](https://quay.io/repository/almalinuxorg/almalinux-bootc), in two flavors:

| Flavor | Image | Recipe |
|---|---|---|
| Bare metal | `ghcr.io/gamerx27/x27-linux-homelab` | [recipe.yml](recipes/recipe.yml) |
| VM | `ghcr.io/gamerx27/x27-linux-homelab-vm` | [recipe-vm.yml](recipes/recipe-vm.yml) |

Both share [common.yml](recipes/common.yml).

**Contents:** [Install](#install) · [Channels](#channels) · [OS updates](#os-updates) ·
[Container updates](#container-updates) · [Dashboard](#dashboard) · [NetBird](#netbird) ·
[What's included](#whats-included) · [Good to know](#good-to-know) ·
[For maintainers](#for-maintainers)

---

## Install

### On a new machine

Get the installer ISO in one of two ways:

- **From CI:** Actions → **build-iso** → Run workflow, then download the ISO from the run's
  artifacts.
- **Locally** (needs Docker, sudo and 20 GB free):

  ```sh
  ./scripts/build-iso.sh        # bare metal → iso-out/x27-linux-homelab.iso
  ./scripts/build-iso.sh vm     # VM         → iso-out/x27-linux-homelab-vm.iso
  ```

Automatic partitioning uses plain ext4 partitions (no LVM). Custom partitioning still lets you
pick LVM, Btrfs or something else. Tick **Make this user administrator** for your user: you need it to log in to the
dashboard.

### On an existing Fedora bootc system

Rebase onto the image, reboot, then rebase again to switch to signed (verified) pulls:

```sh
sudo rpm-ostree rebase ostree-unverified-registry:ghcr.io/gamerx27/x27-linux-homelab:44
systemctl reboot

sudo rpm-ostree rebase ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:44
systemctl reboot
```

For a VM, use `x27-linux-homelab-vm` instead.

---

## Channels

Each machine follows one channel and only gets updates from it.

| Channel | Base | Use it for |
|---|---|---|
| `stable` | Fedora 44 | Everyday use. The default. |
| `testing` | Fedora 45 | Trying the next Fedora. May break. |
| `testing-alma` | AlmaLinux 10 | Trying AlmaLinux. May break. Older kernel (6.12 with backports), so very new hardware may not work. |

Switch with `channel`:

```sh
channel                    # show the current channel
channel testing            # switch, then asks whether to reboot
channel stable --reboot    # switch and reboot right away
```

The switch applies after the reboot. Your data, containers and settings stay; only the OS
changes. To undo it, roll back (`sudo rpm-ostree rollback` or **Roll back** in the dashboard)
before the next update.

<details>
<summary>Image names and tags</summary>

All images are at `ghcr.io/gamerx27/`, signed, and rebuilt every Sunday at 02:00 UTC with
security updates only. On the 1st of the month the base image is refreshed and everything is
upgraded.

| Channel | Bare metal | VM | Branch |
|---|---|---|---|
| `stable` | `x27-linux-homelab:44` | `x27-linux-homelab-vm:44` | `main` |
| `testing` | `x27-linux-homelab-testing:45` | `x27-linux-homelab-vm-testing:45` | `testing` |
| `testing-alma` | `x27-linux-homelab-alma-testing:10` | `x27-linux-homelab-vm-alma-testing:10` | `testing-alma` |

Every image has three tags (shown for `stable`):

- `:44` follows the channel. This is what `channel` uses.
- `:<date>-44` (e.g. `:20260924-44`) is one fixed build, for pinning or rolling back.
- `:latest` is the newest build, whatever its version.

Only `stable` gets [releases](#releases).

</details>

---

## OS updates

**By hand:** `sudo rpm-ostree upgrade`, then reboot. (Not `bootc upgrade`; see
[Good to know](#good-to-know).)

### Automatic updates

Off by default. Pick a schedule, and at that time the machine checks for a newer image. If
there is one, it installs it and reboots; if not, nothing happens.

```sh
autoupdate                       # interactive menu
autoupdate status                # schedule, next check, last result
autoupdate on daily 04:00
autoupdate on weekly sun 3:30am
autoupdate on monthly 15 11pm    # days 29-31 skip months that don't have them
autoupdate off
```

Times can be 24-hour (`04:00`) or 12-hour (`4am`, `3:30 PM`). If the machine is off at the
scheduled time, the check runs at the next boot. After the reboot, the machine checks that it
came up on the new image.

### Gotify messages

Optional. With a [Gotify](https://gotify.net) server, you get a message before the machine
reboots into an update, when it's back, and when an update fails. Nothing is sent when there's
no update.

In Gotify, create an app (**Apps → Create Application**) and copy its token, then:

```sh
autoupdate gotify https://gotify.example.com   # asks for the token, sends a test message
autoupdate gotify test                         # send another test message
autoupdate gotify show                         # show the stored URL and token
autoupdate gotify off
```

Nothing is saved if the test message doesn't arrive. The dashboard's **Features** page can do
the same.

<details>
<summary>Example update message</summary>

```
X27-Linux Homelab 44 (2026-09-24) → X27-Linux Homelab 44 (2026-09-30)
Image: ghcr.io/gamerx27/x27-linux-homelab:44
Digest: sha256:5f2e…
Packages: 12 upgraded, 1 removed, 1 added

Upgraded (12):
  kernel 6.16.3-200.fc44.x86_64 -> 6.16.5-200.fc44.x86_64
  ...

Rebooting now.
```

</details>

<details>
<summary>Where things are stored</summary>

- Schedule: `/etc/systemd/system/autoupdate.timer` (in `/etc`, so it survives updates). It runs
  `autoupdate.service`.
- Progress of the last update: `/var/lib/autoupdate/state.json`, output in `last.log` next to it.
  `autoupdate status` and the dashboard read these.
- Gotify URL and token: `/etc/autoupdate.conf` (root only).

</details>

---

## Container updates

`docker-compose-update` checks the images your containers use, lists the ones with a newer
version, and updates only what you pick.

```sh
docker-compose-update                 # looks in ~/docker
docker-compose-update /srv/stacks     # or another folder
```

Keys: `↑`/`↓` (or `k`/`j`) move, `Space` selects, `a` selects all, `Enter` updates, `q` quits.
Afterwards it offers to remove the old images. Set `COMPOSE_ROOT` to change the default folder.

| Where the container comes from | What an update does |
|---|---|
| Compose file (in the folder, or anywhere for a running container) | Pulls, then `docker compose up -d` |
| Portainer or other external stack | Pulls; redeploy in Portainer to apply |
| `docker run` | Pulls; can recreate it with the same settings (via [runlike](https://github.com/lavie/runlike)) |

The dashboard can do this too, for all your machines at once.

---

## Dashboard

A web UI for this machine and your other machines running the image. Off by default.

One machine is the **main node** and serves the UI. Others join it as **nodes**, and you
manage all of them from the same page.

### Set up

**1. On the main node:**

```sh
dashboard enable        # prints the address, e.g. https://192.168.1.10:9090
```

Log in with your Linux username and password; only users in `wheel` can. The certificate is
self-signed, so the browser warns once. `dashboard status` prints its fingerprint to compare.

**2. On each other machine:**

```sh
dashboard enable node   # prints a one-time pairing password
```

Then in the UI: **Add node**, enter its address (add `:port` if it isn't 9090) and the
password. To use the terminal on a node, your user needs an account in `wheel` there too.

> **Security:** anyone who logs in has root-level control. The image has no firewall, so keep
> the dashboard on your LAN or behind a VPN, never open to the internet.

### What you can do

**For all nodes (main page):** see every node with its version, update the OS on several
nodes at once, and update containers on all nodes.

**For each node:**

| Page | What's on it |
|---|---|
| Overview | CPU, memory, disks, network, temperatures, uptime, OS version |
| Updates | Current OS image, available update, last update and its log; update, reboot or roll back |
| Docker | Your apps (compose stacks) and their containers: start, stop, update, edit, logs, remove. Plus images, volumes and networks for cleanup |
| Files | Your home folder: browse, edit, upload, download, rename, delete. Same permissions as over SSH |
| Terminal | A shell in the browser, as your user |
| Features | Automatic updates, Gotify messages, and turning services (SSH, smartd, NetBird, Docker…) on or off |
| Settings | Hostname, time zone, network time, reboot and shut down |

Your sidebar picture comes from [Gravatar](https://gravatar.com): click your name and enter
your email. Only a hash of it is stored.

### Apps (compose stacks)

An app is a folder in `~/docker/<name>/` with a `compose.yml`, the same folders
`docker-compose-update` uses. The Docker page also shows compose projects running from other
places.

- **New app:** name it and write the `compose.yml` (and an optional `.env`). Start blank or
  from a **preset** in [X27/Docker-X27-Composes](https://codeberg.org/X27/Docker-X27-Composes).
- **Saving** checks the file with `docker compose config` first and keeps the old version as
  `compose.yml.bak`.
- **Pull & recreate** and **Update** get newer images and recreate the containers. Data in
  volumes and bind mounts is kept. Updates run in the background, one at a time per node, with
  a live log.
- **Remove** runs `docker compose down`. Named volumes are kept, and the folder too unless you
  tick **Also delete the folder**.

To use another preset repository, set `PRESETS_REPO=https://host/owner/repo` in
`/etc/dashboard.conf`. It needs the same `Composes/<name>/compose.yml` layout.

### How an OS update runs

When you update from the dashboard, the main node runs the update and opens the **OS update**
page:

1. All selected nodes download the new image at the same time. Nothing restarts yet.
2. They reboot one at a time, the main node last. Each has to come back on the new image before
   the next one goes.
3. If a node fails, the update stops there. You choose **Reboot the rest** or **Leave them for
   later**.

The update keeps going if you close the page, and survives the main node's own reboot.

### Commands

```sh
dashboard status        # on/off, mode, address, pairing, certificate fingerprint
dashboard nodes         # main node: list nodes
dashboard pair          # node: print a new pairing password
dashboard unpair        # node: forget the main node
dashboard port 9443     # change the port (default 9090)
dashboard disable
```

Settings are in `/etc/dashboard.conf`; certificate, nodes and sessions in `/var/lib/dashboard`.

---

## NetBird

The [NetBird](https://netbird.io) VPN client is installed but off. To join your network:

```sh
sudo systemctl enable --now netbird
sudo netbird up                                   # prints a login link
sudo netbird up --setup-key <KEY>                 # or log in with a setup key
sudo netbird up --management-url https://netbird.example.com   # self-hosted server
netbird status
```

It stays connected across reboots and updates. `sudo netbird down` disconnects, and
`sudo systemctl disable --now netbird` turns it off. You can also switch it on the dashboard's
**Features** page.

Settings go in `/etc/sysconfig/netbird`, state is in `/var/lib/netbird`, logs with
`journalctl -u netbird`.

---

## What's included

| | |
|---|---|
| **Base** | AlmaLinux bootc, no desktop. SSH on. Podman and toolbox removed. EPEL on, for fish, `htop`, `ncdu` and `nvtop`. |
| **Docker** | Docker CE from Docker's repo, with buildx and the compose plugin. On at boot. |
| **Commands** | `channel`, `autoupdate`, `docker-compose-update`, `dashboard` (off), `netbird` (off) |
| **Tools** | fish, `htop`, `git`, `wget`, `lspci`, `ncdu`, `zip`, `unzip` |
| **Bare metal only** | `smartmontools` (smartd on), `nvme-cli`, `nvtop`, `lm_sensors` |
| **VM only** | `qemu-guest-agent` on; no hardware firmware or CPU microcode |
| **DNS-ready** | Port 53 is free for AdGuard Home or Pi-hole, with larger UDP buffers for DNS-over-QUIC |
| **Quiet console** | Only kernel errors on screen; the rest is in `journalctl -k` |

`/etc/os-release` names the build, e.g. `X27-Linux Homelab 44 (2026-09-24)`.

---

## Good to know

**Installing software.** The OS is read-only and replaced on every update, so `dnf` is turned
off. Run services in Docker. For a system package, add it to a recipe here and let CI rebuild.

**Docker without sudo.** Regular users (UID 1000+) are added to the `docker` group at boot.
To take someone out for good: `sudo gpasswd -d <user> docker`. Being in `docker` is effectively
root access.

**fish is the login shell** for every regular user. To go back to bash:
`sudo chsh -s /bin/bash $USER`. It stays that way.

**Port 53.** systemd-resolved's stub listener is off and `/etc/resolv.conf` points straight at
the upstream DNS servers, so a DNS container can use port 53.

**Use `rpm-ostree`, not `bootc`, for updates.** `bootc` needs Podman, which is removed, so
`bootc upgrade` and `bootc switch` fail. `bootc status` still works. bootc's own update timer
is turned off.

---

## For maintainers

### Releases

Every `stable` build publishes a [GitHub Release](../../releases) listing what changed: key
versions (kernel, systemd, Docker…), commits, and the packages upgraded, added or removed.

- `vYYYY.MM.DD`: the weekly security rebuild, or the monthly full update on the 1st
- `vYYYY.MM.DD.N`: builds from changes pushed in between

### Signing

Images are signed with cosign ([cosign.pub](cosign.pub)). The repo needs the private key as
the `SIGNING_SECRET` Actions secret.

### Trying the OS update page

`DASHBOARD_FAKE_OS=1 dashboard serve --dev` fakes a node with an update (download, a 15-second
reboot, done). `DASHBOARD_FAKE_OS=fail` makes it come back on the old image.
