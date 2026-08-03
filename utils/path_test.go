package utils

import (
	"os/user"
	"path"
	"testing"
)

func TestGetDefaultCliConfigPath(t *testing.T) {
	// Save original GOOS getter and restore after test
	originalGetGOOS := getGOOS
	defer func() { getGOOS = originalGetGOOS }()

	tests := []struct {
		name     string
		goos     string
		wantPath string
		user     *user.User
	}{
		{
			name:     "darwin path",
			goos:     "darwin",
			user:     &user.User{HomeDir: "/Users/test"},
			wantPath: path.Join("/Users/test", "Library/Arduino15", "arduino-cli.yaml"),
		},
		{
			name:     "linux path",
			goos:     "linux",
			user:     &user.User{HomeDir: "/home/test"},
			wantPath: path.Join("/home/test", ".arduino15", "arduino-cli.yaml"),
		},
		{
			name:     "windows path",
			goos:     "windows",
			user:     &user.User{HomeDir: "C:\\Users\\test"},
			wantPath: path.Join("C:\\Users\\test", "AppData\\Local\\Arduino15", "arduino-cli.yaml"),
		},
		{
			name:     "nil user",
			goos:     "linux",
			user:     nil,
			wantPath: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// mocks
			getGOOS = tt.goos
			userCurrent = func() (*user.User, error) {
				return tt.user, nil
			}

			got := GetDefaultCliConfigPath()
			if got != tt.wantPath {
				t.Errorf("GetDefaultCliConfigPath() = %v, want %v", got, tt.wantPath)
			}
		})
	}
}
