package storage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Docker waits for the mounts listed in a drop-in, so containers that start at boot see the
// share and not the empty folder underneath (and don't fill the local disk). WantsMountsFor,
// not RequiresMountsFor: a share that's down delays Docker by the mount's timeout but never
// keeps it from starting.

func dockerText(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return marker + "\n# Docker starts after these mounts are up (or have failed), so containers\n" +
		"# don't write into the empty folders underneath.\n[Unit]\nWantsMountsFor=" + strings.Join(paths, " ") + "\n"
}

func parseDocker(text string) []string {
	if !strings.HasPrefix(text, marker+"\n") {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "WantsMountsFor="); ok {
			paths = append(paths, strings.Fields(v)...)
		}
	}
	return paths
}

func dockerWaits() []string {
	text, _, _ := readMarked(dockerDropIn)
	return parseDocker(text)
}

// setDockerWaits adds or removes one mount point; the caller reloads systemd. It takes
// effect when Docker next starts, at the next boot: Docker isn't restarted for it.
func setDockerWaits(where string, on bool) error {
	text, ours, exists := readMarked(dockerDropIn)
	if exists && !ours {
		return nil
	}
	paths := slices.DeleteFunc(parseDocker(text), func(p string) bool { return p == where })
	if on {
		paths = append(paths, where)
	}
	slices.Sort(paths)
	if len(paths) == 0 {
		if exists {
			return os.Remove(dockerDropIn)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dockerDropIn), 0o755); err != nil {
		return err
	}
	return writeFile(dockerDropIn, dockerText(paths), 0o644)
}
