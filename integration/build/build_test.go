package build_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"text/template"
	"time"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

var buildTextprotoTmpl = template.Must(template.New("").Parse(`
source: "{{ .Source }}"
hash: "{{ .Hash }}"
version: "1"

dep: "bash-amd64-4.4.18"

runtime_dep: "pkg-config-amd64-0.29.2"

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "exit 0"
>
`))

var unversionedBuildTextprotoTmpl = template.Must(template.New("").Parse(`
source: "{{ .Source }}"
hash: "{{ .Hash }}"
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
`))

// TODO: refactor out of build.go
func resolve1(imgDir, pkg string) ([]string, error) {
	resolved := []string{pkg}
	meta, err := pb.ReadMetaFile(filepath.Join(imgDir, pkg+".meta.textproto"))
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

func emptyArchive() ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestBuild(t *testing.T) {
	t.Parallel()

	// Serve upstream source tarball via HTTP:
	empty, err := emptyArchive()
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(empty)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "empty.tar.gz", time.Time{}, bytes.NewReader(empty))
	}))
	defer srv.Close()
	source := srv.URL + "/empty.tar.gz"
	hash := fmt.Sprintf("%x", h.Sum(nil))

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
	var buf bytes.Buffer
	if err := buildTextprotoTmpl.Execute(&buf, struct {
		Source string
		Hash   string
	}{
		Source: source,
		Hash:   hash,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(pkgDir, "build.textproto"), buf.Bytes(), 0644); err != nil {
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
			"pam-amd64-1.3.1",         // from util-linux-2.32
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

	// Serve upstream source tarball via HTTP:
	empty, err := emptyArchive()
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(empty)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "empty.tar.gz", time.Time{}, bytes.NewReader(empty))
	}))
	defer srv.Close()
	source := srv.URL + "/empty.tar.gz"
	hash := fmt.Sprintf("%x", h.Sum(nil))

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
	var buf bytes.Buffer
	if err := unversionedBuildTextprotoTmpl.Execute(&buf, struct {
		Source string
		Hash   string
	}{
		Source: source,
		Hash:   hash,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(pkgDir, "build.textproto"), buf.Bytes(), 0644); err != nil {
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
			"pam-amd64-1.3.1",         // from util-linux-2.32
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
