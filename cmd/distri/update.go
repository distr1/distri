package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/xerrors"
)

const updateHelp = `TODO
`

func update(args []string) error {
	fset := flag.NewFlagSet("update", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")

		repo = fset.String("repo", "", "repository from which to install packages from. path (default TODO) or HTTP URL (e.g. TODO)")
	)
	fset.Parse(args)
	if *repo == "" {
		return xerrors.Errorf("-repo flag is required")
	}

	if os.Getenv("DISTRI_REEXEC") != "1" {
		if err := install([]string{"-root=" + *root, "-repo=" + *repo, "distri1"}); err != nil {
			return err
		}

		cmd := exec.Command(os.Args[0], append([]string{"update"}, args...)...)
		log.Printf("re-executing %v", cmd.Args)
		// TODO: clean the environment
		cmd.Env = append(os.Environ(), "DISTRI_REEXEC=1")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return xerrors.Errorf("%v: %v", cmd.Args, err)
		}
		return nil
	}

	if err := install([]string{"-root=" + *root, "-repo=" + *repo, "base"}); err != nil {
		return err
	}

	var pkgs []string
	matches, err := filepath.Glob(filepath.Join(*root, "etc", "distri", "pkgset.d", "*.pkgset"))
	if err != nil {
		return err
	}
	for _, match := range matches {
		b, err := ioutil.ReadFile(match)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			pkgs = append(pkgs, line)
		}
	}

	if len(pkgs) == 0 {
		return nil
	}

	if err := install(append([]string{"-root=" + *root, "-repo=" + *repo}, pkgs...)); err != nil {
		return err
	}

	return nil
}
