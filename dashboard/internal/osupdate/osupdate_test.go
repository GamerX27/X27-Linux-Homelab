package osupdate

import "testing"

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
