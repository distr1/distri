package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
)

const bumpHelp = `distri bump [-flags] [package]

bump increases the version number of the specified packages and their reverse
dependencies.
`

type versionIncrement struct {
	pkg     string
	current string
	new     string
}

func (i *versionIncrement) Perform() error {
	fn := filepath.Join(env.DistriRoot, "pkgs", i.pkg, "build.textproto")
	b, err := ioutil.ReadFile(fn)
	if err != nil {
		return err
	}
	// TODO: programmatically modify textproto
	lines := strings.Split(string(b), "\n")
	rewritten := make([]string, len(lines))
	for idx, line := range lines {
		rewritten[idx] = strings.ReplaceAll(line, `version: "`+i.current+`"`, `version: "`+i.new+`"`)
	}
	return renameio.WriteFile(fn, []byte(strings.Join(rewritten, "\n")), 0644)
}

func incrementVersion(current string) string {
	v := distri.ParseVersion(current)
	v.DistriRevision++
	return v.String()
}

func bumpAll(write bool) ([]versionIncrement, error) {
	d, err := os.Open(filepath.Join(env.DistriRoot, "pkgs"))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	d.Close()
	var inc []versionIncrement
	for _, name := range names {
		fn := filepath.Join(env.DistriRoot, "pkgs", name, "build.textproto")
		b, err := ioutil.ReadFile(fn)
		if err != nil {
			return nil, err
		}
		var build pb.Build
		if err := proto.UnmarshalText(string(b), &build); err != nil {
			return nil, err
		}

		inc = append(inc, versionIncrement{
			pkg:     name,
			current: build.GetVersion(),
			new:     incrementVersion(build.GetVersion()),
		})
	}
	return inc, nil
}

func bump(args []string) error {
	fset := flag.NewFlagSet("bump", flag.ExitOnError)
	var (
		all   = fset.Bool("all", false, "bump all packages")
		write = fset.Bool("w", false, "write changes (default is a dry run)")
	)
	fset.Usage = usage(fset, bumpHelp)
	fset.Parse(args)

	var inc []versionIncrement
	if *all {
		var err error
		inc, err = bumpAll(*write)
		if err != nil {
			return err
		}
	} else {
		// TODO: verify flag.Args() contains a package name
	}
	if *write {
		for _, i := range inc {
			if err := i.Perform(); err != nil {
				return err
			}
			log.Printf("bumped %s from %s to %s", i.pkg, i.current, i.new)
		}
		return nil
	} else {
		for _, i := range inc {
			log.Printf("bump package %s from %s to %s", i.pkg, i.current, i.new)
		}
	}

	return nil
}
