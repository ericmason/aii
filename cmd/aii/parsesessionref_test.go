package main

import "testing"

func TestParseSessionRef(t *testing.T) {
	cases := []struct {
		in      string
		wantUID string
		wantOrd int
	}{
		{"23a54a8b", "23a54a8b", -1},
		{"cc/23a54a8b", "23a54a8b", -1},
		{"cc/23a54a8b:193", "23a54a8b", 193},
		{"cdx/23a54a8b:0", "23a54a8b", 0},
		{"23a54a8b:42", "23a54a8b", 42},
		{"  cc/23a54a8b:193  ", "23a54a8b", 193},
		{"3f0b9d6e-1234-5678-9abc-def012345678", "3f0b9d6e-1234-5678-9abc-def012345678", -1},
		{"", "", -1},
		// A trailing colon with non-numeric tail is left as part of the uid.
		{"cc/23a54a8b:foo", "23a54a8b:foo", -1},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotUID, gotOrd := parseSessionRef(tc.in)
			if gotUID != tc.wantUID || gotOrd != tc.wantOrd {
				t.Errorf("parseSessionRef(%q) = (%q, %d); want (%q, %d)",
					tc.in, gotUID, gotOrd, tc.wantUID, tc.wantOrd)
			}
		})
	}
}
