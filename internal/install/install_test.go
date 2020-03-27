package install_test

import (
	"bytes"
	"io/ioutil"
	"os"
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
	var buf bytes.Buffer
	c := &install.Ctx{
		HookDryRun: &buf,
	}
	if err := c.Packages([]string{"linux"}, tmpdir, env.DefaultRepoRoot, false /* update */); err != nil {
		t.Fatal(err)
	}
	distri.RunAtExit()
	want := `[sh -c dracut --force /boot/initramfs-5.5.2-12.img 5.5.2]
[/etc/update-grub]
`
	got := buf.String()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("hooks: unexpected commands: diff (-want +got):\n%s", diff)
	}
}
