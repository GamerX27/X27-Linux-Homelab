package cluster

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// systemBinds are host paths apps commonly mount that exist on every node.
var systemBinds = []string{"/var/run/docker.sock", "/run/docker.sock", "/etc/localtime", "/etc/timezone", "/dev", "/proc", "/sys"}

func under(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

func isSystemBind(src string) bool {
	return slices.ContainsFunc(systemBinds, func(b string) bool { return under(src, b) })
}

// shareOf is the share name for a path under mountRoot (/var/mnt/<name>/…), or "".
func shareOf(src, mountRoot string) string {
	rest, ok := strings.CutPrefix(src, mountRoot+"/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// nfsVolume reports whether a named volume's data is on an NFS server (the local driver
// with type=nfs), so every node sees the same data.
func nfsVolume(v docker.ComposeVolume) bool {
	if v.Driver != "" && v.Driver != "local" {
		return false
	}
	t := v.DriverOpts["type"]
	return t == "nfs" || t == "nfs4" || strings.Contains(v.DriverOpts["o"], "addr=")
}

// DataCheck is what a stack's compose file says about where its data is.
type DataCheck struct {
	Shares   []string `json:"shares"`   // cluster shares it uses
	Problems []string `json:"problems"` // why it can't be a shared HA app
	Warnings []string `json:"warnings"` // what a replicated HA app doesn't copy
}

// CheckData sorts a stack's mounts: binds under a cluster share on every member, binds in
// its folder, named volumes (on local disk or NFS) and anything else.
func CheckData(cf docker.ComposeFile, shares []Share, members []string, mountRoot, stackDir string) DataCheck {
	c := DataCheck{Shares: []string{}, Problems: []string{}, Warnings: []string{}}
	used := map[string]bool{}
	add := func(list *[]string, msg string) {
		if !slices.Contains(*list, msg) {
			*list = append(*list, msg)
		}
	}
	names := make([]string, 0, len(cf.Services))
	for n := range cf.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, svc := range names {
		for _, m := range cf.Services[svc].Volumes {
			switch m.Type {
			case "bind":
				src := filepath.Clean(m.Source)
				if isSystemBind(src) {
					continue
				}
				if name := shareOf(src, mountRoot); name != "" {
					sh, ok := findShare(shares, name)
					switch {
					case !ok:
						add(&c.Problems, fmt.Sprintf("%s: %s is on %s, which isn't a cluster share", svc, src, filepath.Join(mountRoot, name)))
						add(&c.Warnings, fmt.Sprintf("%s: %s isn't a cluster share, so the other members may not have it", svc, src))
					case !sh.OnAll(members):
						add(&c.Problems, fmt.Sprintf("%s: the cluster share %s isn't mounted on every member of the virtual IP", svc, name))
						add(&c.Warnings, fmt.Sprintf("%s: the cluster share %s isn't mounted on every member of the virtual IP", svc, name))
					default:
						used[name] = true
					}
					continue
				}
				if stackDir != "" && under(src, stackDir) {
					add(&c.Problems, fmt.Sprintf("%s: %s is in the stack's folder on this node's disk; move it to a cluster share, or replicate the app", svc, src))
					continue
				}
				add(&c.Problems, fmt.Sprintf("%s: %s is on this node's disk, not on a cluster share", svc, src))
				add(&c.Warnings, fmt.Sprintf("%s: %s is outside the stack's folder and not on a cluster share; it isn't copied", svc, src))
			case "volume":
				if m.Source == "" {
					add(&c.Problems, fmt.Sprintf("%s: an unnamed volume at %s is on this node's disk", svc, m.Target))
					add(&c.Warnings, fmt.Sprintf("%s: an unnamed volume at %s isn't copied; name it in the compose file", svc, m.Target))
					continue
				}
				v := cf.Volumes[m.Source]
				if nfsVolume(v) {
					continue
				}
				if v.External {
					add(&c.Warnings, fmt.Sprintf("%s: the external volume %s isn't copied", svc, m.Source))
				}
				add(&c.Problems, fmt.Sprintf("%s: the volume %s is on this node's disk; use a bind under a cluster share, or an NFS volume", svc, m.Source))
			}
		}
	}
	for n := range used {
		c.Shares = append(c.Shares, n)
	}
	sort.Strings(c.Shares)
	return c
}

func findShare(shares []Share, name string) (Share, bool) {
	for _, s := range shares {
		if s.Name == name {
			return s, true
		}
	}
	return Share{}, false
}

func quote(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid YAML double-quoted scalar
	return string(b)
}

// localHost reports whether a port's host address is a wildcard one, so the port is
// reachable on every address of the node.
func wildcard(ip string) bool { return ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "[::]" }

// Override is compose.x27-ha.yml: every service gets restart: on-failure, so Docker doesn't
// start it at boot on a member that doesn't hold the virtual IP (the dashboard starts it on
// the one that does). With vipOnly, each published port is listed again bound to the
// virtual IP (ports: !override needs Compose 2.24 or newer); ports bound to one address
// already, like 127.0.0.1, stay as they are.
func Override(cf docker.ComposeFile, vip string, vipOnly bool) string {
	var b strings.Builder
	b.WriteString("# Written by the dashboard (Cluster → High-availability apps). Changes here are overwritten.\n")
	b.WriteString("# It's used together with compose.yml, which stays as you wrote it.\n")
	names := make([]string, 0, len(cf.Services))
	for n := range cf.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		b.WriteString("services: {}\n")
		return b.String()
	}
	b.WriteString("services:\n")
	for _, n := range names {
		svc := cf.Services[n]
		fmt.Fprintf(&b, "  %s:\n    restart: on-failure\n", quote(n))
		if !vipOnly || len(svc.Ports) == 0 {
			continue
		}
		b.WriteString("    ports: !override\n")
		for _, p := range svc.Ports {
			host := p.HostIP
			if wildcard(host) {
				host = vip
			}
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			fmt.Fprintf(&b, "      - target: %d\n", p.Target)
			if p.Published != "" {
				fmt.Fprintf(&b, "        published: %s\n", quote(p.Published))
			}
			fmt.Fprintf(&b, "        host_ip: %s\n        protocol: %s\n", quote(host), proto)
			if p.Mode != "" {
				fmt.Fprintf(&b, "        mode: %s\n", p.Mode)
			}
			if p.Name != "" {
				fmt.Fprintf(&b, "        name: %s\n", quote(p.Name))
			}
		}
	}
	return b.String()
}
