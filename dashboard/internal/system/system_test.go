package system

import "testing"

func TestMeminfo(t *testing.T) {
	mt, mu, st, su := ParseMeminfo("MemTotal:  1000 kB\nMemFree: 100 kB\nMemAvailable: 400 kB\nSwapTotal: 200 kB\nSwapFree: 50 kB\n")
	if mt != 1000*1024 || mu != 600*1024 || st != 200*1024 || su != 150*1024 {
		t.Fatal(mt, mu, st, su)
	}
}

func TestCPUTimes(t *testing.T) {
	tot, idle := cpuTimes("cpu  100 0 50 800 50 0 0 0 30 0\ncpu0 1 2 3\n")
	if tot != 1000 || idle != 850 {
		t.Fatal(tot, idle)
	}
}

func TestNetDev(t *testing.T) {
	s := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 500 5 0 0 0 0 0 0 500 5 0 0 0 0 0 0
  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0
docker0: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0
vethab: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0`
	n := parseNetDev(s)
	if len(n) != 1 || n[0].Name != "eth0" || n[0].RxBytes != 1000 || n[0].TxBytes != 2000 {
		t.Fatalf("%+v", n)
	}
}

func TestOSRelease(t *testing.T) {
	m := ParseOSRelease("NAME=\"X27-Linux Homelab\"\nVERSION_ID=44\n# c\nBUILD_ID=\"44-2026-09-24\"\n")
	if m["NAME"] != "X27-Linux Homelab" || m["BUILD_ID"] != "44-2026-09-24" || m["VERSION_ID"] != "44" {
		t.Fatal(m)
	}
}
