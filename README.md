# X27-Linux Homelab

> **Testing branch:** these images are built on Fedora 45 and tagged `:br-testing-45`. The
> tags and install steps below describe production (`main`, Fedora 44). See [Branches](#branches).

Docker-first, minimal headless Fedora 45 server images, built with
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
- `dashboard`: a web admin UI for this server and your other servers running the image (see
  [Dashboard](#dashboard)). Off by default
- `netbird`: the [NetBird](https://netbird.io) VPN client. Off by default (see [NetBird](#netbird))
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

Regular users (UID 1000+) are added to the `docker` group at boot by `docker-group.service`,
so the user made in the installer runs Docker without sudo. A user added later gets it at the
next boot (or run `sudo usermod -aG docker <user>` and log in again). Each user is added once,
so `sudo gpasswd -d <user> docker` stays. Membership in `docker` is effectively root access.

fish is the login shell (console and SSH) for every regular user. New users get it from
`/etc/default/useradd`. Existing users who still have bash are switched once at boot by
`fish-default-shell.service`. To go back to bash: `sudo chsh -s /bin/bash $USER`, and it stays
that way.

### Image tags

- `:44`: follows Fedora 44 and is rebuilt weekly (Sundays, 02:00 UTC). Use this one.
- `:<date>-44` (e.g. `:20260924-44`): one fixed build, for rolling back.
- `:latest`: the newest build, whatever Fedora version that is.

### Branches

- `main`: production, Fedora 44. Publishes the tags above and a [release](#releases) per build.
- `testing`: the same images on Fedora 45, tagged `:br-testing-45` and rebuilt weekly with
  main. No releases, and it may break. To try it:
  `sudo rpm-ostree rebase ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:br-testing-45`

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

After the reboot, `autoupdate-verify.service` checks that the machine runs the image it staged.
Every step (checking, downloading, staged, rebooting, done or failed) is written to
`/var/lib/autoupdate/state.json` and the output to `last.log` next to it. `autoupdate status`
shows the last result, and the dashboard follows an update through its reboot with them.

### Gotify messages

Optional. With a [Gotify](https://gotify.net) server set up, you get a message just before the
machine reboots into an update, one when it's back on the new image ("Updated …"), and one when an
update fails, including when the machine comes back on the old image. Nothing is sent when there's no
update. The update message shows what changed, like `rpm-ostree` does:

```
X27-Linux Homelab 44 (2026-09-24) → X27-Linux Homelab 44 (2026-09-30)
Image: ghcr.io/gamerx27/x27-linux-homelab:44
Digest: sha256:5f2e…
Packages: 12 upgraded, 1 removed, 1 added

Upgraded (12):
  kernel 6.16.3-200.fc44.x86_64 -> 6.16.5-200.fc44.x86_64
  ...
Removed (1):
  ...
Added (1):
  ...

Rebooting now.
```

Create an app in Gotify (Apps → Create Application) for its token, then:

```
autoupdate gotify https://gotify.example.com   # asks for the app token, sends a test message
autoupdate gotify test                         # send another test message
autoupdate gotify show                         # show the stored URL and app token
autoupdate gotify off
```

The URL and token are stored in `/etc/autoupdate.conf` (root only). Nothing is saved if the
test message fails. The dashboard's **Features** page does the same, shows the stored token
(hidden until you click Show, with Copy), and lets you change the URL without entering the token
again.

## NetBird

The [NetBird](https://netbird.io) client is installed but off. To join your NetBird network:

```
sudo systemctl enable --now netbird
sudo netbird up                                  # prints a login link
sudo netbird up --setup-key <KEY>                # or use a setup key (no browser needed)
sudo netbird up --management-url https://netbird.example.com   # self-hosted server
netbird status
```

It stays connected across reboots and updates. `sudo netbird down` disconnects;
`sudo systemctl disable --now netbird` turns it off again. It can also be switched on or off in
the dashboard's **Features** page. Settings for the service (e.g. `NB_MANAGEMENT_URL`) go in
`/etc/sysconfig/netbird`; its state is in `/var/lib/netbird`. Logs: `journalctl -u netbird`.

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

## Dashboard

A web admin UI, off by default. One server is the **main node** and serves the UI. Other
servers running this image can be added to it as **nodes** and managed from the same page.

For each node it shows:

- **Overview:** CPU, memory, disks, network, temperatures, uptime, OS and image version
- **Updates:** the OS image: the running version, whether a newer one is out (checked every
  6 hours), the result of the last update (with its log), and buttons to update, reboot or roll
  back. **Update now** opens the [OS update page](#os-update-page)
- **Docker:** three sections. **Apps** lists each stack with its containers inside, image
  update badges, one button for what's needed (Update or Start) and a ⋯ menu for the rest
  (restart, stop, pull & recreate, edit, logs, stats, remove); containers started with
  `docker run` are grouped as Standalone. A strip on top checks all images against their
  registries and updates them. **Images** and **Storage** (volumes, networks) are for cleanup.
  Published ports are links that open the service on the node it runs on
- **Files:** your home folder: browse, edit text files, upload (drag and drop), download
  (folders as `.tar.gz`), rename, move and delete. It runs as your user, so you have exactly
  the permissions you have over SSH
- **Terminal:** a shell in the browser, as the user you logged in with
- **Features:** automatic updates (schedule, on/off), Gotify messages, and turning services
  (SSH, smartd, NetBird, Docker, ...) on or off
- **Settings:** hostname, time zone, network time, reboot and shut down

### Stacks

A stack is a compose project in your `~/docker/<name>/` folder (e.g. `/home/sindre/docker/jellyfin/compose.yml`),
the same place `docker-compose-update` looks. The Stacks section lists those folders plus any other
compose projects running on the node, with Start/Stop, Restart, **Pull & recreate**, Edit and Remove.

- **New stack:** name it, write the `compose.yml` (and an optional `.env`), then Save or Save & start.
  Start from a blank template or a **preset** from
  [X27/Docker-X27-Composes](https://codeberg.org/X27/Docker-X27-Composes) (fetched by the main
  node; its notes are shown). Set `PRESETS_REPO=https://host/owner/repo` in `/etc/dashboard.conf`
  to use another Forgejo/Gitea repository with the same `Composes/<name>/compose.yml` layout.
- Files are checked with `docker compose config` before anything is written, and are owned by
  you. Editing keeps the previous version as `compose.yml.bak`.
- **Pull & recreate** pulls newer images and recreates the containers; data in volumes and bind
  mounts is kept. For a container started with plain `docker run` it makes a new container with
  the same ports, environment, volumes (anonymous ones too), networks and restart policy, and puts
  the old one back if anything fails. Image updates do the same.
- **Update** and **Update all** run in the background, one update at a time per node. The bar
  above the stacks shows what is being pulled or recreated, the stack shows an "Updating" badge
  (others wait as "Queued"), and **Log** shows the output live. When it's done the bar keeps the
  result and its log until you dismiss it, even if you left the page meanwhile.
- **Remove** runs `docker compose down`; named volumes stay. Its folder stays too, unless you
  tick "Also delete the folder" in the dialog, which deletes the folder and everything in it.

On a paired node, stacks live in the same user's `~/docker` on that node.

Your picture in the sidebar comes from [Gravatar](https://gravatar.com): click your name and enter
your email. Only a hash of it is stored, and your browser loads the picture from gravatar.com.

The main page has two update panels for all nodes at once:

- **OS updates:** check every node for a newer image, then **Update…**: pick the nodes and follow
  them on the [OS update page](#os-update-page).
- **Container updates:** check every node's images, then **Update all** pulls and recreates only
  what has a newer image; containers that are up to date aren't restarted.

It also lists every node with its version and a badge when an OS or container update is
available.

### OS update page

An OS update (from **Update…** on the main page, or **Update now** on a node) is run by the main
node, and the UI opens the **OS update** page to follow it:

1. Every node downloads and stages the new image at the same time. Nothing restarts yet.
2. Then the nodes reboot one at a time, paired nodes first and the main node last. Each one has to
   come back on the new image (the booted commit is the one it staged) before the next one goes.
3. If a node fails (the download fails, it doesn't come back within 15 minutes, or it comes back on
   the old image), the update stops there. The nodes that are left keep the update staged, and you
   pick **Reboot the rest** or **Leave them staged**. A node that fails to download is skipped, and
   the others carry on.

Each node shows its steps (Download → Staged → Reboot → Back online → Verified), what it's doing
now (e.g. "Downloading layer 12 of 65") and a live log. **Stop after current node** stops before
the next reboot.

The update keeps running with the page closed. It's saved in `/var/lib/dashboard/rollout.json`, so
it also survives the main node's own reboot, and so do logins (`sessions.json`, which stores only a
hash of each session cookie). While the main node reboots, the page shows that it's reconnecting
and then carries on by itself.

To try the page without a real update: `DASHBOARD_FAKE_OS=1 dashboard serve --dev` fakes a node
with an update (download, a reboot with the API down for 15 seconds, done), and
`DASHBOARD_FAKE_OS=fail` makes it come back on the old image.

### Main node

```
dashboard enable        # prints the address, e.g. https://192.168.1.10:9090
```

Log in with your Linux username and password. Only users in the `wheel` group can log in (in
the installer, tick "Make this user administrator"). The certificate is self-signed, so the
browser warns the first time; `dashboard status` prints its fingerprint to compare.

### Adding a node

On the other server:

```
dashboard enable node   # prints a one-time pairing password, e.g. K7QM-2XRT-9HVA-PWE4-CN3S
```

In the main node's UI: **Add node**, then its address (IP or hostname, `:port` if not 9090)
and the pairing password. The main node gets an access token for the node and pins the
node's certificate, so it refuses to talk to anything else at that address later. The
password works once; `dashboard pair` on the node prints a new one (pairing again replaces the
old main node). Removing a node in the UI makes it forget the token.

The browser only talks to the main node, which passes requests on to the nodes. A node's
terminal opens as the same username on that node, so that user needs an account in `wheel`
there too.

### Commands

```
dashboard status        # on/off, mode, address, paired or not, certificate fingerprint
dashboard pair          # node: new pairing password
dashboard unpair        # node: forget the main node
dashboard port 9443     # change the port (default 9090)
dashboard nodes         # main: list paired nodes
dashboard disable
```

Settings are in `/etc/dashboard.conf`; the certificate and node list are in
`/var/lib/dashboard`. The image has no firewall, so the port is open to your network once the
dashboard is on. Anyone who logs in gets root-level control (Docker, updates, services), so keep
it on your LAN or behind a VPN, not exposed to the internet.

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
