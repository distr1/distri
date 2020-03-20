package build

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var lddRe = regexp.MustCompile(`^\t([^ ]+) => (/ro/[^/]+[^\s]+)`)

var errLddFailed = errors.New("ldd failed") // sentinel

type libDep struct {
	pkg      string
	path     string
	basename string
}

func findShlibDeps(ldd, fn string, env []string) ([]libDep, error) {
	cmd := exec.Command(ldd, fn)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		// TODO: do not print an error for wrapper programs
		log.Printf("TODO: exclude file %s: %v (out: %s)", fn, err, string(out))
		return nil, errLddFailed // TODO: fix
		return nil, err
	}
	var pkgs []libDep
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		matches := lddRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		path, err := filepath.EvalSymlinks(matches[2])
		if err != nil {
			return nil, err
		}
		var pkg string
		if strings.HasPrefix(path, "/ro/") {
			pkg = strings.TrimPrefix(path, "/ro/")
			if idx := strings.IndexByte(pkg, '/'); idx > -1 {
				pkg = pkg[:idx]
			}
		}
		pkgs = append(pkgs, libDep{
			pkg:      pkg,
			path:     path,
			basename: filepath.Base(matches[2]),
		})
	}
	return pkgs, nil
}
