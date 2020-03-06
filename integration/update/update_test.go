package update_test

import (
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/google/go-cmp/cmp"
)

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

func TestUpdate(t *testing.T) {
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	tmpdir, err := ioutil.TempDir("", "distriupdate")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, tmpdir)

	pkgset := filepath.Join(tmpdir, "etc", "distri", "pkgset.d", "extrabase.pkgset")
	if err := os.MkdirAll(filepath.Dir(pkgset), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(pkgset, []byte("base-full\n"), 0644); err != nil {
		t.Fatal(err)
	}

	addr, cleanup, err := distritest.Export(ctx, env.DefaultRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	u, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	distri1deps := make(map[string]bool)
	{
		distri1deps["/pkg/distri1-amd64.meta.textproto"] = true
		deps, err := resolve1(env.DefaultRepo, "distri1-amd64")
		if err != nil {
			t.Fatal(err)
		}
		for _, dep := range deps {
			distri1deps["/pkg/"+dep+".squashfs"] = true
			distri1deps["/pkg/"+dep+".meta.textproto"] = true
		}
	}
	var (
		mu     sync.Mutex
		first  = true
		reexec = true
		bash   = false // from base
		strace = false // from base-full
	)
	// rp.Director is called for each request, so we use it as a hook to verify
	// the correct HTTP calls are made:
	dir := rp.Director
	rp.Director = func(r *http.Request) {
		// verify distri1 is installed first
		mu.Lock()
		defer mu.Unlock()

		// verify all requests but the first distri1 request come from the re-executed distri(1)
		if !distri1deps[r.URL.Path] && r.Header.Get("X-Distri-Reexec") != "yes" {
			log.Printf("non-reexec request for %v", r.URL.Path)
			reexec = false
		}

		if first {
			first = false
			if got, want := r.URL.Path, "/pkg/distri1-amd64.meta.textproto"; got != want {
				t.Errorf("first request = %s, want %v", got, want)
			}
		}
		if strings.HasPrefix(r.URL.Path, "/pkg/bash-amd64-") &&
			strings.HasSuffix(r.URL.Path, ".meta.textproto") {
			bash = true
		}
		if strings.HasPrefix(r.URL.Path, "/pkg/strace-amd64-") &&
			strings.HasSuffix(r.URL.Path, ".meta.textproto") {
			strace = true
		}

		dir(r)
	}
	proxy := httptest.NewServer(rp)
	defer proxy.Close()
	update := exec.CommandContext(ctx, "distri", "update", "-repo="+proxy.URL, "-root="+tmpdir, "-pkgset=extrabase")
	update.Stderr = os.Stderr
	if err := update.Run(); err != nil {
		t.Fatalf("%v: %v", update.Args, err)
	}

	mu.Lock()
	bashUpgraded := bash
	straceUpgraded := strace
	allReexec := reexec
	mu.Unlock()
	if !bashUpgraded {
		t.Errorf("bash was not upgraded (via base)")
	}
	if !allReexec {
		t.Errorf("distri did not re-exec after updating distri1")
	}
	if !straceUpgraded {
		t.Errorf("strace was not upgraded (via base-full)")
	}

	t.Run("VerifyBeforeAfterLog", func(t *testing.T) {
		matches, err := filepath.Glob(filepath.Join(tmpdir, "var", "log", "distri", "*"))
		if err != nil {
			t.Fatal(err)
		}
		updateDir := matches[0]

		b, err := ioutil.ReadFile(filepath.Join(updateDir, "files.before.txt"))
		if err != nil {
			t.Fatal(err)
		}
		got := string(b)
		if diff := cmp.Diff("", got); diff != "" {
			t.Errorf("files.before.txt: unexpected content: diff (-want +got):\n%s", diff)
		}

		b, err = ioutil.ReadFile(filepath.Join(updateDir, "files.after.txt"))
		if err != nil {
			t.Fatal(err)
		}
		got = string(b)
		if diff := cmp.Diff("", got); diff == "" {
			t.Errorf("files.after.txt: unexpectedly empty")
		}
	})
}
