package nodes

import "testing"

func TestNormalizeAddress(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.5":            "192.168.1.5:9090",
		"node.lan:9443":          "node.lan:9443",
		"https://10.0.0.2:9090/": "10.0.0.2:9090",
		"[fe80::1]:9090":         "[fe80::1]:9090",
		"fd00::5":                "[fd00::5]:9090",
	} {
		if got, err := NormalizeAddress(in); err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a/b", "host:99999", "user@host"} {
		if _, err := NormalizeAddress(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
