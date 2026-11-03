package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// All is every tool, in the order clients list them.
var All = []Tool{
	// ---- read ----
	{Name: "list_nodes", Scope: Read,
		Description: "List every node (server) the dashboard manages, with online state, OS version, release channel, CPU/memory use, containers running, and pending OS or container updates. Start here.",
		Run: func(c Call) (any, error) {
			type sum struct {
				Info struct {
					Hostname   string `json:"hostname"`
					PrettyName string `json:"prettyName"`
				} `json:"info"`
				CPUPercent       float64 `json:"cpuPercent"`
				MemTotal         uint64  `json:"memTotal"`
				MemUsed          uint64  `json:"memUsed"`
				UptimeSec        float64 `json:"uptimeSec"`
				Version          string  `json:"version"`
				Channel          string  `json:"channel"`
				UpdateAvailable  bool    `json:"updateAvailable"`
				UpdateVersion    string  `json:"updateVersion"`
				UpdateStaged     bool    `json:"updateStaged"`
				Updating         bool    `json:"updating"`
				AutoUpdate       bool    `json:"autoUpdate"`
				ContainerUpdates int     `json:"containerUpdates"`
				Running          int     `json:"running"`
				Containers       int     `json:"containers"`
			}
			list, err := decode[[]struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Address string `json:"address"`
				Local   bool   `json:"local"`
				Online  bool   `json:"online"`
				Error   string `json:"error"`
				Summary *sum   `json:"summary"`
			}](c.get("/api/nodes"))
			if err != nil {
				return nil, err
			}
			var out []map[string]any
			for _, n := range list {
				m := map[string]any{"id": n.ID, "name": n.Name, "online": n.Online}
				if n.Local {
					m["main"] = true
				} else {
					m["address"] = n.Address
				}
				if n.Error != "" {
					m["error"] = n.Error
				}
				if s := n.Summary; s != nil {
					m["os"] = s.Info.PrettyName
					m["imageVersion"] = s.Version
					m["channel"] = s.Channel
					m["cpuPercent"] = int(s.CPUPercent)
					if s.MemTotal > 0 {
						m["memoryPercent"] = int(float64(s.MemUsed) / float64(s.MemTotal) * 100)
					}
					m["uptimeHours"] = int(s.UptimeSec / 3600)
					m["containersRunning"] = fmt.Sprintf("%d/%d", s.Running, s.Containers)
					m["osUpdateAvailable"] = s.UpdateAvailable
					if s.UpdateAvailable {
						m["osUpdateVersion"] = s.UpdateVersion
					}
					m["osUpdateStaged"] = s.UpdateStaged
					m["osUpdating"] = s.Updating
					m["autoUpdate"] = s.AutoUpdate
					m["containerImageUpdates"] = s.ContainerUpdates
				}
				out = append(out, m)
			}
			return out, nil
		}},
	{Name: "node_overview", Scope: Read, Params: with(nil),
		Description: "Detailed health of one node: CPU and load, memory and swap, every disk's use, network rates, temperatures, uptime, kernel, Docker version.",
		Run: func(c Call) (any, error) {
			d, err := decode[struct {
				Info  map[string]any `json:"info"`
				Stats struct {
					CPUPercent float64          `json:"cpuPercent"`
					Load       []float64        `json:"load"`
					MemTotal   uint64           `json:"memTotal"`
					MemUsed    uint64           `json:"memUsed"`
					SwapTotal  uint64           `json:"swapTotal"`
					SwapUsed   uint64           `json:"swapUsed"`
					UptimeSec  float64          `json:"uptimeSec"`
					Disks      []map[string]any `json:"disks"`
					Net        []map[string]any `json:"net"`
					Temps      []map[string]any `json:"temps"`
				} `json:"stats"`
				Docker      map[string]any `json:"docker"`
				DockerError string         `json:"dockerError"`
			}](c.nodeGet("/overview"))
			if err != nil {
				return nil, err
			}
			disks := []string{}
			for _, x := range d.Stats.Disks {
				total, _ := x["total"].(float64)
				used, _ := x["used"].(float64)
				p := 0.0
				if total > 0 {
					p = used / total * 100
				}
				disks = append(disks, fmt.Sprintf("%v (%v): %s of %s used (%.0f%%)", x["mount"], x["fs"], human(used), human(total), p))
			}
			out := map[string]any{"info": d.Info, "cpuPercent": int(d.Stats.CPUPercent), "load": d.Stats.Load,
				"memory": human(float64(d.Stats.MemUsed)) + " of " + human(float64(d.Stats.MemTotal)),
				"swap":   human(float64(d.Stats.SwapUsed)) + " of " + human(float64(d.Stats.SwapTotal)),
				"uptimeHours": int(d.Stats.UptimeSec / 3600), "disks": disks, "temperatures": d.Stats.Temps}
			if d.Docker != nil {
				out["docker"] = map[string]any{"version": d.Docker["ServerVersion"], "containers": d.Docker["Containers"],
					"running": d.Docker["ContainersRunning"], "images": d.Docker["Images"]}
			} else {
				out["dockerError"] = d.DockerError
			}
			return out, nil
		}},
	{Name: "os_status", Scope: Read, Params: with(nil),
		Description: "The node's OS image: the deployments (running, staged, previous for rollback), whether a newer image is available, and the last OS update run with its result.",
		Run: func(c Call) (any, error) {
			raw, err := c.nodeGet("/os")
			if err != nil {
				return nil, err
			}
			var st map[string]any
			json.Unmarshal(raw, &st)
			delete(st, "bootId")
			return st, nil
		}},
	{Name: "get_channel", Scope: Read, Params: with(nil),
		Description: "The node's release channel (stable, testing, testing-alma), the channels it can switch to, and a switch waiting for a reboot.",
		Run:         func(c Call) (any, error) { return passthrough(c.nodeGet("/os/channel")) }},
	{Name: "list_stacks", Scope: Read, Params: with(nil),
		Description: "The Docker apps (compose stacks) on a node: name, folder, containers running out of total, published ports. Apps in ~/docker can be edited.",
		Run: func(c Call) (any, error) {
			d, err := decode[struct {
				Root   string `json:"root"`
				Stacks []struct {
					Name       string   `json:"name"`
					Dir        string   `json:"dir"`
					InRoot     bool     `json:"inRoot"`
					Running    int      `json:"running"`
					Total      int      `json:"total"`
					Containers []string `json:"containers"`
					Ports      []struct {
						PublicPort int    `json:"PublicPort"`
						Type       string `json:"Type"`
					} `json:"ports"`
				} `json:"stacks"`
			}](c.nodeGet("/docker/stacks"))
			if err != nil {
				return nil, err
			}
			var out []map[string]any
			for _, s := range d.Stacks {
				ports := []string{}
				for _, p := range s.Ports {
					if v := fmt.Sprintf("%d/%s", p.PublicPort, p.Type); p.PublicPort > 0 && !slices.Contains(ports, v) {
						ports = append(ports, v)
					}
				}
				out = append(out, map[string]any{"name": s.Name, "dir": s.Dir, "editable": s.InRoot,
					"running": fmt.Sprintf("%d/%d", s.Running, s.Total), "containers": s.Containers, "ports": ports})
			}
			return map[string]any{"appsFolder": d.Root, "stacks": out}, nil
		}},
	{Name: "get_stack", Scope: Read, Params: with(map[string]any{"stack": str("Stack name from list_stacks.")}), Required: []string{"stack"},
		Description: "The compose.yml of an app in ~/docker, and the names (not values) of its .env variables.",
		Run: func(c Call) (any, error) {
			f, err := decode[struct {
				Compose string `json:"compose"`
				Env     string `json:"env"`
				File    string `json:"file"`
			}](c.nodeGet("/docker/stacks/" + seg(c.str("stack"))))
			if err != nil {
				return nil, err
			}
			return map[string]any{"file": f.File, "compose": f.Compose, "envVariables": EnvNames(f.Env)}, nil
		}},
	{Name: "list_containers", Scope: Read, Params: with(nil),
		Description: "Every container on a node: name, image, state (running, exited, restarting…), status text, its compose project and ports.",
		Run: func(c Call) (any, error) {
			cs, err := decode[[]struct {
				ID     string            `json:"Id"`
				Names  []string          `json:"Names"`
				Image  string            `json:"Image"`
				State  string            `json:"State"`
				Status string            `json:"Status"`
				Labels map[string]string `json:"Labels"`
				Ports  []struct {
					PublicPort int    `json:"PublicPort"`
					Type       string `json:"Type"`
				} `json:"Ports"`
			}](c.nodeGet("/docker/containers"))
			if err != nil {
				return nil, err
			}
			var out []map[string]any
			for _, x := range cs {
				name := x.ID
				if len(x.Names) > 0 {
					name = strings.TrimPrefix(x.Names[0], "/")
				}
				m := map[string]any{"name": name, "image": x.Image, "state": x.State, "status": x.Status}
				if p := x.Labels["com.docker.compose.project"]; p != "" {
					m["stack"] = p
				}
				var ports []string
				for _, p := range x.Ports {
					if v := fmt.Sprintf("%d/%s", p.PublicPort, p.Type); p.PublicPort > 0 && !slices.Contains(ports, v) {
						ports = append(ports, v) // once, not for IPv4 and IPv6
					}
				}
				if ports != nil {
					m["ports"] = ports
				}
				out = append(out, m)
			}
			return out, nil
		}},
	{Name: "container_logs", Scope: Read,
		Params:      with(map[string]any{"container": str("Container name or id."), "tail": integer("Lines from the end, default 150, at most 1000.")}),
		Required:    []string{"container"},
		Description: "The last lines a container wrote (stdout and stderr, with timestamps). Use it to find out why an app fails or restarts.",
		Run: func(c Call) (any, error) {
			tail := min(max(c.num("tail", 150), 1), 1000)
			r, err := decode[struct {
				Logs string `json:"logs"`
			}](c.nodeGet("/docker/containers/" + seg(c.str("container")) + "/logs?tail=" + strconv.Itoa(tail)))
			if err != nil {
				return nil, err
			}
			return r.Logs, nil
		}},
	{Name: "container_updates", Scope: Read, Params: with(nil),
		Description: "Which container images on a node have a newer version in their registry, which containers and stacks use them, and the running or last update job.",
		Run:         func(c Call) (any, error) { return passthrough(c.nodeGet("/docker/updates")) }},
	{Name: "journal_logs", Scope: Read,
		Params: with(map[string]any{
			"unit":     str(`A systemd unit, e.g. "docker.service", "netbird.service", "autoupdate.service", "dashboard.service". Empty for the whole journal.`),
			"kernel":   boolean("Kernel messages only."),
			"priority": enum("Show this priority and more important.", "err", "warning", "notice", "info", "debug"),
			"since":    enum("How far back.", "15m", "1h", "24h", "7d", "boot"),
			"grep":     str("Only lines containing this text (case-insensitive)."),
			"limit":    integer("Most recent lines to return, default 200, at most 1000."),
		}),
		Description: "Read the systemd journal of a node: system services, the kernel, or one unit, filtered by priority, time and text.",
		Run: func(c Call) (any, error) {
			q := url.Values{}
			if c.boolean("kernel") {
				q.Set("kernel", "1")
			} else if u := c.str("unit"); u != "" {
				q.Set("unit", u)
			}
			q.Set("priority", strconv.Itoa(map[string]int{"err": 3, "warning": 4, "notice": 5, "info": 6, "debug": 7}[c.str("priority")]))
			if c.str("priority") == "" {
				q.Set("priority", "7")
			}
			if s := c.str("since"); s != "" {
				q.Set("since", s)
			}
			if g := c.str("grep"); g != "" {
				q.Set("grep", g)
			}
			q.Set("limit", strconv.Itoa(min(max(c.num("limit", 200), 1), 1000)))
			r, err := decode[struct {
				Entries []struct {
					Time     int64  `json:"time"`
					Unit     string `json:"unit"`
					Ident    string `json:"ident"`
					Priority int    `json:"priority"`
					Message  string `json:"message"`
				} `json:"entries"`
			}](c.nodeGet("/logs?" + q.Encode()))
			if err != nil {
				return nil, err
			}
			var sb strings.Builder
			for _, e := range r.Entries {
				src := e.Ident
				if src == "" {
					src = e.Unit
				}
				fmt.Fprintf(&sb, "%s %s %s: %s\n", time.UnixMilli(e.Time).UTC().Format("2006-01-02 15:04:05"), prioName(e.Priority), src, e.Message)
			}
			if sb.Len() == 0 {
				return "No log entries match.", nil
			}
			return sb.String(), nil
		}},
	{Name: "list_services", Scope: Read, Params: with(nil),
		Description: "The node's switchable services (SSH, smartd, Docker, …) with enabled/active state, and its automatic OS update schedule (next and last run, Gotify on or off).",
		Run: func(c Call) (any, error) {
			raw, err := c.nodeGet("/features")
			if err != nil {
				return nil, err
			}
			var f map[string]any
			json.Unmarshal(raw, &f)
			if a, ok := f["autoupdate"].(map[string]any); ok {
				for _, k := range []string{"nextRun", "lastRun"} {
					if v, ok := a[k].(float64); ok {
						a[k] = ago(int64(v))
					}
				}
			}
			return f, nil
		}},
	{Name: "netbird_status", Scope: Read, Params: with(nil),
		Description: "The node's NetBird VPN: service on/off, connected or not, its NetBird IP and name, and the peers with their connection type and latency.",
		Run:         func(c Call) (any, error) { return passthrough(c.nodeGet("/netbird")) }},
	{Name: "list_users", Scope: Read, Params: with(nil),
		Description: "The regular Linux users on a node: wheel (admin) and docker group membership, login shell, locked or not.",
		Run:         func(c Call) (any, error) { return passthrough(c.nodeGet("/users")) }},
	{Name: "list_backups", Scope: Read, Params: with(nil),
		Description: "App backups on a node: the schedule, which apps are included, the archives kept, and the last backup run.",
		Run:         func(c Call) (any, error) { return passthrough(c.nodeGet("/docker/backups")) }},
	{Name: "list_mounts", Scope: Read, Params: with(nil),
		Description: "NFS and SMB shares mounted on a node: server and share, mount point (/var/mnt/<name>), state (mounted, waiting for first access, failed, off), mounted at boot or not, whether Docker waits for it at boot, and space used. Never includes passwords.",
		Run:         func(c Call) (any, error) { return passthrough(c.nodeGet("/storage/mounts")) }},
	{Name: "rollout_status", Scope: Read,
		Description: "The multi-node OS update the main node runs or ran last: each node's step (downloading, rebooting, done, failed).",
		Run:         func(c Call) (any, error) { return passthrough(c.get("/api/rollout")) }},

	// ---- operate ----
	{Name: "container_action", Scope: Operate,
		Params:   with(map[string]any{"container": str("Container name or id."), "action": enum("What to do.", "start", "stop", "restart")}),
		Required: []string{"container", "action"},
		Description: "Start, stop or restart one container.",
		Run: func(c Call) (any, error) {
			return output(c.nodePost("/docker/containers/"+seg(c.str("container"))+"/"+c.str("action"), nil))
		}},
	{Name: "stack_action", Scope: Operate,
		Params:   with(map[string]any{"stack": str("Stack name from list_stacks."), "action": enum("start: compose up -d · stop · restart · recreate: pull newer images and recreate the containers (data in volumes is kept).", "start", "stop", "restart", "recreate")}),
		Required: []string{"stack", "action"},
		Description: "Start, stop, restart or pull & recreate a whole app (compose stack).",
		Run: func(c Call) (any, error) {
			return output(c.nodePost("/docker/stacks/"+seg(c.str("stack"))+"/"+c.str("action"), nil))
		}},
	{Name: "update_containers", Scope: Operate,
		Params: with(map[string]any{"stack": str("Update only this stack. Leave out stack and image to update every image that has an update."),
			"image": str("Update only this image (as container_updates names it).")}),
		Description: "Pull newer images and recreate only what uses them, in the background (one job at a time per node). Follow it with container_updates.",
		Run: func(c Call) (any, error) {
			switch {
			case c.str("stack") != "":
				return output(c.nodePost("/docker/stacks/"+seg(c.str("stack"))+"/update", nil))
			case c.str("image") != "":
				return output(c.nodePost("/docker/updates/apply", map[string]string{"image": c.str("image")}))
			}
			return output(c.nodePost("/docker/updates/apply-all", nil))
		}},
	{Name: "check_updates", Scope: Operate,
		Params:      with(map[string]any{"what": enum("os: look for a newer OS image · containers: ask the registries for newer images.", "os", "containers")}),
		Required:    []string{"what"},
		Description: "Start a fresh update check on a node. It runs in the background; read os_status or container_updates a little later.",
		Run: func(c Call) (any, error) {
			if c.str("what") == "os" {
				return output(c.nodePost("/os/check", nil))
			}
			return output(c.nodePost("/docker/updates/check", nil))
		}},
	{Name: "run_backup", Scope: Operate,
		Params:      with(map[string]any{"apps": strList("Apps to back up. Leave out for the apps the backup schedule includes.")}),
		Description: "Back up apps now (stops each one briefly, archives its folder, starts it again).",
		Run: func(c Call) (any, error) {
			return output(c.nodePost("/docker/backups/run", map[string]any{"apps": c.list("apps")}))
		}},

	// ---- admin ----
	{Name: "start_os_update", Scope: Admin,
		Params:      map[string]any{"nodes": strList(`Node ids or names to update, e.g. ["local"].`)},
		Required:    []string{"nodes"},
		Description: "Install the newer OS image on nodes: all download at once, then reboot one at a time (the main node last), each checked on the new image before the next. Follow it with rollout_status.",
		Run: func(c Call) (any, error) {
			var ids []string
			for _, n := range c.list("nodes") {
				cc := c
				cc.Args = map[string]any{"node": n}
				base, err := cc.node()
				if err != nil {
					return nil, err
				}
				ids = append(ids, strings.TrimPrefix(base, "/api/n/"))
			}
			if len(ids) == 0 {
				return nil, errors.New("name at least one node")
			}
			return passthrough(c.post("/api/rollout", map[string]any{"nodes": ids}))
		}},
	{Name: "reboot_node", Scope: Admin, Params: with(nil), Required: []string{"node"},
		Description: "Reboot a node now. Its containers restart with it.",
		Run:         func(c Call) (any, error) { return output(c.nodePost("/power/reboot", nil)) }},
	{Name: "switch_channel", Scope: Admin,
		Params:      with(map[string]any{"channel": enum("Channel to follow.", "stable", "testing", "testing-alma")}),
		Required:    []string{"node", "channel"},
		Description: "Make a node follow another release channel. It downloads that image now and runs it after the next reboot; data and containers stay.",
		Run: func(c Call) (any, error) {
			return output(c.nodePost("/os/channel", map[string]string{"channel": c.str("channel")}))
		}},
	{Name: "set_autoupdate", Scope: Admin,
		Params: with(map[string]any{
			"off":     boolean("Turn automatic OS updates off."),
			"freq":    enum("How often.", "daily", "weekly", "monthly"),
			"weekday": enum("For weekly.", "mon", "tue", "wed", "thu", "fri", "sat", "sun"),
			"day":     integer("Day of month 1-31, for monthly."),
			"time":    str("24-hour HH:MM."),
		}),
		Description: "Set or turn off the node's automatic OS update schedule.",
		Run: func(c Call) (any, error) {
			if c.boolean("off") {
				return output(c.nodePost("/features/autoupdate", map[string]any{"off": true}))
			}
			return output(c.nodePost("/features/autoupdate", map[string]any{"freq": c.str("freq"), "weekday": c.str("weekday"),
				"day": strconv.Itoa(c.num("day", 1)), "time": c.str("time")}))
		}},
	{Name: "save_stack", Scope: Admin,
		Params: with(map[string]any{
			"stack":   str("App name (lowercase letters, digits, - and _)."),
			"compose": str("The full compose.yml."),
			"create":  boolean("A new app in ~/docker/<stack>/ (otherwise an existing app is changed; its .env is kept)."),
			"start":   boolean("Start it (new app) or pull & recreate it (existing app) after saving."),
		}),
		Required:    []string{"stack", "compose"},
		Description: "Write an app's compose.yml. It's checked with docker compose config first; the old file is kept as compose.yml.bak.",
		Run: func(c Call) (any, error) {
			name := c.str("stack")
			body := map[string]any{"compose": c.Args["compose"], "start": c.boolean("start")}
			if c.boolean("create") {
				body["name"] = name
				return output(c.nodePost("/docker/stacks", body))
			}
			cur, err := decode[struct {
				Env string `json:"env"`
			}](c.nodeGet("/docker/stacks/" + seg(name)))
			if err != nil {
				return nil, err
			}
			body["env"] = cur.Env // never seen by the model, written back unchanged
			return output(c.nodePost("/docker/stacks/"+seg(name), body))
		}},
	{Name: "remove_stack", Scope: Admin,
		Params:      with(map[string]any{"stack": str("Stack name."), "deleteFolder": boolean("Also delete its folder in ~/docker (named volumes are kept either way).")}),
		Required:    []string{"stack"},
		Description: "Stop and remove an app's containers (docker compose down).",
		Run: func(c Call) (any, error) {
			return output(c.nodePost("/docker/stacks/"+seg(c.str("stack"))+"/remove", map[string]any{"deleteFolder": c.boolean("deleteFolder")}))
		}},
	{Name: "set_service", Scope: Admin,
		Params:      with(map[string]any{"service": enum("Service id.", "ssh", "smartd", "docker-group", "fish-default-shell", "qemu-guest-agent", "netbird", "docker"), "enabled": boolean("On (enable and start) or off (disable and stop).")}),
		Required:    []string{"service"},
		Description: "Turn one of the node's services on or off.",
		Run: func(c Call) (any, error) {
			id := c.str("service")
			if id == "netbird" {
				return output(c.nodePost("/netbird/service", map[string]any{"enabled": c.boolean("enabled")}))
			}
			return output(c.nodePost("/features/services/"+seg(id), map[string]any{"enabled": c.boolean("enabled")}))
		}},
	{Name: "mount_action", Scope: Admin,
		Params:      with(map[string]any{"mount": str("Mount name from list_mounts."), "action": enum("mount · unmount (fails while containers use it).", "mount", "unmount")}),
		Required:    []string{"mount", "action"},
		Description: "Mount or unmount one of a node's NFS/SMB shares. Adding, changing and removing mounts is only possible in the dashboard.",
		Run: func(c Call) (any, error) {
			return output(c.nodePost("/storage/mounts/"+seg(c.str("mount"))+"/"+c.str("action"), nil))
		}},
	{Name: "prune_docker", Scope: Admin,
		Params:      with(map[string]any{"what": enum("images: unused images · containers: stopped containers · networks: unused networks.", "images", "containers", "networks")}),
		Required:    []string{"what"},
		Description: "Free disk space by removing unused Docker images, stopped containers or unused networks. Volumes are never pruned from here.",
		Run:         func(c Call) (any, error) { return output(c.nodePost("/docker/prune/"+c.str("what"), nil)) }},
	{Name: "set_user_group", Scope: Admin,
		Params:      with(map[string]any{"user": str("Username."), "group": enum("wheel: admin (sudo, dashboard login) · docker: docker without sudo.", "wheel", "docker"), "member": boolean("Add (true) or remove (false).")}),
		Required:    []string{"user", "group"},
		Description: "Add a user to or remove them from wheel or docker.",
		Run: func(c Call) (any, error) {
			return output(c.nodePost("/users/"+seg(c.str("user"))+"/group", map[string]any{"group": c.str("group"), "member": c.boolean("member")}))
		}},
}

func passthrough(raw json.RawMessage, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(raw, &v)
}

func prioName(p int) string {
	names := []string{"EMERG", "ALERT", "CRIT", "ERR", "WARN", "NOTICE", "INFO", "DEBUG"}
	if p < 0 || p >= len(names) {
		return "INFO"
	}
	return names[p]
}

func human(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return strconv.FormatFloat(n, 'f', 1, 64) + " " + units[i]
}

var envLineRe = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// EnvNames lists the variables a .env sets, never their values.
func EnvNames(env string) []string {
	names := []string{}
	for _, line := range strings.Split(env, "\n") {
		if m := envLineRe.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}
