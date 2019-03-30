package build_test

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

const buildTextproto = `
source: "empty://"
hash: ""
version: "1"

dep: "bash-amd64-4.4.18"

runtime_dep: "pkg-config-amd64-0.29.2"

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "exit 0"
>
`

const unversionedBuildTextproto = `
source: "empty://"
hash: ""
version: "1"

dep: "bash"
# linux is a good test because linux-firmware is an easy false-positive
dep: "linux"

runtime_dep: "pkg-config"

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "exit 0"
>
`

const multiPackageBuildTextproto = `
source: "empty://"
hash: ""
version: "1"

dep: "bash"
dep: "coreutils"

split_package: <
  name: "multi-libs"
  runtime_dep: "bash"
  claim: < glob: "out/lib/*.so" >
>

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/lib; mkdir -p $d; touch $d/liba.so"
>

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/share/doc/multi; mkdir -p $d; touch $d/README.md"
>
`

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

// TODO: refactor out of build.go
func resolve1(imgDir, pkg string) ([]string, error) {
	const ext = ".meta.textproto"
	resolved := []string{pkg}
	fn := filepath.Join(imgDir, pkg+ext)
	if target, err := os.Readlink(fn); err == nil {
		resolved = append(resolved, strings.TrimSuffix(filepath.Base(target), ext))
	}
	meta, err := pb.ReadMetaFile(fn)
	if err != nil {
		return nil, err
	}
	for _, dep := range meta.GetRuntimeDep() {
		if dep == pkg {
			continue // skip circular dependencies, e.g. gcc depends on itself
		}
		resolved = append(resolved, dep)
	}
	return resolved, nil
}

func resolve(imgDir string, pkgs []string) ([]string, error) {
	deps := make(map[string]bool)
	for _, pkg := range pkgs {
		r, err := resolve1(imgDir, pkg)
		if err != nil {
			return nil, err
		}
		for _, dep := range r {
			deps[dep] = true
		}
	}
	resolved := make([]string, 0, len(deps))
	for dep := range deps {
		resolved = append(resolved, dep)
	}
	return resolved, nil
}

func TestBuild(t *testing.T) {
	t.Parallel()

	distriroot, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(distriroot)

	// Copy build dependencies into our temporary DISTRIROOT:
	repo := filepath.Join(distriroot, "build", "distri", "pkg")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	deps, err := resolve(env.DefaultRepo, []string{
		"bash-amd64-4.4.18",
		"pkg-config-amd64-0.29.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps {
		cp := exec.Command("cp",
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

	build := exec.Command("distri", "build")
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
			"pkg-config-amd64-0.29.2", // from hello-1 (direct)
			"glib-amd64-2.58.0",       // from pkg-config (transitive)
			"glibc-amd64-2.27",        // from glib-2.58.0
			"zlib-amd64-1.2.11",       // from glib-2.58.0
			"util-linux-amd64-2.32",   // from glib-2.58.0
			"pam-amd64-1.3.1-7",       // from util-linux-2.32
			"libffi-amd64-3.2.1",      // from glib-2.58.0
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

func TestUnversionedBuild(t *testing.T) {
	t.Parallel()

	distriroot, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(distriroot)

	// Copy build dependencies into our temporary DISTRIROOT:
	repo := filepath.Join(distriroot, "build", "distri", "pkg")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	deps, err := resolve(env.DefaultRepo, []string{
		"bash-amd64-4.4.18",
		"linux-amd64-4.18.7",
		"linux-firmware-amd64-20181104",
		"pkg-config-amd64-0.29.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps {
		cp := exec.Command("cp",
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
		[]byte(unversionedBuildTextproto),
		0644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("distri", "build")
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
			"pkg-config-amd64-0.29.2", // from hello-1 (direct)
			"glib-amd64-2.58.0",       // from pkg-config (transitive)
			"glibc-amd64-2.27",        // from glib-2.58.0
			"zlib-amd64-1.2.11",       // from glib-2.58.0
			"util-linux-amd64-2.32",   // from glib-2.58.0
			"pam-amd64-1.3.1-7",       // from util-linux-2.32
			"libffi-amd64-3.2.1",      // from glib-2.58.0
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

func TestMultiPackageBuild(t *testing.T) {
	t.Parallel()

	distriroot, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(distriroot)

	// Copy build dependencies into our temporary DISTRIROOT:
	repo := filepath.Join(distriroot, "build", "distri", "pkg")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	deps, err := resolve(env.DefaultRepo, []string{
		"bash-amd64-4.4.18",
		"coreutils-amd64-8.30",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps {
		cp := exec.Command("cp",
			filepath.Join(env.DefaultRepo, dep+".squashfs"),
			filepath.Join(env.DefaultRepo, dep+".meta.textproto"),
			repo)
		cp.Stderr = os.Stderr
		if err := cp.Run(); err != nil {
			t.Fatalf("%v: %v", cp.Args, err)
		}
	}

	// Write package build instructions:
	pkgDir := filepath.Join(distriroot, "pkg", "multi")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "build.textproto"),
		[]byte(multiPackageBuildTextproto),
		0644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("distri", "build")
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
		image string
		want  []string
	}{
		{
			image: "multi-amd64-1.squashfs",
			want: []string{
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
			f, err := os.Open(filepath.Join(distriroot, "build", "distri", "pkg", test.image))
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
		})
	}

	for _, test := range []struct {
		meta string
		want []string
	}{
		{
			meta: "multi-amd64-1.meta.textproto",
			want: nil,
		},

		{
			meta: "multi-libs-amd64-1.meta.textproto",
			want: []string{
				"bash-amd64-4.4.18",
				"glibc-amd64-2.27", // from bash
			},
		},
	} {
		test := test // copy
		t.Run("VerifyRuntimeDep/"+test.meta, func(t *testing.T) {
			meta, err := pb.ReadMetaFile(filepath.Join(distriroot, "build", "distri", "pkg", test.meta))
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

func TestPkgConfigRuntimeDeps(t *testing.T) {
	t.Parallel()

	distriroot, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(distriroot)

	// Copy build dependencies into our temporary DISTRIROOT:
	repo := filepath.Join(distriroot, "build", "distri", "pkg")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	deps, err := resolve(env.DefaultRepo, []string{
		//"mesa-amd64-18.2.0",
		"bash-amd64-4.4.18",
		"coreutils-amd64-8.30",
		"libepoxy-amd64-1.5.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps {
		cp := exec.Command("cp",
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

	build := exec.Command("distri", "build")
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
				"glibc-amd64-2.27",     // from shlibdeps
				"libepoxy-amd64-1.5.2", // from pkgconfig
			},
		},
	} {
		test := test // copy
		t.Run("VerifyRuntimeDep/"+test.meta, func(t *testing.T) {
			meta, err := pb.ReadMetaFile(filepath.Join(distriroot, "build", "distri", "pkg", test.meta))
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
