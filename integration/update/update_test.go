package update_test

import (
	"context"
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

	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/env"
)

func TestUpdate(t *testing.T) {
	ctx, canc := context.WithCancel(context.Background())
	defer canc()

	tmpdir, err := ioutil.TempDir("", "distriupdate")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	pkgset := filepath.Join(tmpdir, "etc", "distri", "pkgset.d", "extrabase.pkgset")
	if err := os.MkdirAll(filepath.Dir(pkgset), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(pkgset, []byte("base-full\n"), 0644); err != nil {
		t.Fatal(err)
	}

	addr, cleanup, err := distritest.Export(ctx, env.DefaultRepo)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	u, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	distri1deps := map[string]bool{
		"/distri1-amd64.meta.textproto":    true,
		"/glibc-amd64-2.27.squashfs":       true,
		"/glibc-amd64-2.27.meta.textproto": true,
		"/distri1-amd64-1.squashfs":        true,
		"/distri1-amd64-1.meta.textproto":  true,
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
			if got, want := r.URL.Path, "/distri1-amd64.meta.textproto"; got != want {
				t.Errorf("first request = %s, want %v", got, want)
			}
		}
		if strings.HasPrefix(r.URL.Path, "/bash-amd64-") &&
			strings.HasSuffix(r.URL.Path, ".meta.textproto") {
			bash = true
		}
		if strings.HasPrefix(r.URL.Path, "/strace-amd64-") &&
			strings.HasSuffix(r.URL.Path, ".meta.textproto") {
			strace = true
		}

		dir(r)
	}
	proxy := httptest.NewServer(rp)
	defer proxy.Close()
	update := exec.Command("distri", "update", "-repo="+proxy.URL, "-root="+tmpdir)
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
}
