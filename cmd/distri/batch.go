package main

import (
	"context"
	"flag"
	"log"
	"os"
	"runtime"

	"github.com/distr1/distri/internal/batch"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/trace"
)

// batch builder.
// use cases:
// - continuous build system:
//   + a new commit comes in, mirror should be updated
//   + local modification to a local tree, rebuild all affected packages
//     → increment version numbers with a tool
//
// - build-all (e.g. create distri image)

// milestone: -bootstrap flag: print which packages need to be bootstrapped (those which depend on themselves)
//   needs cycle detection (e.g. pkg-config→glib→pkg-config)
// milestone: run a build action on a remote machine using cpu(1)

// build action
// build(inputs []file, buildflags []string) []file
// e.g. buildflags = []string{"-cross", "i686"}, passed to distri build
// in an RPC, inputs and outputs should be streamed (uni-directional)
// caching: calculate efficient hash over inputs in parallel
//          cache-key := hash(file-hashes)
// - inputs:
//   - build/distri/pkg/<build dep images>
//   - build/<pkg>source tarball
//   - pkgs/<pkg>/build.textproto
// - outputs:
//   - build/distri/pkg/<image>
//   - build/distri/debug/<image>
//   - build/<pkg>/build-<version>.log
//   - dev/stdout
//   - dev/stderr

// to rebuild the archive: increment version number of all packages (helper tool which does this and commits?)

const batchHelp = `distri batch [-flags]

Build all distri packages.

Packages which are already built (i.e. their .squashfs image exists) are skipped.

Example:
  % distri batch -dry_run
`

func cmdbatch(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("batch", flag.ExitOnError)
	var (
		dryRun    = fset.Bool("dry_run", false, "only print packages which would otherwise be built")
		simulate  = fset.Bool("simulate", false, "simulate builds by sleeping for random times instead of actually building packages")
		rebuild   = fset.Bool("rebuild", false, "rebuild all packages, regardless of whether they need to be built or not")
		jobs      = fset.Int("jobs", runtime.NumCPU(), "number of parallel jobs to run")
		ignoreGov = fset.Bool("dont_set_governor",
			false,
			"Don’t automatically set the “performance” CPU frequency scaling governor. Why wouldn’t you?")
		bootstrapFromPath = fset.String("bootstrap_from",
			"",
			"Bootstrap a distri build based on the specified packages")
	)
	fset.Usage = usage(fset, batchHelp)
	fset.Parse(args)

	if *ctracefile == "" {
		// Enable writing ctrace output files by default for distri batch. Not
		// specifying the flag is a time- and power-costly mistake :)
		trace.Enable("batch")
	}

	if !*ignoreGov {
		cleanup, err := setGovernor("performance")
		if err != nil {
			log.Printf("Setting “performance” CPU frequency scaling governor failed: %v", err)
		} else {
			defer cleanup()
		}
	}

	if *bootstrapFromPath != "" {
		return bootstrapFrom(*bootstrapFromPath, *dryRun)
	}

	bctx := &batch.Ctx{
		Log:        log.New(os.Stdout, "", log.LstdFlags),
		DistriRoot: env.DistriRoot,
		DefaultBuildCtx: &build.Ctx{
			Arch: "amd64", // TODO
			Repo: env.DefaultRepo,
		},
	}
	return bctx.Build(ctx, *dryRun, *simulate, *rebuild, *jobs)
}
