package buildtest

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/pb"
)

// Builddeps globs and resolves Builderdeps() and proto build dependencies for
// the transitive closure of the specified packages.
//
// After copying the packages which Builddeps() returns into a separate
// DISTRIROOT, distri batch has all dependencies it needs and will consider all
// packages up to date.
func Builddeps(t testing.TB, packages ...string) []string {
	t.Helper()
	b, err := build.NewCtx()
	if err != nil {
		t.Fatal(err)
	}

	buildDeps := make(map[string]bool)
	seen := make(map[string]bool)
	work := make([]string, len(packages))
	copy(work, packages)
	for len(work) > 0 {
		var pkg string
		pkg, work = work[0], work[1:]
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		build, err := pb.ReadBuildFile(filepath.Join(distritest.DistriRoot, "pkgs", pkg, "build.textproto"))
		if err != nil {
			t.Fatal(err)
		}
		fullname := pkg + "-amd64-" + build.GetVersion()
		buildDeps[fullname] = true
		b.Pkg = pkg
		deps, err := b.Builddeps(build)
		if err != nil {
			t.Fatal(err)
		}
		{
			pkgs := make([]string, len(deps))
			copy(pkgs, deps)
			for idx, pkg := range pkgs {
				meta, err := pb.ReadMetaFile(filepath.Join(distritest.Repo, pkg+".meta.textproto"))
				if err != nil {
					t.Fatal(err)
				}
				pkgs[idx] = meta.GetSourcePkg()
			}
			work = append(work, pkgs...)
		}
		for _, d := range deps {
			buildDeps[d] = true
		}
	}
	result := make([]string, 0, len(buildDeps))
	for d := range buildDeps {
		result = append(result, d)
	}
	sort.Strings(result)
	return result
}
