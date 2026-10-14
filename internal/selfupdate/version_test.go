package selfupdate

import "testing"

func TestVersionsOrderAsSemVerSays(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.3.0-rc.1", "0.2.0", 1}, // a candidate on the machine is newer than the older stable: no downgrade
		{"0.3.0", "0.3.0-rc.2", 1}, // the final is newer than its candidates
		{"0.3.0-rc.2", "0.3.0-rc.1", 1},
		{"0.3.0-rc.10", "0.3.0-rc.9", 1}, // numeric identifiers compare as numbers
		{"0.3.0-rc.1", "0.3.0-rc", 1},    // a longer pre-release wins when the shared part is equal
		{"0.3.0-beta", "0.3.0-alpha", 1}, // other identifiers compare as text
		{"0.3.0-alpha", "0.3.0-1", 1},    // and rank above numbers
		{"v0.3.0", "0.3.0", 0},
		{"1.0.0", "0.99.99", 1},
		{"0.10.0", "0.9.0", 1},
		{"0.2.1", "0.2.0", 1},
	} {
		a, errA := ParseVersion(c.a)
		b, errB := ParseVersion(c.b)
		if errA != nil || errB != nil {
			t.Fatalf("%s, %s: %v %v", c.a, c.b, errA, errB)
		}
		if got := a.Compare(b); got != c.want {
			t.Errorf("%s vs %s: %d, want %d", c.a, c.b, got, c.want)
		}
		if got := b.Compare(a); got != -c.want {
			t.Errorf("%s vs %s: %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestParseVersionRefusesWhatIsNoRelease(t *testing.T) {
	for _, s := range []string{"", "dev", "0.3", "0.3.0.1", "0.3.0-", "x.y.z", "0.3.0-rc..1", "-1.0.0", "0.3.0+build"} {
		if v, err := ParseVersion(s); err == nil {
			t.Errorf("%q parsed as %+v", s, v)
		}
	}
}

func TestVersionStringDropsTheV(t *testing.T) {
	v, err := ParseVersion("v0.3.0-rc.1")
	if err != nil || v.String() != "0.3.0-rc.1" || !v.Prerelease() {
		t.Fatalf("%q %v %v", v.String(), err, v.Prerelease())
	}
	if stable, _ := ParseVersion("0.3.0"); stable.Prerelease() {
		t.Fatal("0.3.0 is no pre-release")
	}
}
