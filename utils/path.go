package utils

import (
	"os/user"
	"path"
	"runtime"
)

// package-level variables for mocking in tests
var (
	userCurrent = user.Current
	getGOOS     = runtime.GOOS
)

// GetDefaultCliConfigPath returns the default path for the ArduinoCLI configuration file.
func GetDefaultCliConfigPath() string {
	if user, _ := userCurrent(); user != nil {
		return path.Join(user.HomeDir, func() string {
			switch getGOOS {
			case "darwin":
				return "Library/Arduino15"
			case "windows":
				return "AppData\\Local\\Arduino15"
			default:
				return ".arduino15"
			}
		}(), "arduino-cli.yaml")
	}
	return ""
}
