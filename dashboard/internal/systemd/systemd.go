// Package systemd reads unit properties with `systemctl show`, for the packages that
// report on units (Features, Storage).
package systemd

import (
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

// Show returns the unit's properties as KEY → value. Timestamps come as "@<unix seconds>".
func Show(unit string, props ...string) map[string]string {
	args := []string{"show", unit, "--timestamp=unix"}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out, _ := run.Cmd(5*time.Second, "systemctl", args...)
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}
