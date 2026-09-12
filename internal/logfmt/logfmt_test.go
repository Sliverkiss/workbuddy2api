package logfmt

import "testing"

func TestUID8(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		want string
	}{
		{"full uuid truncated to 8", "5895a56c-449f-49e7-b723-8ddf10a97cdb", "5895a56c"},
		{"exactly 8 chars unchanged", "00e26541", "00e26541"},
		{"short uid unchanged", "abc", "abc"},
		{"empty uid returns dash", "", "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange — tc.uid
			// Act
			got := UID8(tc.uid)
			// Assert
			if got != tc.want {
				t.Errorf("UID8(%q) = %q, want %q", tc.uid, got, tc.want)
			}
		})
	}
}
