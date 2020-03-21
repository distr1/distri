package build_test

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

const buildTextproto = `
source: "empty://"
hash: ""
version: "1"

dep: "bash"

runtime_dep: "pkg-config"

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "exit 0"
>
`

func TestBuild(t *testing.T) {
	t.Parallel()

	ctx, canc := distri.InterruptibleContext()
	defer canc()

	distriroot, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, distriroot)

	// Copy build dependencies into our temporary DISTRIROOT:
	repo := filepath.Join(distriroot, "build", "distri", "pkg")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	b, err := build.NewCtx()
	if err != nil {
		t.Fatal(err)
	}
	deps, err := b.GlobAndResolve(env.DefaultRepo, []string{
		"bash",
		"pkg-config",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps {
		cp := exec.CommandContext(ctx, "cp",
			filepath.Join(env.DefaultRepo, dep+".squashfs"),
			filepath.Join(env.DefaultRepo, dep+".meta.textproto"),
			repo)
		cp.Stderr = os.Stderr
		if err := cp.Run(); err != nil {
			t.Fatalf("%v: %v", cp.Args, err)
		}
	}

	// Write package build instructions:
	pkgDir := filepath.Join(distriroot, "pkg", "test")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "build.textproto"),
		[]byte(buildTextproto),
		0644); err != nil {
		t.Fatal(err)
	}

	build := exec.CommandContext(ctx, "distri", "build")
	build.Dir = pkgDir
	build.Env = []string{
		"DISTRIROOT=" + distriroot,
		"PATH=" + os.Getenv("PATH"), // to locate tar(1)
	}
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("%v: %v", build.Args, err)
	}

	// TODO: verify package properties

	t.Run("VerifyRuntimeDep", func(t *testing.T) {
		meta, err := pb.ReadMetaFile(filepath.Join(distriroot, "build", "distri", "pkg", "test-amd64-1.meta.textproto"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"pkg-config-amd64-0.29.2-3", // from hello-1 (direct)
			"glib-amd64-2.58.0-3",       // from pkg-config (transitive)
			"glibc-amd64-2.27-3",        // from glib-2.58.0
			"zlib-amd64-1.2.11-3",       // from glib-2.58.0
			"util-linux-amd64-2.32-6",   // from glib-2.58.0
			"pam-amd64-1.3.1-10",        // from util-linux-2.32
			"libffi-amd64-3.2.1-3",      // from glib-2.58.0
		}
		opts := []cmp.Option{
			cmpopts.SortSlices(func(a, b string) bool {
				return a < b
			}),
		}
		if diff := cmp.Diff(want, meta.GetRuntimeDep(), opts...); diff != "" {
			t.Fatalf("unexpected runtime deps: (-want +got)\n%s", diff)
		}
	})
}
