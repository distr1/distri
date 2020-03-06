package gc_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/distritest"
)

func stracePresent(store, pkg string) bool {
	for _, suffix := range []string{
		".squashfs",
		".meta.textproto",
	} {
		if _, err := os.Stat(filepath.Join(store, pkg+suffix)); err != nil {
			return false
		}
	}
	return true
}

func gc(ctx context.Context, tmpdir string) error {
	distrigc := exec.CommandContext(ctx, "distri", "gc", "-store="+tmpdir)
	distrigc.Stderr = os.Stderr
	if err := distrigc.Run(); err != nil {
		return fmt.Errorf("%v: %v", distrigc.Args, err)
	}
	return nil
}

func TestGC(t *testing.T) {
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	store, err := ioutil.TempDir("", "distrigc")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, store)

	const (
		oldPkg = "strace-amd64-5.1-4"
		newPkg = "strace-amd64-5.1-5"
	)

	setup := func() {
		for _, fn := range []string{
			oldPkg + ".squashfs",
			oldPkg + ".meta.textproto",
			newPkg + ".squashfs",
			newPkg + ".meta.textproto",
		} {
			if err := ioutil.WriteFile(filepath.Join(store, fn), nil, 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("MostRecent", func(t *testing.T) {
		setup()

		if !stracePresent(store, oldPkg) || !stracePresent(store, newPkg) {
			t.Error("BUG: strace package not present after creation")
		}

		if err := gc(ctx, store); err != nil {
			t.Fatal(err)
		}

		if !stracePresent(store, newPkg) {
			t.Errorf("gc unexpectedly deleted new version %s", newPkg)
		}

		if stracePresent(store, oldPkg) {
			t.Errorf("gc unexpectedly did not delete old version %s", oldPkg)
		}
	})

	t.Run("Referenced", func(t *testing.T) {
		setup()

		if err := ioutil.WriteFile(filepath.Join(store, "i3status-amd64-2.13-3.squashfs"), nil, 0644); err != nil {
			t.Fatal(err)
		}

		if err := ioutil.WriteFile(filepath.Join(store, "i3status-amd64-2.13-3.meta.textproto"), []byte(`runtime_dep: "`+oldPkg+`"`), 0644); err != nil {
			t.Fatal(err)
		}

		if !stracePresent(store, oldPkg) || !stracePresent(store, newPkg) {
			t.Error("BUG: strace package not present after creation")
		}

		if err := gc(ctx, store); err != nil {
			t.Fatal(err)
		}

		if !stracePresent(store, newPkg) {
			t.Errorf("gc unexpectedly deleted new version %s", newPkg)
		}

		if !stracePresent(store, oldPkg) {
			t.Errorf("gc unexpectedly deleted old version %s", oldPkg)
		}
	})
}
