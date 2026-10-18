package users

import "testing"

const passwd = `root:x:0:0:Super User:/root:/bin/bash
sindre:x:1000:1000:Sindre,,,:/var/home/sindre:/usr/bin/fish
anna:x:1001:1001::/var/home/anna:/bin/bash
nobody:x:65534:65534:Kernel Overflow User:/:/usr/sbin/nologin
broken line`

func TestParse(t *testing.T) {
	us := parsePasswd(passwd)
	if len(us) != 2 || us[0].Name != "sindre" || us[0].Full != "Sindre" || us[1].Shell != "/bin/bash" {
		t.Fatalf("%+v", us)
	}
	g := parseGroups("wheel:x:10:sindre,anna\ndocker:x:978:\n")
	if len(g["wheel"]) != 2 || len(g["docker"]) != 0 {
		t.Fatalf("%+v", g)
	}
}

func TestWheelGuards(t *testing.T) {
	l := List{Users: []User{{Name: "sindre", Wheel: true}, {Name: "anna", Wheel: true}, {Name: "bob"}}}
	if err := CheckRemoveWheel(l, "sindre", "sindre"); err == nil {
		t.Error("removed yourself from wheel")
	}
	if err := CheckRemoveWheel(l, "sindre", "anna"); err != nil {
		t.Error(err)
	}
	l.Users[0].Locked = true // only anna can log in now
	if err := CheckRemoveWheel(l, "sindre", "anna"); err == nil {
		t.Error("removed the last usable wheel user")
	}
}

func TestPasswordCheck(t *testing.T) {
	for _, bad := range []string{"short", "has:colon1", "line\nbreak1"} {
		if checkPassword(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if checkPassword("correct horse") != nil {
		t.Error("good password refused")
	}
	if nameRe.MatchString("Root") || nameRe.MatchString("-x") || !nameRe.MatchString("anna_2") {
		t.Error("name check")
	}
}
