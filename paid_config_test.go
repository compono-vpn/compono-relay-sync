package main

import "testing"

func TestPaidModeConfigurationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		input            string
		enabled, invalid bool
	}{
		{"", false, false}, {"false", false, false}, {"true", true, false},
		{" TRUE ", true, false}, {"treu", false, true}, {"yes", false, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			enabled, err := parsePaidMode(tc.input)
			if (err != nil) != tc.invalid || enabled != tc.enabled {
				t.Fatalf("enabled=%v error=%v", enabled, err)
			}
		})
	}
}
