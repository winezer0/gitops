package gitops

import (
	"fmt"
	"path"
	"strings"
)

const maximumSparsePaths = 512

func validateSparsePaths(paths []string) error {
	if len(paths) > maximumSparsePaths {
		return fmt.Errorf("too many sparse checkout paths")
	}
	seen := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		value := strings.TrimSpace(raw)
		if value == "" || value != raw || strings.ContainsAny(value, "\\:\x00") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
			return fmt.Errorf("invalid sparse checkout path")
		}
		for _, segment := range strings.Split(value, "/") {
			if segment == "" || segment == "." || segment == ".." || strings.EqualFold(segment, ".git") {
				return fmt.Errorf("invalid sparse checkout path")
			}
		}
		portable := strings.ToLower(value)
		if _, exists := seen[portable]; exists {
			return fmt.Errorf("duplicate sparse checkout path")
		}
		seen[portable] = struct{}{}
	}
	return nil
}
