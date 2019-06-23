package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var lddRe = regexp.MustCompile(`^\t([^ ]+) => (/ro/([^/]+)[^\s]+)`)

var errLddFailed = errors.New("ldd failed") // sentinel

type libDep struct {
	pkg  string
	path string
}

func findShlibDeps(ldd, fn string, env []string) ([]libDep, error) {
	cmd := exec.Command(ldd, fn)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		log.Printf("TODO: exclude file %s", fn)
		return nil, errLddFailed // TODO: fix
		return nil, err
	}
	var pkgs []libDep
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		matches := lddRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		pkgs = append(pkgs, libDep{
			pkg:  matches[3],
			path: matches[2],
		})
	}
	return pkgs, nil
}
