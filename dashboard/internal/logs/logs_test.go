package logs

import (
	"strings"
	"testing"
)

func TestArgs(t *testing.T) {
	a, err := Args(Query{Unit: "netbird.service", Priority: 4, Since: "1h", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(a, " ")
	for _, want := range []string{"-u netbird.service", "-p 4", "--since -1h", "-n 100"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	for _, bad := range []Query{{Unit: "x; rm -rf /"}, {Unit: "--root=/"}, {Priority: 9}, {Since: "yesterday"}, {Cursor: "a b"}} {
		if _, err := Args(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if a, _ := Args(Query{Kernel: true, Since: "boot", Priority: 7}); strings.Contains(strings.Join(a, " "), " -p ") {
		t.Error("priority 7 should show everything")
	}
}

func TestParse(t *testing.T) {
	out := `{"__CURSOR":"s=1;i=1","__REALTIME_TIMESTAMP":"1760000000123456","PRIORITY":"3","_SYSTEMD_UNIT":"netbird.service","SYSLOG_IDENTIFIER":"netbird","_PID":"42","MESSAGE":"connection failed"}
{"__CURSOR":"s=1;i=2","__REALTIME_TIMESTAMP":"1760000001000000","PRIORITY":"6","SYSLOG_IDENTIFIER":"kernel","MESSAGE":[104,105]}
not json`
	r := Parse(out, "", 10)
	if len(r.Entries) != 2 || r.Cursor != "s=1;i=2" {
		t.Fatalf("%+v", r)
	}
	if e := r.Entries[0]; e.Time != 1760000000123 || e.Priority != 3 || e.Unit != "netbird.service" {
		t.Fatalf("%+v", e)
	}
	if r.Entries[1].Message != "hi" {
		t.Fatalf("byte message: %q", r.Entries[1].Message)
	}
	if r := Parse(out, "FAILED", 10); len(r.Entries) != 1 || r.Cursor != "s=1;i=2" {
		t.Fatalf("grep: %+v", r)
	}
	if r := Parse(out, "", 1); len(r.Entries) != 1 || r.Entries[0].Message != "hi" {
		t.Fatalf("limit keeps the newest: %+v", r)
	}
}
