package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const resetHelp = `distri reset [-flags] <path/to/files.before.txt>

Reset your package store to the contents specified by the provided
files.before.txt, which distri updates writes.

Example:
  % distri reset /var/log/distri/update-1561126278/files.before.txt
`

func reset(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("reset", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")
		write = fset.Bool("w",
			false,
			"write changes (default is dry run)")
	)
	fset.Usage = usage(fset, resetHelp)
	fset.Parse(args)

	if fset.NArg() != 1 {
		fset.Usage()
		os.Exit(2)
	}
	before := fset.Arg(0)
	b, err := ioutil.ReadFile(before)
	if err != nil {
		return err
	}
	pkgs := strings.Split(strings.TrimSpace(string(b)), "\n")
	keep := make(map[string]bool, len(pkgs))
	for _, pkg := range pkgs {
		keep[pkg] = true
	}
	roimg := filepath.Join(*root, "roimg")
	log.Printf("resetting package store %s to contents %s", roimg, before)
	f, err := os.Open(roimg)
	if err != nil {
		return err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		if keep[n] {
			continue
		}
		log.Printf("deleting %s", n)
		if !*write {
			continue
		}
		if err := os.Remove(filepath.Join(roimg, n)); err != nil {
			return err
		}
	}

	return nil
}
