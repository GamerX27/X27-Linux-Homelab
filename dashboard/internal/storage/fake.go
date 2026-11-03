package storage

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Fake is only for `dashboard serve --dev` with DASHBOARD_FAKE_STORAGE=1: mounts live in
// memory, so the Storage page can be tried on a machine that isn't a node. A test against
// the server fail.example fails; everything else connects. Nothing is mounted.
var Fake bool

var fake = struct {
	sync.Mutex
	mounts map[string]Mount
}{mounts: map[string]Mount{}}

func fakeList() []Mount {
	fake.Lock()
	defer fake.Unlock()
	out := []Mount{}
	for _, m := range fake.mounts {
		if m.State == "mounted" {
			m.Total, m.Used = 4<<40, 1<<40+(uint64(len(m.Name))<<36)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func fakeTest(m Mount) (string, error) {
	if m.Server == "fail.example" {
		return "", errors.New(hint("mount error(112): Host is down"))
	}
	return fmt.Sprintf("Connected to %s: 12 item(s) at the top.", source(m)), nil
}

func fakeSave(m Mount, create bool) error {
	if _, err := fakeTest(m); err != nil {
		return err
	}
	fake.Lock()
	defer fake.Unlock()
	if _, ok := fake.mounts[m.Name]; ok == create {
		if create {
			return fmt.Errorf("there's already a mount named %s", m.Name)
		}
		return fmt.Errorf("there's no mount named %s", m.Name)
	}
	m.MountPoint, m.Unit, _ = names(m.Name)
	m.HasPassword = m.Type == "smb" && !m.Guest
	m.Enabled, m.State = true, "mounted"
	if m.Automount {
		m.State = "waiting"
	}
	fake.mounts[m.Name] = m
	return nil
}

func fakeAction(name, action string) error {
	fake.Lock()
	defer fake.Unlock()
	m, ok := fake.mounts[name]
	if !ok {
		return fmt.Errorf("there's no mount named %s", name)
	}
	switch action {
	case "mount":
		if m.Server == "fail.example" {
			return errors.New(hint("mount error(112): Host is down"))
		}
		m.State = "mounted"
	case "unmount":
		m.State = "off"
	case "enable", "disable":
		m.Enabled = action == "enable"
	case "remove":
		delete(fake.mounts, name)
		return nil
	default:
		return errors.New("unknown action")
	}
	fake.mounts[name] = m
	return nil
}
