package cluster

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// A shared HA app's lock is a file on its cluster share, /var/mnt/<share>/.x27-ha/<app>.lock,
// holding the node that runs it and when it last said so. The holder refreshes it every
// lockBeat; another node starts the app only once that is lockStale old. With two members
// there's no quorum, so this is what keeps both from writing the same files when they
// can't see each other but can both see the share.
const (
	lockBeat  = 10 * time.Second
	lockStale = 30 * time.Second
)

type Lock struct {
	Node string `json:"node"`
	At   int64  `json:"at"`
}

func lockPath(mountRoot, share, app string) string {
	return filepath.Join(mountRoot, share, ".x27-ha", app+".lock")
}

// withTimeout runs fn but gives up after d: a share whose server is gone can block file
// calls for a long time.
func withTimeout(d time.Duration, fn func() error) error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		return errors.New("the share didn't answer")
	}
}

func readLock(p string) (Lock, error) {
	var l Lock
	err := withTimeout(5*time.Second, func() error {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, &l)
	})
	return l, err
}

func writeLock(p, node string, now time.Time) error {
	return withTimeout(5*time.Second, func() error {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		b, _ := json.Marshal(Lock{Node: node, At: now.Unix()})
		tmp := p + "." + node + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, p)
	})
}

// removeLock lets go of the lock, if it's still this node's.
func removeLock(p, node string) {
	withTimeout(5*time.Second, func() error {
		if l, err := readLock(p); err == nil && l.Node == node {
			return os.Remove(p)
		}
		return nil
	})
}

// lockBlocker is the node that keeps node from taking the lock, or "" when it may: there's
// no lock, it's node's own, or its holder hasn't refreshed it for lockStale.
func lockBlocker(l Lock, node string, now time.Time) string {
	if l.Node == "" || l.Node == node || now.Sub(time.Unix(l.At, 0)) >= lockStale {
		return ""
	}
	return l.Node
}
