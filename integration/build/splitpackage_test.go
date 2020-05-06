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
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

const (
	splitPackageBuildTextproto = `
source: "empty://"
hash: ""
version: "1"

dep: "bash"
dep: "coreutils"
dep: "libxcb"
dep: "yajl2"
dep: "gcc"
dep: "binutils"
dep: "pkg-config"
dep: "musl"

extra_file: "xcb.c"
extra_file: "yajl.c"

split_package: <
  name: "multi-libs"
  runtime_dep: "bash"
  claim: < glob: "out/bin/xcb" >
>

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/bin; mkdir -p $d; gcc ${DISTRI_SOURCEDIR}/xcb.c -o $d/xcb -Wall $(pkg-config --cflags --libs xcb) && gcc ${DISTRI_SOURCEDIR}/yajl.c -o $d/yajl -Wall $(pkg-config --cflags --libs yajl)"
>

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/share/doc/multi; mkdir -p $d; touch $d/README.md"
>
`

	splitPackageXcbC = `
#include <xcb/xcb.h>
int main(int argc, char *argv[]) {
  xcb_disconnect(NULL); // documented as a no-op
  return 0;
}
`

	splitPackageYajlC = `
#include <yajl_version.h>
int main(int argc, char *argv[]) {
  yajl_version();
  return 0;
}
`
)

func list(rd *squashfs.Reader, dir string, inode squashfs.Inode) ([]string, error) {
	fis, err := rd.Readdir(inode)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, fi := range fis {
		if fi.IsDir() {
			r, err := list(rd, filepath.Join(dir, fi.Name()), fi.Sys().(*squashfs.FileInfo).Inode)
			if err != nil {
				return nil, err
			}
			files = append(files, r...)
		} else {
			files = append(files, filepath.Join(dir, fi.Name()))
		}
	}
	return files, nil
}

func readLink(rd *squashfs.Reader, name string) (string, error) {
	inode, err := rd.LlookupPath(name)
	if err != nil {
		return "", nil
	}
	return rd.ReadLink(inode)
}

func TestSplitPackageBuild(t *testing.T) {
	t.Parallel()

	ctx, canc := distri.InterruptibleContext()
	defer canc()

	dr, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, dr)
	distriroot := env.DistriRootDir(dr)

	// Copy build dependencies into our temporary DISTRIROOT:
	repo := filepath.Join(distriroot.BuildDir("distri"), "pkg")
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
		"libxcb",
		"yajl2",
		"gcc",
		"binutils", // TODO: make this a runtime_dep in gcc
		"pkg-config",
		"musl",
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
	pkgDir := distriroot.PkgDir("multi")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "build.textproto"),
		[]byte(splitPackageBuildTextproto),
		0644); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "xcb.c"),
		[]byte(splitPackageXcbC),
		0644); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "yajl.c"),
		[]byte(splitPackageYajlC),
		0644); err != nil {
		t.Fatal(err)
	}

	build := exec.CommandContext(ctx, "distri", "build")
	build.Dir = pkgDir
	build.Env = []string{
		"DISTRIROOT=" + string(distriroot),
		"PATH=" + os.Getenv("PATH"), // to locate tar(1)
	}
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("%v: %v", build.Args, err)
	}

	// TODO: verify package properties

	for _, test := range []struct {
		image string
		want  []string
	}{
		{
			image: "multi-amd64-1.squashfs",
			want: []string{
				"out/lib/liba.so", // symlink
				"out/share/doc/multi/README.md",
			},
		},

		{
			image: "multi-libs-amd64-1.squashfs",
			want: []string{
				"out/lib/liba.so",
			},
		},
	} {
		test := test // copy
		t.Run("VerifySquashFS/"+test.image, func(t *testing.T) {
			f, err := os.Open(filepath.Join(distriroot.BuildDir("distri"), "pkg", test.image))
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

			if diff := cmp.Diff(test.want, files); diff != "" {
				t.Fatalf("unexpected files: (-want +got)\n%s", diff)
			}

			if test.image == "multi-amd64-1.squashfs" {
				// Verify out/lib/liba.so is a symlink to the version in multi-libs
				target, err := readLink(rd, "out/lib/liba.so")
				if err != nil {
					t.Fatal(err)
				}
				if got, want := target, "../../../multi-libs-amd64-1/out/lib/liba.so"; got != want {
					t.Errorf("ReadLink(%s) = %q, want %q", "out/lib/liba.so", got, want)
				}
			}
		})
	}

	for _, test := range []struct {
		meta string
		want []string
	}{
		{
			meta: "multi-amd64-1.meta.textproto",
			want: []string{
				"multi-libs-amd64-1", // from splitting
				// from multi-libs:
				"bash-amd64-5.0-4",
				"glibc-amd64-2.31-4",  // from bash
				"ncurses-amd64-6.2-9", // from bash
			},
		},

		{
			meta: "multi-libs-amd64-1.meta.textproto",
			want: []string{
				"bash-amd64-5.0-4",
				"glibc-amd64-2.31-4",  // from bash
				"ncurses-amd64-6.2-9", // from bash
			},
		},
	} {
		test := test // copy
		t.Run("VerifyRuntimeDep/"+test.meta, func(t *testing.T) {
			meta, err := pb.ReadMetaFile(filepath.Join(distriroot.BuildDir("distri"), "pkg", test.meta))
			if err != nil {
				t.Fatal(err)
			}
			opts := []cmp.Option{
				cmpopts.SortSlices(func(a, b string) bool {
					return a < b
				}),
			}
			if diff := cmp.Diff(test.want, meta.GetRuntimeDep(), opts...); diff != "" {
				t.Fatalf("unexpected runtime deps: (-want +got)\n%s", diff)
			}
		})
	}
}
