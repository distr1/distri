package install_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
)

func installFile(ctx context.Context, tmpdir, pkg string) (_ error, cleanup func()) {
	install := exec.Command("distri",
		"install",
		"-root="+tmpdir,
		"-repo="+env.DefaultRepo,
		pkg)
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return fmt.Errorf("%v: %v", install.Args, err), nil
	}
	return nil, func() {}
}

func installHTTP(ctx context.Context, tmpdir, pkg string) (_ error, cleanup func()) {
	export := exec.CommandContext(ctx, "distri",
		"-addrfd=3", // Go dup2()s ExtraFiles to 3 and onwards
		"export",
		"-listen=localhost:0",
		// Disable gzip: the gzipped.FileServer package is already tested, and
		// uncompressing these files makes the test run significantly slower.
		"-gzip=false",
	)
	r, w, err := os.Pipe()
	export.Stderr = os.Stderr
	export.Stdout = os.Stdout
	export.ExtraFiles = []*os.File{w}
	if err := export.Start(); err != nil {
		return fmt.Errorf("%v: %v", export.Args, err), nil
	}

	// Close the write end of the pipe in the parent process.
	if err := w.Close(); err != nil {
		return err, nil
	}

	// Read the listening address from the pipe. A successful read also serves
	// as readiness notification.
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return err, nil
	}
	addr := string(b)

	install := exec.Command("distri",
		"install",
		"-root="+tmpdir,
		"-repo=http://"+addr,
		pkg)
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return fmt.Errorf("%v: %v", install.Args, err), nil
	}
	return nil, func() {
		export.Process.Kill()
		export.Wait()
	}
}

func TestInstall(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		desc        string
		installFunc func(ctx context.Context, tmpdir, pkg string) (_ error, cleanup func())
	}{
		{"File", installFile},
		{"HTTP", installHTTP},
	} {
		tt := tt // copy
		t.Run(tt.desc, func(t *testing.T) {
			t.Parallel()
			ctx, canc := context.WithCancel(context.Background())
			defer canc()

			// install a package from DISTRIROOT/build/distri/pkg to a temporary directory
			tmpdir, err := ioutil.TempDir("", "integrationinstall")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpdir)

			const pkg = "systemd-amd64-239"

			err, cleanup := tt.installFunc(ctx, tmpdir, pkg)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			b, err := ioutil.ReadFile(filepath.Join(env.DefaultRepo, pkg+".meta.textproto"))
			if err != nil {
				t.Fatal(err)
			}
			var m pb.Meta
			if err := proto.UnmarshalText(string(b), &m); err != nil {
				t.Fatal(err)
			}

			for _, pkg := range append([]string{pkg}, m.GetRuntimeDep()...) {
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
		})
	}
}
