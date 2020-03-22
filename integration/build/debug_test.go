package build_test

import (
	"archive/tar"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/google/go-cmp/cmp"
)

// TODO: separate test which verifies that without writable_sourcedir, building
// spits out an error about missing files (based on DWARF debug info)

// TODO(maintainability): implement secure:// URL scheme

const debugPackageBuildTextproto = `
# The integration test places source.tar in $DISTRIROOT/build/debug,
# so the URL does not need to be valid:
source: "http://invalid.example/source.tar"
hash: "8ffdc3ac9a42fbb74eba3bb5eadc8a9513c9591344487413e3f2f54d0a4887a5"
version: "1"

writable_sourcedir: true

dep: "bash"
dep: "coreutils"
dep: "gcc"
dep: "binutils"
dep: "musl"
dep: "file"

# This file is supposed to be included in the src squashfs image
# because its symbol is referenced by the DWARF debug info:
build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "echo 'char *generated_sym = (char*)0xc0ffee;' > generated.c"
>

# This file is supposed to NOT be included in the src squashfs image,
# it is an implementation detail of the build system (like config.log):
build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "echo 'wrote generated.c' > generated.log"
>

# Actually build a program; this test needs DWARF debug info:
build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/bin; mkdir -p $d; gcc -g -o $d/print ${DISTRI_SOURCEDIR}/sourcedir.c generated.c ${LDFLAGS} && file $d/print && env && LD_TRACE_LOADED_OBJECTS=1 $d/print"
>
`

const sourceDirC = `
#include <stdio.h>

extern char *generated_sym;

int main() {
  printf("generated symbol: %s\n", generated_sym);
  return 0;
}
`

func writeSingleFileTarball(t *testing.T, dest, fn, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}
	o, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	wr := tar.NewWriter(o)
	if err := wr.WriteHeader(&tar.Header{
		Name: "source/" + fn,
		Size: int64(len(contents)),
		Mode: 0644,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := wr.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := wr.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDebugBuild(t *testing.T) {
	t.Parallel()

	ctx, canc := distri.InterruptibleContext()
	defer canc()

	distriroot, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, distriroot)

	// Write source tarball:
	writeSingleFileTarball(t,
		filepath.Join(distriroot, "build", "debug", "source.tar"),
		"sourcedir.c",
		sourceDirC)

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
		"coreutils",
		"gcc",
		"binutils",
		"musl",
		"file",
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
	pkgDir := filepath.Join(distriroot, "pkg", "debug")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "build.textproto"),
		[]byte(debugPackageBuildTextproto),
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

	f, err := os.Open(filepath.Join(distriroot, "build", "distri", "src", "debug-amd64-1.squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd, err := squashfs.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}

	files, err := list(rd, "", rd.RootInode())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"sourcedir.c",       // from source tarball
		"build/generated.c", // generated during build
		// absent: generated.log
	}

	if diff := cmp.Diff(want, files); diff != "" {
		t.Fatalf("unexpected files: (-want +got)\n%s", diff)
	}
}
