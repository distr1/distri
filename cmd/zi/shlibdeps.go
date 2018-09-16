package main

import (
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var lddRe = regexp.MustCompile(`^\t([^ ]+) => /ro/([^/]+)`)

func findShlibDeps(fn string) ([]string, error) {
	cmd := exec.Command("ldd", fn)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		log.Printf("TODO: exclude file %s", fn)
		return nil, nil // TODO: fix
		return nil, err
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		matches := lddRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		pkgs = append(pkgs, matches[2])
	}
	return pkgs, nil
}
