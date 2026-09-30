package osupdate

import (
	"os"
	"path/filepath"
	"testing"
)

const status = `{"deployments":[
 {"booted":false,"staged":true,"version":"44.20261005","timestamp":1759600000,
  "container-image-reference":"ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:44","checksum":"aaa"},
 {"booted":true,"staged":false,"version":"44.20260928","timestamp":1759000000,
  "container-image-reference":"ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:44",
  "container-image-reference-digest":"sha256:bbb","checksum":"ccc"},
 {"booted":false,"version":"44.20260921","timestamp":1758400000,"checksum":"ddd"}],
 "cached-update":null}`

func TestParseStatus(t *testing.T) {
	st, err := ParseStatus([]byte(status))
	if err != nil {
		t.Fatal(err)
	}
	if st.Booted == nil || st.Booted.Version != "44.20260928" || st.Booted.Image != "ghcr.io/gamerx27/x27-linux-homelab:44" || st.Booted.Digest != "sha256:bbb" {
		t.Fatalf("booted: %+v", st.Booted)
	}
	if st.Staged == nil || st.Staged.Version != "44.20261005" || !st.CanRollback {
		t.Fatalf("staged/rollback: %+v %v", st.Staged, st.CanRollback)
	}
}

func TestParseCheck(t *testing.T) {
	out := `Note: --check and --preview may be unreliable.  See https://github.com/coreos/rpm-ostree/issues/1579
AvailableUpdate:
        Version: 44.20261005 (2026-10-05T02:10:00Z)
         Digest: sha256:0123abcd
      Timestamp: 2026-10-05T02:10:00Z`
	v, d := ParseCheck(out)
	if v != "44.20261005" || d != "sha256:0123abcd" {
		t.Fatalf("got %q %q", v, d)
	}
}

func TestAdjustJob(t *testing.T) {
	cases := []struct {
		phase, boot string
		active      bool
		want        string
	}{
		{PhaseDownloading, "b1", true, PhaseDownloading},
		{PhaseDownloading, "b1", false, PhaseFailed}, // the run died
		{PhaseChecking, "b2", false, PhaseFailed},    // cut off by a reboot
		{PhaseRebooting, "b1", false, PhaseRebooting},
		{PhaseRebooting, "b2", false, PhaseVerifying}, // rebooted, verify hasn't run yet
		{PhaseDone, "b2", false, PhaseDone},
	}
	for _, c := range cases {
		j := adjustJob(&Job{Phase: c.phase, BootID: "b1"}, c.boot, c.active)
		if j.Phase != c.want {
			t.Errorf("%s boot %s active %v: got %s, want %s", c.phase, c.boot, c.active, j.Phase, c.want)
		}
	}
}

func TestReadJobAndLog(t *testing.T) {
	StateDir = t.TempDir()
	if ReadJob("b1", false) != nil {
		t.Fatal("job without a state file")
	}
	os.WriteFile(filepath.Join(StateDir, "state.json"), []byte(`{"id":"1","phase":"staged","to":"44.2","targetChecksum":"abc","startedAt":5}`), 0o644)
	j := ReadJob("b1", false)
	if j == nil || j.Phase != PhaseStaged || j.To != "44.2" || j.TargetChecksum != "abc" || j.StartedAt != 5 {
		t.Fatalf("job: %+v", j)
	}
	os.WriteFile(filepath.Join(StateDir, "last.log"), []byte("one\ntwo\n"), 0o644)
	text, off := ReadLog(0)
	if text != "one\ntwo\n" || off != 8 {
		t.Fatalf("log: %q %d", text, off)
	}
	os.WriteFile(filepath.Join(StateDir, "last.log"), []byte("new\n"), 0o644)
	if text, off = ReadLog(off); text != "new\n" || off != 4 { // a new run starts the log over
		t.Fatalf("restarted log: %q %d", text, off)
	}
}

func TestProgress(t *testing.T) {
	log := "Pulling manifest: x\nostree chunk layers needed: 3 (1 GB)\ncustom layers needed: 1 (5 MB)\n" +
		"Fetching ostree chunk sha256:aa (10 MB)... done\nFetching ostree chunk sha256:bb (10 MB)...\n"
	if p := Progress(log); p != "Downloading layer 2 of 4" {
		t.Fatalf("got %q", p)
	}
	if p := Progress("Checking…\nStaging deployment... done\n"); p != "Staging deployment... done" {
		t.Fatalf("got %q", p)
	}
}

func TestAnyActive(t *testing.T) {
	for out, want := range map[string]bool{
		"inactive\ninactive\ninactive\ninactive\n":   false,
		"inactive\nactivating\ninactive\ninactive\n": true,
		"inactive\ninactive\nactive\ninactive\n":     true,
		"":                                           false,
	} {
		if got := AnyActive(out); got != want {
			t.Errorf("AnyActive(%q) = %v, want %v", out, got, want)
		}
	}
}
