package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"golang.org/x/xerrors"
)

const logHelp = `distri log [-flags] <package>

Show package build log (local).

Example:
  % distri log i3status
`

func showlog(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("log", flag.ExitOnError)
	var (
		version = fset.String("version", "", "package version (default: most recent)")
	)
	fset.Usage = usage(fset, logHelp)
	fset.Parse(args)
	if fset.NArg() != 1 {
		return xerrors.Errorf("syntax: log <package>")
	}
	pkg := fset.Arg(0)

	var match string
	if *version != "" {
		match = filepath.Join(env.DistriRoot, "build", pkg, "build-"+*version+".log")
	} else {
		matches, err := filepath.Glob(filepath.Join(env.DistriRoot, "build", pkg, "*.log"))
		if err != nil {
			return err
		}
		sort.Slice(matches, func(i, j int) bool {
			return distri.PackageRevisionLess(matches[j], matches[i]) // reverse
		})
		match = matches[0]
	}

	shargs := []string{
		"/bin/sh",
		"-c",
		fmt.Sprintf("${PAGER:-less} %q", match),
	}
	return syscall.Exec("/bin/sh", shargs, os.Environ())
}
