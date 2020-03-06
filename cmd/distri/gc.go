package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/pb"
	"google.golang.org/grpc"
)

const gcHelp = `distri gc [-flags]

Garbage collect unreferenced packages.

Example:
  % distri gc -dry_run
`

func gc(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("gc", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally operating on a chroot")

		storeFlag = fset.String("store",
			"",
			"if non-empty, operate on the specified package store directly (operate on the package store in -root/roimg otherwise)")

		dryRun = fset.Bool("dry_run",
			false,
			"only print packages which would otherwise be deleted")
	)
	fset.Usage = usage(fset, gcHelp)
	fset.Parse(args)

	store := *storeFlag
	if store == "" {
		store = filepath.Join(*root, "roimg")
	}

	// eligible maps from package (e.g. libudev) to a list of package
	// versions (e.g. libudev-amd64-239-10) to be garbage collected.
	eligible := make(map[string]map[string]bool)
	{
		matches, err := filepath.Glob(filepath.Join(store, "*.squashfs"))
		if err != nil {
			return err
		}
		for _, match := range matches {
			pkg := strings.TrimSuffix(filepath.Base(match), ".squashfs")
			pv := distri.ParseVersion(pkg)
			if _, ok := eligible[pv.Pkg]; !ok {
				eligible[pv.Pkg] = make(map[string]bool)
			}
			eligible[pv.Pkg][pkg] = true
		}

		// Keep the most recent package version around.
		var kept []string
		for pkg, pkgs := range eligible {
			if len(pkgs) == 0 {
				continue
			}
			if len(pkgs) == 1 {
				for pkg := range pkgs {
					kept = append(kept, pkg)
				}
				eligible[pkg] = nil // keep the only revision around
				continue
			}
			revs := make([]distri.PackageVersion, 0, len(pkgs))
			for pkg := range pkgs {
				revs = append(revs, distri.ParseVersion(pkg))
			}
			sort.Slice(revs, func(i, j int) bool {
				revI := revs[i].DistriRevision
				revJ := revs[j].DistriRevision
				return revI >= revJ // reverse
			})
			mostRecent := revs[0].String()
			delete(eligible[pkg], mostRecent) // keep in store
			kept = append(kept, mostRecent)
		}

		// Keep all of their runtime dependencies around, too. This happens in a
		// separate pass so that we can clearly attribute which package causes
		// which other packages to stick around.
		for _, mostRecent := range kept {
			meta, err := pb.ReadMetaFile(filepath.Join(store, mostRecent+".meta.textproto"))
			if err != nil {
				return err
			}
			for _, rd := range meta.GetRuntimeDep() {
				pv := distri.ParseVersion(rd)
				delete(eligible[pv.Pkg], rd)
			}
		}
	}

	// delete all eligible packages (first .meta.textproto, then .squashfs)
	for _, pkgs := range eligible {
		for pkg := range pkgs {
			for _, suffix := range []string{".meta.textproto", ".squashfs"} {
				fn := filepath.Join(store, pkg+suffix)
				if *dryRun {
					fmt.Printf("rm '%s'\n", fn)
				} else {
					if err := os.Remove(fn); err != nil {
						return err
					}
				}
			}
		}
	}

	if *storeFlag != "" {
		// Not operating on a running system; skip the ScanPackages call.
		return nil
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
