package version

import (
	"testing"
)

func TestNewInfo(t *testing.T) {
	info := NewInfo("TestApp")
	if info.Application != "TestApp" {
		t.Errorf("Expected application name 'TestApp', got '%s'", info.Application)
	}
}

func TestInfoString(t *testing.T) {
	info := &Info{
		Application:   "TestApp",
		VersionString: "1.0.0",
		Commit:        "abc123",
		Date:          "2023-01-01",
	}
	expected := "TestApp Version: 1.0.0 Commit: abc123 Date: 2023-01-01"
	if got := info.String(); got != expected {
		t.Errorf("Expected '%s', got '%s'", expected, got)
	}
}

func TestInfoShortString(t *testing.T) {
	tests := []struct {
		name     string
		info     Info
		expected string
	}{
		{
			name: "full info",
			info: Info{
				Application:   "TestApp",
				VersionString: "1.0.0",
				Commit:        "abc123",
				Date:          "2023-01-01T12:00:00Z",
			},
			expected: "TestApp 1.0.0 (abc123 2023-01-01)",
		},
		{
			name: "no commit",
			info: Info{
				Application:   "TestApp",
				VersionString: "1.0.0",
				Date:          "2023-01-01T12:00:00Z",
			},
			expected: "TestApp 1.0.0 (2023-01-01)",
		},
		{
			name: "no date",
			info: Info{
				Application:   "TestApp",
				VersionString: "1.0.0",
				Commit:        "abc123",
			},
			expected: "TestApp 1.0.0 (abc123)",
		},
		{
			name: "version only",
			info: Info{
				Application:   "TestApp",
				VersionString: "1.0.0",
			},
			expected: "TestApp 1.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.ShortString(); got != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, got)
			}
		})
	}
}
