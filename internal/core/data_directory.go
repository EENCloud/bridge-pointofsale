package core

import (
	"os"
	"path/filepath"
)

// GetDataDirectory returns the best available data directory, trying production paths first,
// then falling back to user-accessible locations for development/testing
func GetDataDirectory() string {
	// Try production paths first
	productionPaths := []string{
		"/opt/een/data/point_of_sale",
		"/var/lib/bridge-pointofsale",
		"/usr/local/var/bridge-pointofsale",
	}

	for _, path := range productionPaths {
		if err := os.MkdirAll(path, 0755); err == nil {
			// Test if we can actually write to it
			testFile := filepath.Join(path, ".write_test")
			if file, err := os.Create(testFile); err == nil {
				_ = file.Close()
				_ = os.Remove(testFile)
				return path
			}
		}
	}

	// Fall back to user-accessible locations for development
	fallbackPaths := []string{
		filepath.Join(os.TempDir(), "bridge-pointofsale"),
		"./data",
		"./test_data",
	}

	for _, path := range fallbackPaths {
		if err := os.MkdirAll(path, 0755); err == nil {
			return path
		}
	}

	// Last resort - current directory
	return "."
}
