package updates

import "testing"

func TestVersionsAreOrderedByNumberAndABuildIsOlderThanItsRelease(t *testing.T) {
	order := []string{"0.4.0-dev", "0.4.0", "0.4.1-3-gdd6b25b", "0.4.1", "0.10.0", "1.0.0+local", "1.0.0"}
	for i, a := range order {
		va, ok := ParseVersion(a)
		if !ok {
			t.Fatalf("%q is not read as a version", a)
		}
		for j, b := range order {
			vb, _ := ParseVersion(b)
			if got, want := va.Less(vb), i < j; got != want {
				t.Errorf("%s < %s = %v, want %v", a, b, got, want)
			}
		}
	}
}

func TestWhatIsAVersionAndWhatIsARelease(t *testing.T) {
	for s, want := range map[string][2]bool{
		"0.4.0":                    {true, true},
		"12.0.3":                   {true, true},
		"0.4.0-dev":                {true, false},
		"0.4.0+x":                  {true, false},
		"dev":                      {false, false},
		"":                         {false, false},
		"0.4":                      {false, false},
		"0.4.0.1":                  {false, false},
		"v0.4.0":                   {false, false},
		"0.04.0":                   {false, false},
		"0.4.-1":                   {false, false},
		"0.4.0-":                   {true, false},
		"latest":                   {false, false},
		"1.2.3456789":              {true, true},
		"1.2.34567890123456789012": {false, false},
		"１.2.3":                    {false, false},
	} {
		if _, ok := ParseVersion(s); ok != want[0] {
			t.Errorf("ParseVersion(%q) ok = %v, want %v", s, ok, want[0])
		}
		if got := IsRelease(s); got != want[1] {
			t.Errorf("IsRelease(%q) = %v, want %v", s, got, want[1])
		}
	}
}

func TestTargets(t *testing.T) {
	if got := Target("windows", "amd64"); got != "windows-amd64" {
		t.Errorf("Target = %q", got)
	}
	for s, want := range map[string]bool{
		"windows-amd64": true, "linux-arm64": true, "darwin-arm64": true,
		"": false, "windows": false, "windows-amd64.exe": false, "../x-y": false, "a-b-c": false, "Windows-amd64": false, "latest": false,
	} {
		if got := ValidTarget(s); got != want {
			t.Errorf("ValidTarget(%q) = %v, want %v", s, got, want)
		}
	}
}
