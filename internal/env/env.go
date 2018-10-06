// Package env captures details about the distri environment. Inspect the
// environment using `distri env`.
package env

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DistriRoot is the root directory of where the distri repository was checked out.
var DistriRoot = findDistriRoot()

func findDistriRoot() string {
	env := os.Getenv("DISTRIROOT")
	if env != "" {
		return env
	}

	// TODO: find the dominating distri directory, if any.

	return os.ExpandEnv("$HOME/distri") // default
}

// TODO: support multiple configured repositories, read from configfile

// DefaultRepo is the default repository path or URL.
var DefaultRepo = join(DistriRoot, "build/distri/pkg")

func join(elem ...string) string {
	if len(elem) == 0 {
		return ""
	}
	if strings.HasPrefix(elem[0], "http://") ||
		strings.HasPrefix(elem[0], "https://") {
		return strings.TrimSuffix(elem[0], "/") + "/" + path.Join(elem[1:]...)
	}
	return filepath.Join(elem...)
}
