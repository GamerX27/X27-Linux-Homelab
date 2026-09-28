// Package system reads host info and resource use straight from /proc and /sys.
// A sampler goroutine turns the cumulative CPU and network counters into rates.
package system

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Root prefixes /proc, /sys and /etc paths; tests point it at fixtures.
var Root = ""

type Info struct {
	Hostname   string `json:"hostname"`
	PrettyName string `json:"prettyName"`
	BuildID    string `json:"buildId"`
	Kernel     string `json:"kernel"`
	CPUModel   string `json:"cpuModel"`
	CPUCount   int    `json:"cpuCount"`
}

type Disk struct {
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type Net struct {
	Name    string  `json:"name"`
	RxBytes uint64  `json:"rxBytes"`
	TxBytes uint64  `json:"txBytes"`
	RxRate  float64 `json:"rxRate"` // bytes/s
	TxRate  float64 `json:"txRate"`
}

type Temp struct {
	Name    string  `json:"name"`
	Celsius float64 `json:"celsius"`
}

type Stats struct {
	UptimeSec  float64    `json:"uptimeSec"`
	Load       [3]float64 `json:"load"`
	CPUPercent float64    `json:"cpuPercent"`
	MemTotal   uint64     `json:"memTotal"`
	MemUsed    uint64     `json:"memUsed"`
	SwapTotal  uint64     `json:"swapTotal"`
	SwapUsed   uint64     `json:"swapUsed"`
	Disks      []Disk     `json:"disks"`
	Net        []Net      `json:"net"`
	Temps      []Temp     `json:"temps"`
}

func read(p string) string {
	b, _ := os.ReadFile(Root + p)
	return string(b)
}

func GetInfo() Info {
	i := Info{CPUCount: runtime.NumCPU()}
	i.Hostname, _ = os.Hostname()
	osr := ParseOSRelease(read("/etc/os-release"))
	i.PrettyName, i.BuildID = osr["PRETTY_NAME"], osr["BUILD_ID"]
	i.Kernel = strings.TrimSpace(read("/proc/sys/kernel/osrelease"))
	for _, line := range strings.Split(read("/proc/cpuinfo"), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			i.CPUModel = strings.TrimSpace(v)
			break
		}
	}
	return i
}

func ParseOSRelease(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		m[k] = strings.Trim(v, `"'`)
	}
	return m
}

// cpuTimes returns total and idle jiffies from the first line of /proc/stat.
func cpuTimes(stat string) (total, idle uint64) {
	line, _, _ := strings.Cut(stat, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0
	}
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		if i == 7 || i == 8 { // guest time is already counted in user/nice
			continue
		}
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return total, idle
}

func ParseMeminfo(s string) (memTotal, memUsed, swapTotal, swapUsed uint64) {
	m := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(f[0], 10, 64)
		m[k] = n * 1024
	}
	return m["MemTotal"], m["MemTotal"] - m["MemAvailable"], m["SwapTotal"], m["SwapTotal"] - m["SwapFree"]
}

func parseNetDev(s string) []Net {
	var out []Net
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out = append(out, Net{Name: name, RxBytes: rx, TxBytes: tx})
	}
	return out
}

var diskFS = map[string]bool{"ext4": true, "ext3": true, "xfs": true, "btrfs": true, "vfat": true, "f2fs": true, "zfs": true, "bcachefs": true}

// disks lists real filesystems once each. On bootc, / is a composefs overlay; the disk
// behind it shows up as /sysroot (and again as /var, /etc, …, which are deduplicated).
func disks() []Disk {
	var out []Disk
	seen := map[string]bool{}
	for _, line := range strings.Split(read("/proc/self/mounts"), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !diskFS[f[2]] || seen[f[0]] {
			continue
		}
		mount := strings.ReplaceAll(f[1], `\040`, " ")
		if strings.HasPrefix(mount, "/var/lib/docker") || strings.HasPrefix(mount, "/var/lib/containers") {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mount, &st) != nil || st.Blocks == 0 {
			continue
		}
		seen[f[0]] = true
		bs := uint64(st.Bsize)
		out = append(out, Disk{Mount: mount, FS: f[2], Total: st.Blocks * bs, Used: (st.Blocks - st.Bfree) * bs})
	}
	sort.Slice(out, func(a, b int) bool { return len(out[a].Mount) < len(out[b].Mount) })
	return out
}

// temps reports the hottest sensor of each hwmon device (k10temp, coretemp, nvme, …).
func temps() []Temp {
	hot := map[string]float64{}
	dirs, _ := filepath.Glob(Root + "/sys/class/hwmon/hwmon*")
	for _, d := range dirs {
		name := strings.TrimSpace(readAbs(filepath.Join(d, "name")))
		if name == "" {
			name = filepath.Base(d)
		}
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		for _, in := range inputs {
			v, err := strconv.ParseFloat(strings.TrimSpace(readAbs(in)), 64)
			if err != nil || v <= 0 {
				continue
			}
			if c := v / 1000; c > hot[name] {
				hot[name] = c
			}
		}
	}
	out := make([]Temp, 0, len(hot))
	for n, c := range hot {
		out = append(out, Temp{Name: n, Celsius: c})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func readAbs(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// Sampler keeps the latest CPU % and network rates.
type Sampler struct {
	mu      sync.Mutex
	cpu     float64
	rates   map[string][2]float64
	lastT   time.Time
	lastTot uint64
	lastIdl uint64
	lastNet map[string][2]uint64
}

func NewSampler() *Sampler {
	s := &Sampler{rates: map[string][2]float64{}, lastNet: map[string][2]uint64{}}
	s.sample()
	go func() {
		for range time.Tick(2 * time.Second) {
			s.sample()
		}
	}()
	return s
}

func (s *Sampler) sample() {
	now := time.Now()
	tot, idl := cpuTimes(read("/proc/stat"))
	nets := parseNetDev(read("/proc/net/dev"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastT.IsZero() {
		if dt := tot - s.lastTot; dt > 0 && tot >= s.lastTot {
			s.cpu = 100 * float64(dt-(idl-s.lastIdl)) / float64(dt)
		}
		secs := now.Sub(s.lastT).Seconds()
		for _, n := range nets {
			if prev, ok := s.lastNet[n.Name]; ok && secs > 0 && n.RxBytes >= prev[0] && n.TxBytes >= prev[1] {
				s.rates[n.Name] = [2]float64{float64(n.RxBytes-prev[0]) / secs, float64(n.TxBytes-prev[1]) / secs}
			}
		}
	}
	s.lastT, s.lastTot, s.lastIdl = now, tot, idl
	for _, n := range nets {
		s.lastNet[n.Name] = [2]uint64{n.RxBytes, n.TxBytes}
	}
}

func (s *Sampler) Stats() Stats {
	var st Stats
	if f := strings.Fields(read("/proc/uptime")); len(f) > 0 {
		st.UptimeSec, _ = strconv.ParseFloat(f[0], 64)
	}
	if f := strings.Fields(read("/proc/loadavg")); len(f) >= 3 {
		for i := range 3 {
			st.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	st.MemTotal, st.MemUsed, st.SwapTotal, st.SwapUsed = ParseMeminfo(read("/proc/meminfo"))
	st.Disks = disks()
	st.Temps = temps()
	st.Net = parseNetDev(read("/proc/net/dev"))
	s.mu.Lock()
	st.CPUPercent = s.cpu
	for i := range st.Net {
		r := s.rates[st.Net[i].Name]
		st.Net[i].RxRate, st.Net[i].TxRate = r[0], r[1]
	}
	s.mu.Unlock()
	return st
}
