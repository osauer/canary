package main

import (
	"bytes"
	"fmt"
	"strings"
)

func parseRawDiff(raw []byte) ([]stagedChange, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	tokens := bytes.Split(raw, []byte{0})
	if len(tokens) > 0 && len(tokens[len(tokens)-1]) == 0 {
		tokens = tokens[:len(tokens)-1]
	}

	var changes []stagedChange
	for index := 0; index < len(tokens); {
		meta := string(tokens[index])
		index++
		fields := strings.Fields(meta)
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") {
			return nil, fmt.Errorf("malformed raw diff metadata %q", meta)
		}
		oldMode := strings.TrimPrefix(fields[0], ":")
		newMode := fields[1]
		if !validGitMode(oldMode) || !validGitMode(newMode) {
			return nil, fmt.Errorf("malformed raw diff modes %q %q", oldMode, newMode)
		}
		statusText := fields[4]
		if statusText == "" {
			return nil, fmt.Errorf("raw diff entry has no status")
		}
		status := statusText[0]
		pathCount := 1
		switch status {
		case 'R', 'C':
			pathCount = 2
		case 'A', 'M', 'D', 'T':
		default:
			return nil, fmt.Errorf("unsupported raw diff status %q", statusText)
		}
		if index+pathCount > len(tokens) {
			return nil, fmt.Errorf("raw diff status %q lacks %d path(s)", statusText, pathCount)
		}
		paths := make([]string, pathCount)
		for pathIndex := range pathCount {
			paths[pathIndex] = string(tokens[index+pathIndex])
			if paths[pathIndex] == "" {
				return nil, fmt.Errorf("raw diff status %q contains an empty path", statusText)
			}
		}
		index += pathCount
		changes = append(changes, stagedChange{
			status: status, oldMode: oldMode, newMode: newMode, paths: paths,
		})
	}
	return changes, nil
}

func validGitMode(mode string) bool {
	if len(mode) != 6 {
		return false
	}
	for _, char := range mode {
		if char < '0' || char > '7' {
			return false
		}
	}
	return true
}
