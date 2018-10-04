// Package env captures details about the distri environment. Inspect the
// environment using `distri env`.
package env

import "os"

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
