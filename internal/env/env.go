// Package env captures details about the distri environment. Inspect the
// environment using `distri env`.
package env

import (
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/distr1/distri"
)

// DistriRoot is the root directory of where the distri repository was checked out.
var DistriRoot = func() string {
	if env := os.Getenv("DISTRIROOT"); env != "" {
		return env
	}

	// TODO: find the dominating distri directory, if any.

	return os.ExpandEnv("$HOME/distri") // default
}()

// DistriConfig is the directory containing distri config files (typically
// /etc/distri).
var DistriConfig = func() string {
	if env := os.Getenv("DISTRICFG"); env != "" {
		return env
	}
	return "/etc/distri" // default
}()

// Repos returns all configured repositories by consulting DistriConfig. It is a
// function to avoid I/O for invocations which don’t need to deal with
// repositories.
func Repos() ([]distri.Repo, error) {
	dir := filepath.Join(DistriConfig, "repos.d")
	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []distri.Repo{{Path: DefaultRepoRoot}}, nil
		}
		return nil, err
	}
	var repos []distri.Repo
	for _, fi := range fis {
		if !strings.HasSuffix(fi.Name(), ".repo") {
			continue
		}
		b, err := ioutil.ReadFile(filepath.Join(dir, fi.Name()))
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		for _, line := range lines {
			// We might want to append key=value pairs to a repo line later
			if idx := strings.Index(line, " "); idx > -1 {
				line = line[:idx]
			}
			repos = append(repos, distri.Repo{
				Path:    line,
				PkgPath: line + "pkg/",
			})
		}
	}
	return repos, nil
}

// DefaultRepoRoot is the default repository path or URL.
var DefaultRepoRoot = func() string {
	if env := os.Getenv("DEFAULTREPOROOT"); env != "" {
		return env
	}
	return join(DistriRoot, "build/distri/") // default
}()

// DefaultRepo is the default repository path or URL to pkg/.
var DefaultRepo = func() string {
	if env := os.Getenv("DEFAULTREPO"); env != "" {
		return env
	}
	return join(DistriRoot, "build/distri/pkg") // default
}()

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
