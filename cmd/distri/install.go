package main

import (
	"context"
	"flag"

	// TODO: consider "github.com/klauspost/pgzip"

	"github.com/distr1/distri/internal/install"
	"golang.org/x/xerrors"
)

const installHelp = `distri install [-flags] <package>…

Install a distri package from a repository.

Example:
  % distri install i3status
`

func cmdinstall(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")

		repo = fset.String("repo", "", "repository from which to install packages from. path (default TODO) or HTTP URL (e.g. TODO)")

		update = fset.Bool("update", false, "internal flag set by distri update, do not use")

		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Usage = usage(fset, installHelp)
	fset.Parse(args)
	if fset.NArg() < 1 {
		return xerrors.Errorf("syntax: install [options] <package> [<package>...]")
	}

	c := &install.Ctx{}
	if *repo != "" {
		*repo = *repo + "/pkg"
	}
	return c.Packages(fset.Args(), *root, *repo, *update)
}
