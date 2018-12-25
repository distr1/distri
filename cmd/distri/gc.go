package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/distr1/distri/pb"
	"google.golang.org/grpc"
)

const gcHelp = `TODO
`

func gc(args []string) error {
	fset := flag.NewFlagSet("gc", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")

		dryRun = fset.Bool("dry_run",
			false,
			"only print packages which would otherwise be deleted")
	)
	fset.Parse(args)

	// src2pkg is a lookup table from the source package name of the wanted
	// packages, e.g. “bash” or “base”, to the package names of the found
	// packages, e.g. “bash-amd64-4.4.18” and “bash-amd64-4.4.19”).
	src2pkg := make(map[string][]string)
	{
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
				src2pkg[line] = nil
			}
		}
		src2pkg["base"] = nil
	}

	var gcpkgs []string
	// transitive contains the package names of the transitive closure of
	transitive := make(map[string]bool)
	{
		// Discover which package versions are installed for the desired source packages
		matches, err := filepath.Glob(filepath.Join(*root, "roimg", "*.meta.textproto"))
		if err != nil {
			return err
		}
		for _, match := range matches {
			meta, err := pb.ReadMetaFile(match)
			if err != nil {
				return err
			}
			src := meta.GetSourcePkg()
			if _, ok := src2pkg[src]; !ok {
				continue // to be garbage collected in the next pass
			}
			pkg := strings.TrimSuffix(filepath.Base(match), ".meta.textproto")
			src2pkg[src] = append(src2pkg[src], pkg)
		}

		// Select the most recent package and read in all (already-flattened)
		// runtime_deps:
		for _, pkgs := range src2pkg {
			if pkgs == nil {
				continue
			}
			sort.Sort(sort.Reverse(sort.StringSlice(pkgs))) // select the most recent package
			transitive[pkgs[0]] = true
			meta, err := pb.ReadMetaFile(filepath.Join(*root, "roimg", pkgs[0]+".meta.textproto"))
			if err != nil {
				return err
			}
			for _, rd := range meta.GetRuntimeDep() {
				transitive[rd] = true
			}
		}

		// Mark all packages for garbage collection which were not referenced
		for _, match := range matches {
			pkg := strings.TrimSuffix(filepath.Base(match), ".meta.textproto")
			if transitive[pkg] {
				continue
			}
			gcpkgs = append(gcpkgs, pkg)
		}
	}

	// delete all gcpkgs (first .meta.textproto, then .squashfs)
	for _, gcpkg := range gcpkgs {
		for _, suffix := range []string{".meta.textproto", ".squashfs"} {
			fn := filepath.Join(*root, "roimg", gcpkg+suffix)
			if *dryRun {
				fmt.Printf("would delete %s", fn)
			} else {
				if err := os.Remove(fn); err != nil {
					return err
				}
			}
		}
	}

	// TODO(correctness): delete all .squashfs without corresponding
	// .meta.textproto (to recover from interruptions)

	// Make the FUSE daemon update its packages.
	ctl, err := os.Readlink(filepath.Join(*root, "ro", "ctl"))
	if err != nil {
		log.Printf("not updating FUSE daemon: %v", err)
		return nil // no FUSE daemon running?
	}

	log.Printf("connecting to %s", ctl)
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	cl := pb.NewFUSEClient(conn)
	if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
		return err
	}

	return nil
}
