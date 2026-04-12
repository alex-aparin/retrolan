package utils

import (
	"os"
	"path/filepath"
)

type FileContent struct {
	Path    string
	Name    string
	Content []byte
	Error   error
}

// ScanFilesWithPattern scans for files matching a glob pattern
func ScanFilesWithPattern(rootDir string, pattern string) ([]FileContent, error) {
	var results []FileContent

	// Use filepath.Glob to find matching files
	matches, err := filepath.Glob(filepath.Join(rootDir, pattern))
	if err != nil {
		return nil, err
	}

	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			results = append(results, FileContent{
				Path:  path,
				Error: err,
			})
			continue
		}

		if info.IsDir() {
			continue
		}

		content, err := os.ReadFile(path)
		results = append(results, FileContent{
			Path:    path,
			Name:    info.Name(),
			Content: content,
			Error:   err,
		})
	}

	failedFilesCount := 0
	for i, v := range results {
		if err := os.Remove(v.Path); err != nil {
			failedFilesCount++
			// TODO: (alex) add logging
		} else {
			results[i-failedFilesCount] = v
		}
	}

	return results[:len(results)-failedFilesCount], nil
}

func GetEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func SaveStringToFile(content string, filepath string) error {
	// Convert string to bytes and write to file
	// 0644 gives read/write permissions to owner, read-only to others
	err := os.WriteFile(filepath, []byte(content), 0644)
	if err != nil {
		return err
	}
	return nil
}
