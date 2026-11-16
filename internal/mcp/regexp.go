package mcp

import (
	"fmt"
	"regexp"
)

func compile(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	return re, nil
}
