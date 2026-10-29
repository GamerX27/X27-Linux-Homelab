# X27-Linux Homelab

> **`testing` branch: Fedora 45.** Builds `x27-linux-homelab-testing` and
> `x27-linux-homelab-vm-testing` (`:45`). Everything below applies; the install commands
> set up `stable`, then `channel testing` moves a machine here. May break.

A minimal, headless server OS for running Docker. The OS is a small read-only image that
updates itself; everything you run on it goes in containers.

Built with [BlueBuild](https://blue-build.org/) on Fedora's official
[bootc image](https://quay.io/repository/fedora/fedora-bootc), in two flavors:

| Flavor | Image | Recipe |
|---|---|---|
| Bare metal | `ghcr.io/gamerx27/x27-linux-homelab` | [recipe.yml](recipes/recipe.yml) |
| VM | `ghcr.io/gamerx27/x27-linux-homelab-vm` | [recipe-vm.yml](recipes/recipe-vm.yml) |

Both share [common.yml](recipes/common.yml).

**Contents:** [Install](#install) · [Channels](#channels) · [OS updates](#os-updates) ·
[Container updates](#container-updates) · [Dashboard](#dashboard) · [Integrations](#integrations) · [NetBird](#netbird) ·
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

Automatic partitioning uses ext4 on LVM. Custom partitioning still lets you pick something
else. Tick **Make this user administrator** for your user: you need it to log in to the
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

The dashboard does the same on a node's **Updates** page, or for several nodes at once from
**All nodes → Switch channel…**. The switch applies after the reboot. Your data, containers and
settings stay; only the OS changes. To undo it, roll back (`sudo rpm-ostree rollback` or **Roll back** in the dashboard)
before the next update.

<details>
<summary>Image names and tags</summary>

All images are at `ghcr.io/gamerx27/`, signed, and rebuilt every Sunday at 02:00 UTC.

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

**For all nodes (main page):** see every node with its version and channel, update the OS on
several nodes at once, switch their channel, and update containers on all nodes.

**For each node:**

| Page | What's on it |
|---|---|
| Overview | CPU, memory, disks, network, temperatures, uptime, OS version |
| Updates | Current OS image, available update, last update and its log; update, reboot or roll back. The release channel, and switching it |
| Docker | Your apps (compose stacks) and their containers: start, stop, update, edit, logs, remove. Plus images, volumes and networks for cleanup |
| Files | Your home folder: browse, edit, upload, download, rename, delete. Same permissions as over SSH |
| Storage | NFS and SMB shares from a NAS, mounted under `/var/mnt` for your containers: add, test, mount, unmount, remove |
| Terminal | A shell in the browser, as your user |
| Logs | The system journal, the kernel, or one service; filter by priority, time and text, follow it live, download it |
| Users | Regular users: wheel and docker membership, login shell, password, lock, add and delete |
| Features | Automatic updates, Gotify messages, NetBird (connect, peers, disconnect), and turning services (SSH, smartd, Docker…) on or off |
| Settings | Hostname, time zone, network time, reboot and shut down. IPv4 per connection (DHCP or static, gateway, DNS), undone after 2 minutes unless kept. The dashboard's port, addresses and certificate fingerprint |

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

### Network shares (Storage)

The **Storage** page mounts NFS exports and SMB (Samba, Windows) shares on a node, for
containers that keep their data on a NAS. A share named `nas-media` is mounted at
`/var/mnt/nas-media`; give it to a container as a bind mount:

```yaml
services:
  jellyfin:
    volumes:
      - /var/mnt/nas-media:/media
```

- **Add share:** the server, the export path (NFS, with **Browse** to list them) or the share
  name (SMB), and for SMB the username and password. **Test** mounts it once and lists what's
  there; **Add and mount** does the same test first and saves nothing if it fails.
- **More options:** protocol version, read-only, **mount on first access** instead of at boot,
  and for SMB the user who owns the files on the node and their permissions (SMB has no Linux
  owners, so all files show up as that user).
- **Docker waits for this share at boot** (on by default): Docker starts after the share is
  mounted, so containers don't write into the empty folder underneath. If the server is down,
  Docker starts anyway once the mount gives up (up to 45 seconds). Applies from the next boot.
- A server that's down never stops the node from booting: that mount fails on its own, and the
  page shows **Failed** with the reason under **⋯ → Logs**.
- **Remove** unmounts it and deletes the mount and its stored password. The files on the server
  aren't touched. Unmounting fails while containers use the share: stop them first.

<details>
<summary>Where things are stored</summary>

- Each share is a systemd unit in `/etc/systemd/system/`, like `var-mnt-nas\x2dmedia.mount`
  (plus a `.automount` for mount on first access). They start with `# Managed by dashboard`;
  the page lists only those, and never touches `/etc/fstab` or units you wrote yourself.
- SMB passwords: `/etc/dashboard/smb/<name>.cred` (root only). The dashboard never shows them
  again, and AI clients can't see them.
- Docker's wait list: `/etc/systemd/system/docker.service.d/dashboard-mounts.conf`.
- By hand: `systemctl status 'var-mnt-nas\x2dmedia.mount'`, `findmnt /var/mnt/nas-media`.

</details>

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

`status` and `port` are also on each node's **Settings** page, and a paired node's port under
**⋯ → Change port…** on the main page (the main node then follows it).

Settings are in `/etc/dashboard.conf`; certificate, nodes, sessions and API tokens in
`/var/lib/dashboard`.

---

## Integrations

The dashboard's **Integrations** page connects other programs to your homelab.

### AI clients (MCP)

AI programs that speak the [Model Context Protocol](https://modelcontextprotocol.io) (Claude
Code, Claude Desktop, LM Studio, Cursor…) connect to `https://<main node>:9090/mcp`. They can
look at every node: health, apps and containers, their logs, the journal, updates, channel,
services, NetBird, users, backups and network shares. Depending on the token's access, they can also start,
stop and update apps, and install OS updates or reboot.

There are no tools for files, the terminal or secrets: Gotify tokens, `.env` values and
passwords never reach a model. Changes go through the same checks as the dashboard, and the
journal (`journalctl -u dashboard`) logs each one with the token's name.

| Access | Can |
|---|---|
| read | Look only: status, logs, updates, compose files |
| operate | Also start, stop, restart and update apps and containers, run backups |
| admin | Also OS updates, reboots, channel, services, users' groups, editing and removing apps, mounting and unmounting network shares |


Under **Integrations**, create a token (it's shown once). The page then shows every command
below with the server's name, address and token filled in.

**Claude Code** connects over HTTP. The dashboard's certificate is self-signed, so Claude Code
has to trust it first:

1. Download the certificate from the Integrations page (or `https://<main>:9090/ca.pem`), check
   its fingerprint against Settings → Dashboard, and keep it somewhere permanent:
   ```sh
   mkdir -p ~/.config/homelab
   mv ~/Downloads/homelab-<host>.pem ~/.config/homelab/
   ```
2. Trust it in every terminal. `NODE_EXTRA_CA_CERTS` is read when Claude Code starts and holds
   one file; for several dashboards, `cat` their certificates into one.
   ```sh
   echo 'export NODE_EXTRA_CA_CERTS=~/.config/homelab/homelab-<host>.pem' >> ~/.bashrc  # bash/zsh
   set -Ux NODE_EXTRA_CA_CERTS ~/.config/homelab/homelab-<host>.pem                       # fish
   ```
3. Add the server for every folder (`--scope user`; without it, it only exists in the folder you
   ran the command in):
   ```sh
   claude mcp add --scope user --transport http homelab-<host> https://192.168.1.10:9090/mcp \
     --header "Authorization: Bearer x27hl_…"
   ```
4. Start Claude Code from a new terminal and check `/mcp`. `UNABLE_TO_VERIFY_LEAF_SIGNATURE`
   means the certificate isn't trusted yet (step 2); `401` means the token is wrong or cut off
   (remove the server and add it again).

**LM Studio, Claude Desktop, Cursor** and other desktop apps are started from the menu, where
`NODE_EXTRA_CA_CERTS` isn't set, and a remote `url` entry has nowhere to put it. Run the server
through the [`mcp-remote`](https://www.npmjs.com/package/mcp-remote) bridge instead, which needs
Node.js on that computer. The PEM path must be absolute (`~` isn't expanded in `env`):

```json
{
  "mcpServers": {
    "homelab-<host>": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "https://192.168.1.10:9090/mcp", "--header", "Authorization:${AUTH_HEADER}"],
      "env": {
        "AUTH_HEADER": "Bearer x27hl_…",
        "NODE_EXTRA_CA_CERTS": "/home/you/.config/homelab/homelab-<host>.pem"
      }
    }
  }
}
```

In LM Studio that's Program → Install → Edit `mcp.json`. The alternative is to put the dashboard
behind your reverse proxy with a trusted certificate and use the plain URL + `Authorization`
header config.

A token acts as the user who made it, who has to stay in `wheel`; revoke it on the same page. The
server also offers prompts: `health_check`, `troubleshoot_app` and `plan_updates`.

> **Security:** an admin token is root on every node. Keep the dashboard on your LAN or VPN, give
> clients the least access they need, and set an expiry.

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
`sudo systemctl disable --now netbird` turns it off. The dashboard's **Features** page does all
of this too: turn it on, connect with a setup key or a browser login, see the peers, disconnect.

Settings go in `/etc/sysconfig/netbird`, state is in `/var/lib/netbird`, logs with
`journalctl -u netbird`.

---

## What's included

| | |
|---|---|
| **Base** | Fedora bootc, no desktop. SSH on. Podman removed. |
| **Docker** | Docker CE from Docker's repo, with buildx and the compose plugin. On at boot. |
| **Commands** | `channel`, `autoupdate`, `docker-compose-update`, `dashboard` (off), `netbird` (off) |
| **Tools** | fish, `htop`, `git`, `wget`, `lspci`, `ncdu`, `zip`, `unzip`, NFS and SMB clients (`nfs-utils`, `cifs-utils`) |
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

- `vYYYY.MM.DD`: the weekly rebuild
- `vYYYY.MM.DD.N`: builds from changes pushed in between

### Signing

Images are signed with cosign ([cosign.pub](cosign.pub)). The repo needs the private key as
the `SIGNING_SECRET` Actions secret.

### Trying the OS update page

`DASHBOARD_FAKE_OS=1 dashboard serve --dev` fakes a node with an update (download, a 15-second
reboot, done). `DASHBOARD_FAKE_OS=fail` makes it come back on the old image.

`DASHBOARD_FAKE_STORAGE=1 dashboard serve --dev` keeps Storage mounts in memory, so the page
works without root or a NAS. A test against the server `fail.example` fails.

`DASHBOARD_FAKE_NETWORK=1 dashboard serve --dev` fakes one wired connection for the Network
card on Settings, with the same 2-minute revert.
