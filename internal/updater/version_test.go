package updater

import (
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		v1       string
		v2       string
		expected int
	}{
		{"2.9.4", "2.9.4", 0},
		{"v2.9.4", "2.9.4", 0},
		{"2.9.4", "v2.9.4", 0},
		{"v2.9.4", "v2.9.4", 0},
		{"2.9.4", "2.9.5", -1},
		{"2.9.5", "2.9.4", 1},
		{"2.9.4", "2.10.0", -1},
		{"2.10.0", "2.9.4", 1},
		{"2.9", "2.9.0", 0},
		{"2.9.0", "2.9", 0},
		{"3.0.0", "2.99.99", 1},
		{"v2.9.4-beta", "v2.9.4", -1},
		{"v2.9.4", "v2.9.4-rc1", 1},
		{"v2.9.4-beta1", "v2.9.4-beta2", -1},
		{"v2.9.4-beta.2", "v2.9.4-beta.11", -1},
		{"v2.9.4-rc.1", "v2.9.4-beta.2", 1},
		{"v2.9.4+build123", "v2.9.4+build456", 0},
		{"v2.9.4-beta+123", "v2.9.4-beta", 0},
		{"2.9a", "2.9b", -1},
		{"2.10a", "2.9b", 1},
		{"", "2.0.0", -1},
		{"2.0.0", "", 1},
		{"", "", 0},
	}

	for _, c := range cases {
		got := CompareVersions(c.v1, c.v2)
		if got != c.expected {
			t.Errorf("CompareVersions(%q, %q) = %d; want %d", c.v1, c.v2, got, c.expected)
		}
	}
}
