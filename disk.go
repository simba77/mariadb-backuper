package main

import (
	"fmt"
	"syscall"
)

const gib = 1 << 30

// freeSpace returns the space available to unprivileged users on the file
// system of the path: the root-reserved blocks are not counted on purpose
func freeSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func formatSize(bytes uint64) string {
	switch {
	case bytes >= gib:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/gib)
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
