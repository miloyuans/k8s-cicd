package utils

import (
	"regexp"
	"strings"
)

// ClassifyService determines the category of a service based on keyword patterns.
func ClassifyService(service string, keywords map[string][]string) string {
	lowerService := strings.ToLower(service)
	for category, patterns := range keywords {
		for _, pattern := range patterns {
			lowerPattern := strings.ToLower(pattern)
			if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
				re, err := regexp.Compile(lowerPattern)
				if err == nil && re.MatchString(lowerService) {
					return category
				}
			} else if strings.Contains(lowerService, lowerPattern) {
				return category
			}
		}
	}
	return "other"
}

// GetVersionFromImage extracts the version from a container image string.
func GetVersionFromImage(image string) string {
	parts := strings.Split(image, ":")
	if len(parts) == 2 {
		return parts[1]
	}
	return "unknown"
}