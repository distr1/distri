package install_test

import (
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/install"
	"github.com/google/go-cmp/cmp"
)

func TestHooks(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "distritest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	fn := filepath.Join(tmpdir, "etc", "distri", "initramfs-generator")
	if err := os.MkdirAll(filepath.Dir(fn), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(fn, []byte("minitrd\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	c := &install.Ctx{
		HookDryRun: &buf,
	}
	if err := c.Packages([]string{"linux"}, tmpdir, env.DefaultRepo, false /* update */); err != nil {
		t.Fatal(err)
	}
	distri.RunAtExit()
	want := `[sh -c distri initrd -release 5.6.5 -output /boot/initramfs-5.6.5-15.img]
[/etc/update-grub]
`
	got := buf.String()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("hooks: unexpected commands: diff (-want +got):\n%s", diff)
	}
}
