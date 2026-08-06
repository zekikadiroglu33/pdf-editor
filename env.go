package main

import (
	"bufio"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE pairs from a .env file, if present, and
// applies them via os.Setenv - but only for keys not already set in the
// real environment, so container/CI env vars always win over the file.
// A missing .env is not an error: docker-compose deployments set real
// env vars directly and never ship a .env file into the image.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}
