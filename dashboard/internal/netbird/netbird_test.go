package netbird

import "testing"

const sample = `{"peers":{"total":2,"connected":1,"details":[
 {"fqdn":"nas.netbird.cloud","netbirdIp":"100.92.1.2","status":"Connected","connectionType":"P2P","latency":12500000,"transferReceived":2048,"transferSent":1024},
 {"fqdn":"pi.netbird.cloud","netbirdIp":"100.92.1.3","status":"Idle","connectionType":"-","latency":0}]},
 "cliVersion":"0.36.0","daemonVersion":"0.36.0",
 "management":{"url":"https://api.netbird.io:443","connected":true,"error":""},
 "signal":{"url":"https://signal.netbird.io:443","connected":true,"error":""},
 "netbirdIp":"100.92.1.1/16","fqdn":"homelab.netbird.cloud"}`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Connected || s.State != "connected" || s.IP != "100.92.1.1/16" || s.PeersTotal != 2 || s.PeersUp != 1 {
		t.Fatalf("%+v", s)
	}
	if p := s.Peers[0]; p.LatencyMs != 12 || p.Connection != "P2P" || p.Received != 2048 {
		t.Fatalf("%+v", p)
	}
}

func TestUpArgs(t *testing.T) {
	if a, err := upArgs(""); err != nil || len(a) != 1 {
		t.Fatal(a, err)
	}
	if a, err := upArgs("https://netbird.example.com"); err != nil || a[2] != "https://netbird.example.com" {
		t.Fatal(a, err)
	}
	if _, err := upArgs("netbird.example.com; rm -rf /"); err == nil {
		t.Fatal("bad URL accepted")
	}
	if keyRe.MatchString("abc def") || !keyRe.MatchString("A1B2C3D4-E5F6-7890-ABCD-EF1234567890") {
		t.Fatal("setup key check")
	}
}
