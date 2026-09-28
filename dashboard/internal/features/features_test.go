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
