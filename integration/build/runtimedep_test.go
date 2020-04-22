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
)

const pkgConfigBuildTextproto = `
source: "empty://"
hash: ""
version: "1"

dep: "bash"
dep: "coreutils"
dep: "libepoxy"

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/lib/pkgconfig/; mkdir -p $d; f=gtk+-3.0.pc; echo 'Requires: gdk-3.0 atk >= 2.15.1 cairo >= 1.14.0 cairo-gobject >= 1.14.0 gdk-pixbuf-2.0 >= 2.30.0 gio-2.0 >= 2.49.4' > $d/$f; echo 'Requires.private: atk atk-bridge-2.0   epoxy >= 1.4 pangoft2 gio-unix-2.0 >= 2.49.4' >> $d/$f"
>
`

func TestPkgConfigRuntimeDeps(t *testing.T) {
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
		//"mesa",
		"bash",
		"coreutils",
		"libepoxy",
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
	pkgDir := filepath.Join(distriroot, "pkg", "pkgconfig")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "build.textproto"),
		[]byte(pkgConfigBuildTextproto),
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

	for _, test := range []struct {
		meta string
		want []string
	}{
		{
			meta: "pkgconfig-amd64-1.meta.textproto",
			want: []string{
				"glibc-amd64-2.31-4",     // from shlibdeps
				"libepoxy-amd64-1.5.2-7", // from pkgconfig
			},
		},
	} {
		test := test // copy
		t.Run("VerifyRuntimeDep/"+test.meta, func(t *testing.T) {
			meta, err := pb.ReadMetaFile(filepath.Join(distriroot, "build", "distri", "pkg", test.meta))
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[string]bool)
			for _, dep := range meta.GetRuntimeDep() {
				got[dep] = true
			}
			for _, want := range test.want {
				if !got[want] {
					t.Errorf("runtime dep %q not found in %v", want, meta.GetRuntimeDep())
				}
			}
		})
	}
}

const shebangBuildTextproto = `
source: "empty://"
hash: ""
version: "1"

dep: "bash"
dep: "coreutils"
dep: "perl"
dep: "gcc" # for wrapper programs
dep: "binutils" # for wrapper programs
dep: "musl" # for wrapper programs

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/bin; mkdir -p $d; echo '#!/ro/perl-amd64-5.30.2-5/bin/perl' > $d/foo"
>
`

func TestShebangRuntimeDep(t *testing.T) {
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
		"coreutils",
		"perl",
		"gcc",      // for wrapper programs
		"binutils", // for wrapper programs
		"musl",     // for wrapper programs
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
	pkgDir := filepath.Join(distriroot, "pkg", "shebang")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "build.textproto"),
		[]byte(shebangBuildTextproto),
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

	for _, test := range []struct {
		meta string
		want []string
	}{
		{
			meta: "shebang-amd64-1.meta.textproto",
			want: []string{
				"glibc-amd64-2.31-4",  // from shlibdeps
				"perl-amd64-5.30.2-5", // from shebang
			},
		},
	} {
		test := test // copy
		t.Run("VerifyRuntimeDep/"+test.meta, func(t *testing.T) {
			meta, err := pb.ReadMetaFile(filepath.Join(distriroot, "build", "distri", "pkg", test.meta))
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[string]bool)
			for _, dep := range meta.GetRuntimeDep() {
				got[dep] = true
			}
			for _, want := range test.want {
				if !got[want] {
					t.Errorf("runtime dep %q not found in %v", want, meta.GetRuntimeDep())
				}
			}
		})
	}
}
