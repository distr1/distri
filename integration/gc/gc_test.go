package gc_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/env"
)

func verifyStracePresent(tmpdir string) error {
	for _, fn := range []string{
		"strace-amd64-4.24.squashfs",
		"strace-amd64-4.24.meta.textproto",
	} {
		if _, err := os.Stat(filepath.Join(tmpdir, "roimg", fn)); err != nil {
			return err
		}
	}
	return nil
}

func gc(tmpdir string) error {
	distrigc := exec.Command("distri", "gc", "-root="+tmpdir)
	distrigc.Stderr = os.Stderr
	if err := distrigc.Run(); err != nil {
		return fmt.Errorf("%v: %v", distrigc.Args, err)
	}
	return nil
}

func TestGC(t *testing.T) {
	ctx, canc := context.WithCancel(context.Background())
	defer canc()

	tmpdir, err := ioutil.TempDir("", "distrigc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	pkgset := filepath.Join(tmpdir, "etc", "distri", "pkgset.d", "zkj-diag.pkgset")
	if err := os.MkdirAll(filepath.Dir(pkgset), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(pkgset, []byte("strace\n"), 0644); err != nil {
		t.Fatal(err)
	}

	addr, cleanup, err := distritest.Export(ctx, env.DefaultRepo)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	update := exec.Command("distri", "update", "-repo=http://"+addr, "-root="+tmpdir)
	update.Stderr = os.Stderr
	if err := update.Run(); err != nil {
		t.Fatalf("%v: %v", update.Args, err)
	}

	if err := verifyStracePresent(tmpdir); err != nil {
		t.Error(err)
	}

	t.Run("strace present after gc", func(t *testing.T) {
		if err := gc(tmpdir); err != nil {
			t.Fatal(err)
		}
		if err := verifyStracePresent(tmpdir); err != nil {
			t.Error(err)
		}
	})

	t.Run("strace present after gc without pkgset", func(t *testing.T) {
		if err := os.Remove(pkgset); err != nil {
			t.Fatal(err)
		}
		if err := gc(tmpdir); err != nil {
			t.Fatal(err)
		}
		if err := verifyStracePresent(tmpdir); err == nil {
			t.Errorf("strace unexpectedly still present after distri gc")
		}
	})
}
