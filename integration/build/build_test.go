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
	"github.com/golang/protobuf/proto"
	"github.com/google/go-cmp/cmp"
)

var buildTextprotoTmpl = template.Must(template.New("").Parse(`
source: "{{ .Source }}"
hash: "{{ .Hash }}"
version: "1"

dep: "bash-4.4.18"

runtime_dep: "pkg-config-0.29.2"

build_step: <
  argv: "/bin/sh"
  argv: "-c"
  argv: "exit 0"
>
`))

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
	for _, dep := range []string{
		"bash-4.4.18",
		// TODO: remove once bash-4.4.18’s runtime_deps are resolved:
		"glibc-2.27",

		"pkg-config-0.29.2",
		// TODO: remove once pkg-config-0.29.2’s runtime_deps are resolved:
		"glib-2.58.0",
		"zlib-1.2.11",
		"util-linux-2.32",
		"pam-1.3.1",
		"libffi-3.2.1",
	} {
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
		var meta pb.Meta
		b, err := ioutil.ReadFile(filepath.Join(distriroot, "build", "distri", "pkg", "test-1.meta.textproto"))
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.UnmarshalText(string(b), &meta); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"pkg-config-0.29.2", // from hello-1 (direct)
			"glib-2.58.0",       // from pkg-config (transitive)
			"glibc-2.27",        // from glib-2.58.0
			"zlib-1.2.11",       // from glib-2.58.0
			"util-linux-2.32",   // from glib-2.58.0
			"pam-1.3.1",         // from util-linux-2.32
			"libffi-3.2.1",      // from glib-2.58.0
		}
		if diff := cmp.Diff(want, meta.GetRuntimeDep()); diff != "" {
			t.Fatalf("unexpected runtime deps: (-want +got)\n%s", diff)
		}
	})
}
