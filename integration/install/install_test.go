package install_test

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stapelberg/zi/internal/env"
)

func TestInstall(t *testing.T) {
	// install a package from DISTRIROOT/build/distri/pkg to a temporary directory
	tmpdir, err := ioutil.TempDir("", "integrationinstall")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	const pkg = "systemd-239"

	install := exec.Command("distri",
		"install",
		"-root="+tmpdir,
		"-store="+filepath.Join(env.DistriRoot, "build", "distri", "pkg"),
		pkg)
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		t.Fatalf("%v: %v", install.Args, err)
	}

	for _, pkg := range []string{
		pkg,
		// TODO: read the following from the .meta.textproto
		"glibc-2.27",
		"libcap-2.25",
		"util-linux-2.32",
		"pam-1.3.1",
		"kmod-25",
	} {
		t.Run("VerifyPackageInstalled/"+pkg, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(tmpdir, "roimg", pkg+".squashfs")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(tmpdir, "roimg", pkg+".meta.textproto")); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("VerifyEtcCopiedSystemd", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(tmpdir, "etc", "systemd", "system.conf")); err != nil {
			t.Fatal(err)
		}
		linkName := filepath.Join(tmpdir, "etc", "xdg", "systemd", "user")
		st, err := os.Lstat(linkName)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s unexpectedly not a symbolic link", linkName)
		}
		target, err := os.Readlink(linkName)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := target, "../../systemd/user"; got != want {
			t.Errorf("unexpected link target: got %q, want %q", got, want)
		}
	})

	t.Run("VerifyEtcCopiedGlibc", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(tmpdir, "etc", "rpc")); err != nil {
			t.Fatal(err)
		}
	})
}
