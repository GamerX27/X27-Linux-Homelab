package features

import (
	"slices"
	"testing"
)

func TestScheduleArgs(t *testing.T) {
	cases := []struct {
		freq, wd, day, clock string
		want                 []string
	}{
		{"daily", "", "", "04:00", []string{"on", "daily", "04:00"}},
		{"weekly", "Sun", "", "3:30", []string{"on", "weekly", "sun", "3:30"}},
		{"monthly", "", "31", "23:59", []string{"on", "monthly", "31", "23:59"}},
	}
	for _, c := range cases {
		got, err := ScheduleArgs(c.freq, c.wd, c.day, c.clock)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%v: %v %v", c, got, err)
		}
	}
	for _, bad := range [][4]string{{"daily", "", "", "24:00"}, {"weekly", "funday", "", "01:00"},
		{"monthly", "", "0", "01:00"}, {"hourly", "", "", "01:00"}, {"daily", "", "", "1:00; rm -rf /"}} {
		if _, err := ScheduleArgs(bad[0], bad[1], bad[2], bad[3]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestParseCalendar(t *testing.T) {
	for _, tc := range []struct{ cal, freq, wd, day, clock string }{
		{"*-*-* 03:30:00", "daily", "", "", "03:30"},
		{"Sun *-*-* 04:00:00", "weekly", "sun", "", "04:00"},
		{"*-*-07 23:15:00", "monthly", "", "7", "23:15"},
		{"*-*-15 01:00:00", "monthly", "", "15", "01:00"},
	} {
		f, wd, day, clock, ok := ParseCalendar(tc.cal)
		if !ok || f != tc.freq || wd != tc.wd || day != tc.day || clock != tc.clock {
			t.Errorf("%q: %s %s %s %s %v", tc.cal, f, wd, day, clock, ok)
		}
		if _, err := ScheduleArgs(f, wd, day, clock); err != nil {
			t.Errorf("%q: parsed schedule rejected: %v", tc.cal, err)
		}
	}
	for _, bad := range []string{"", "hourly", "Mon..Fri *-*-* 03:00:00", "Sun *-*-07 03:00:00"} {
		if _, _, _, _, ok := ParseCalendar(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
