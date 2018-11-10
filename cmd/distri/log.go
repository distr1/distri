package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/distr1/distri/internal/env"
)

const logHelp = `TODO
`

func showlog(args []string) error {
	fset := flag.NewFlagSet("log", flag.ExitOnError)
	var ()
	fset.Parse(args)
	if fset.NArg() != 1 {
		return fmt.Errorf("syntax: log <package>")
	}
	pkg := fset.Arg(0)

	matches, err := filepath.Glob(filepath.Join(env.DistriRoot, "build", pkg, "*.log"))
	if err != nil {
		return err
	}
	if got, want := len(matches), 1; got != want {
		return fmt.Errorf("unexpected number of logfiles: got %d, want %d (matches: %v)", got, want, matches)
	}

	shargs := []string{
		"/bin/sh",
		"-c",
		fmt.Sprintf("${PAGER:-less} %q", matches[0]),
	}
	return syscall.Exec("/bin/sh", shargs, os.Environ())
}
