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
sudo rpm-ostree rebase ostree-unverified-registry:ghcr.io/gamerx27/x27-linux-homelab:45
systemctl reboot

sudo rpm-ostree rebase ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:45
systemctl reboot
```

For a VM, use `x27-linux-homelab-vm` instead.

---

## Channels

Each machine follows one channel and only gets updates from it.

| Channel | Base | Use it for |
|---|---|---|
| `stable` | Fedora 45 | Everyday use. The default. |
| `testing` | Fedora 45 | Trying changes before they reach `stable`, and the next Fedora once it moves on. May break. |
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

**Fedora 44 is end of life.** `stable` moved to Fedora 45, and the `:44` images get no more
updates. A machine still on 44 says so in `channel` and on its **Updates** page (and gets an
**end of life** badge on **All nodes**). Move it with `channel stable` or **Move to Fedora 45**,
then reboot.

<details>
<summary>Image names and tags</summary>

All images are at `ghcr.io/gamerx27/`, signed, and rebuilt every Sunday at 02:00 UTC.

| Channel | Bare metal | VM | Branch |
|---|---|---|---|
| `stable` | `x27-linux-homelab:45` | `x27-linux-homelab-vm:45` | `main` |
| `testing` | `x27-linux-homelab-testing:45` | `x27-linux-homelab-vm-testing:45` | `testing` |
| `testing-alma` | `x27-linux-homelab-alma-testing:10` | `x27-linux-homelab-vm-alma-testing:10` | `testing-alma` |

Every image has three tags (shown for `stable`):

- `:45` follows the channel. This is what `channel` uses.
- `:<date>-45` (e.g. `:20261108-45`) is one fixed build, for pinning or rolling back.
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

Nothing is saved if the test message doesn't arrive. The dashboard's **System → Features** page can do
the same.

<details>
<summary>Example update message</summary>

```
X27-Linux Homelab 45 (2026-11-08) → X27-Linux Homelab 45 (2026-11-15)
Image: ghcr.io/gamerx27/x27-linux-homelab:45
Digest: sha256:5f2e…
Packages: 12 upgraded, 1 removed, 1 added

Upgraded (12):
  kernel 6.17.3-300.fc45.x86_64 -> 6.17.5-300.fc45.x86_64
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

**Cluster (sidebar):** settings for all nodes together: a [virtual IP](#cluster) with two or
more member nodes, [shares mounted on every node](#cluster-shares), and
[high-availability apps](#high-availability-apps) that run wherever the virtual IP is.

**For each node:**

| Tab | What's on it |
|---|---|
| Overview | CPU, memory, disks, network, temperatures, uptime, OS version |
| Updates | Current OS image, available update, last update and its log; update, reboot or roll back. The release channel, and switching it |
| Docker | Your apps (compose stacks) and their containers: start, stop, update, edit, remove, and move to another node (see [Moving an app](#moving-an-app-to-another-node)). Logs per container or for a whole stack merged by time: search, follow, stdout/stderr, errors and warnings highlighted, download. Plus images, volumes and networks for cleanup |
| Files | **Files:** your home folder: browse, edit, upload, download, rename, delete, with the same permissions as over SSH; compress to zip, tar.gz or tar.zst and extract (see what's inside first, then pick where it goes and what happens to files that already exist), in the background with progress and Cancel. **Network shares:** NFS and SMB shares from a NAS, mounted under `/var/mnt` for your containers: add, test, mount, unmount, remove |
| Backup | **Backups:** apps archived on a schedule or now, kept per app. **Export & import:** every app's config plus the server's own settings in one file, and restoring it here or on a new server |
| Terminal | A shell in the browser, as your user |
| System | **Settings:** hostname, time zone, network time, reboot and shut down; IPv4 per connection (DHCP or static, gateway, DNS), undone after 2 minutes unless kept; the dashboard's port, addresses and certificate fingerprint; the virtual IP is under [Cluster](#cluster). **Users:** wheel and docker membership, login shell, password, lock, add and delete. **Features:** automatic updates, Gotify messages, NetBird, and turning services (SSH, smartd, Docker…) on or off. **Logs:** the system journal, the kernel, or one service; filter, follow live, download. **Activity:** who changed what and when, on this node or every node: dashboard actions, API token (MCP) actions, logins and pairings, and whether each worked |

Each sub-tab has its own address, like `#/n/<node>/system/logs`, so a reload stays on it.

### Cluster

The **Cluster** page in the sidebar holds settings that belong to no single node. It's on the
main node (a member standing in for it shows it too, read-only).

#### Virtual IP

One extra address on your LAN that reaches the dashboard, and your
[HA apps](#high-availability-apps), whichever node is up. Pick a free address in the same
subnet as the members, and **two or more member nodes** in priority order. The first member
that's up holds the address; if it, or its dashboard, stops, the next one takes it within a few
seconds (VRRP through keepalived, unicast between the members).

- The main node doesn't have to be a member. Then the address lives only on other nodes, and
  whichever holds it shows the full dashboard there.
- A member that holds the address while it isn't the main node *stands in*: it serves the
  dashboard with the main node's certificate and the paired nodes, API tokens and cluster
  settings the main node last copied to it (every minute, to every member). Changes made there
  aren't kept. Logins are checked on that node, so your user needs an account there too.
- **Move back to the first member by itself** (failback) is off by default once there are HA
  apps: moving back on its own would restart a replicated app on an out-of-date copy. Use
  **Move virtual IP here** on a member instead: the HA apps stop where they run, replicated
  ones send a final copy, every other member gives the address up, and the apps start on the
  new holder.
- Members must be paired by their IPv4 address. Setting the address up makes the main node's
  certificate again so it includes it; browsers and MCP clients see a new fingerprint once.
- A virtual IP set up before the Cluster page (main node + standby) becomes a cluster virtual
  IP with those two members when the main node starts.

#### Cluster shares

**Shared storage** is one NFS or SMB share mounted at the same `/var/mnt/<name>` on **every
node** (including nodes paired later) or on the nodes you pick. The main node adds it to each
node's [Network shares](#network-shares), where it shows as **Managed by Cluster** and can only
be changed here. Each node tests the share before mounting it; the page shows the result per
node, and a node that was off gets it when it's back (checked every 5 minutes and when a node
is paired). **Docker waits for this share at boot** is on by default.

Because the path is the same everywhere, a compose file works on any node:

```yaml
services:
  jellyfin:
    image: jellyfin/jellyfin
    volumes:
      - ./config:/config              # local, or replicated if it's an HA app
      - /var/mnt/media:/media:ro      # cluster share "media", mounted on every node
    ports: ["8096:8096"]
```

Moving Jellyfin, or failing it over, only carries `./config`; the media stays on the NAS.
Moving an app to a node that doesn't have a `/var/mnt/<name>` it uses is refused. The stack
editor's **Insert share path** adds the line for a share.

#### High-availability apps

An HA app runs on whichever member holds the virtual IP. If that node goes offline, the next
member starts it. Make one from the Cluster page or a stack's **⋯ → Make highly available…**
(on the node that holds the virtual IP). Where its data is, per app (shared, replicated or
synced):

- **Shared:** all its data is on cluster shares mounted on every member (bind mounts under
  `/var/mnt/<share>`, or NFS volumes). Failover is instant. The dashboard refuses this mode and
  lists what's on a node's own disk otherwise. A lock file on the share
  (`/var/mnt/<share>/.x27-ha/<app>.lock`, refreshed every 10 s) keeps two nodes from running it
  at once when they can't see each other: a node waits until another node's lock is 30 s old.
- **Replicated:** its folder and named volumes are copied from the node running it to the other
  members every interval (default 15 minutes), paused for a moment while the copy is taken. A
  failover can lose up to one interval of changes. A node taking over first asks the others
  whether one ran it after its copy, and gets the final copy from there if so; if nobody
  answers, it starts from its own copy and says how old it is.

- **Synced:** like replicated, but only the files that changed travel, every 10 seconds to an
  hour (default 30 seconds). The node running the app sends its file list, each member says
  which files it lacks or has different, and only those are sent, with the list so deleted files
  go too. Changed files land through a rename, so a member never has half a file. Small changes
  are sent with the app paused for a moment; the first sync sends everything while it runs. A
  failover can lose up to one interval; a node taking over first gets the last changes from the
  node that ran it, if it answers. This suits apps with more data than you'd copy whole, like
  Jellyfin's `config` (keep the media itself on a cluster share).

**Ports only on the virtual IP** publishes the app's ports on that address only, so it isn't
reachable on the nodes' own addresses. Nothing is blocked by a firewall; the ports just aren't
published there.

The dashboard never edits your `compose.yml`. It writes `compose.x27-ha.yml` next to it
(`restart: on-failure`, so Docker doesn't start the app at boot on a node without the virtual
IP, and the ports on the virtual IP) and a `.x27-ha` marker, and copies `compose.yml` and
`.env` to every member, again every 5 minutes after you edit it. On a node without the virtual
IP the stack shows **HA · standby copy**, Start is refused, and `docker-compose-update` skips
it. HA apps need Docker Compose 2.24 or newer.

### Moving an app to another node

On **Docker**, a stack's **⋯ → Move to another node…** moves it with its folder
(`~/docker/<app>`) and the named Docker volumes it uses. The dialog first checks both nodes:
how much it copies, that there's room, that the folder, volumes and ports are free on the new
node, and what won't come along (bind mounts outside the app's folder, unnamed volumes, volumes
of other drivers).

The app is stopped while it's copied. The main node streams it straight from one node to the
other, without storing it. Then the app is started on the new node, and its containers are
checked to see that they keep running. If it doesn't start there, the copy on the new node is
removed and the app is started again where it was. The old copy is left stopped and marked
**moved to …**. Remove it once the new one works, or tick **Delete it … once it runs** to have
it removed for you. A move in progress is lost if the main node's dashboard restarts.

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

### Export & import

**Backup → Export & import** puts a whole server in one `.tar.gz`:

- `docker/`: every app in `~/docker` with the files at the top of its folder (`compose.yml`,
  `.env` and so on, up to 1 MiB each). Tick apps to add their data (the whole folder) too;
  running ones are stopped while it's copied, then started again. Named volumes aren't included.
- `server.json`, with the parts you tick: **dashboard settings** (port, compose presets, the
  backup schedule, and on the main node the paired nodes and API tokens), **network shares**
  (with their SMB passwords), **updates & Gotify** (the schedule, the Gotify server and token,
  which services are on), and **network & host** (hostname, time zone, NTP, IPv4 per connection).
- `README.txt`: what's inside and how to restore it by hand.

**The file holds secrets:** the apps' `.env` files, SMB passwords, the Gotify token and the
paired nodes' tokens. Keep it somewhere safe.

**Import** takes such a file and shows what's in it before changing anything. Pick the apps
(one that's already in `~/docker` is skipped unless you tick **Replace**, which writes the
exported files over it and keeps the rest), whether to start them, and which server settings
to apply. Network & host is off by default; an IPv4 change from it is undone after 2 minutes
unless you keep it under System → Settings. The import runs in the background with a log, like
a backup. Files can't be written outside `~/docker`, symlinks are recreated as they were, and
files the exporting user owned become yours.

To restore the apps by hand:

```sh
tar -xzf <host>-export-<date>.tar.gz -C ~ docker/
cd ~/docker/<app> && docker compose up -d
```

### Network shares

**Files → Network shares** mounts NFS exports and SMB (Samba, Windows) shares on a node, for
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
- To mount the same share on several nodes, add it as a [cluster share](#cluster-shares).

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

`status` and `port` are also on each node's **System → Settings** page, and a paired node's port under
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
passwords never reach a model. Changes go through the same checks as the dashboard, and
**System → Activity** (and the journal, `journalctl -u dashboard`) records each one with the
token's name.

| Access | Can |
|---|---|
| read | Look only: status, logs, updates, compose files |
| operate | Also start, stop, restart and update apps and containers, run backups |
| admin | Also OS updates, reboots, channel, services, users' groups, editing, removing and moving apps between nodes, mounting and unmounting network shares |


Under **Integrations**, create a token (it's shown once). The page then shows every command
below with the server's name, address and token filled in.

**Claude Code** connects over HTTP. The dashboard's certificate is self-signed, so Claude Code
has to trust it first:

1. Download the certificate from the Integrations page (or `https://<main>:9090/ca.pem`), check
   its fingerprint against System → Settings → Dashboard, and keep it somewhere permanent:
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
`sudo systemctl disable --now netbird` turns it off. The dashboard's **System → Features** page does all
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

`/etc/os-release` names the build, e.g. `X27-Linux Homelab 45 (2026-11-08)`.

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

`DASHBOARD_FAKE_STORAGE=1 dashboard serve --dev` keeps network share mounts in memory, so the page
works without root or a NAS. A test against the server `fail.example` fails.

`DASHBOARD_FAKE_NETWORK=1 dashboard serve --dev` fakes one wired connection for the Network
card on System → Settings, with the same 2-minute revert.
