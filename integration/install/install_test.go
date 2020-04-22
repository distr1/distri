package install_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
)

const (
	systemd = "systemd-amd64-245-11"
	bash    = "bash-amd64-5.0-4"
)

func installFile(ctx context.Context, tmpdir string, pkg ...string) error {
	install := exec.CommandContext(ctx, "distri",
		append([]string{
			"install",
			"-root=" + tmpdir,
			"-repo=" + env.DefaultRepoRoot,
		}, pkg...)...)
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return fmt.Errorf("%v: %v", install.Args, err)
	}
	return nil
}

func installHTTP(ctx context.Context, tmpdir string, pkg ...string) error {
	addr, cleanup, err := distritest.Export(ctx, env.DefaultRepoRoot)
	if err != nil {
		return err
	}
	defer cleanup()

	install := exec.CommandContext(ctx, "distri",
		append([]string{
			"install",
			"-root=" + tmpdir,
			"-repo=http://" + addr,
		}, pkg...)...)
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return fmt.Errorf("%v: %v", install.Args, err)
	}
	return nil
}

func installHTTPMultiple(ctx context.Context, tmpdir string, pkg ...string) error {
	// Create temporary repos which only hold one package (and its runtime
	// dependencies):
	addrs := make(map[string]string) // pkg → addr
	for _, pkg := range []string{systemd, bash} {
		rtmpdir, err := ioutil.TempDir("", "distritest")
		if err != nil {
			return err
		}
		defer os.RemoveAll(rtmpdir)
		if err := os.Mkdir(filepath.Join(rtmpdir, "pkg"), 0755); err != nil {
			return err
		}
		meta, err := pb.ReadMetaFile(filepath.Join(env.DefaultRepo, pkg+".meta.textproto"))
		if err != nil {
			return err
		}
		for _, dep := range append([]string{pkg}, meta.GetRuntimeDep()...) {
			cp := exec.CommandContext(ctx, "cp",
				filepath.Join(env.DefaultRepo, dep+".squashfs"),
				filepath.Join(env.DefaultRepo, dep+".meta.textproto"),
				filepath.Join(rtmpdir, "pkg"))
			cp.Stderr = os.Stderr
			if err := cp.Run(); err != nil {
				return fmt.Errorf("%v: %v", cp.Args, err)
			}
		}
		addr, cleanup, err := distritest.Export(ctx, rtmpdir)
		if err != nil {
			return err
		}
		defer cleanup()
		addrs[pkg] = addr
	}

	ctmpdir, err := ioutil.TempDir("", "distritest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ctmpdir)
	reposd := filepath.Join(ctmpdir, "repos.d")
	if err := os.Mkdir(reposd, 0755); err != nil {
		return err
	}
	for pkg, addr := range addrs {
		if err := ioutil.WriteFile(filepath.Join(reposd, pkg+".repo"), []byte("http://"+addr), 0644); err != nil {
			return err
		}
	}
	install := exec.CommandContext(ctx, "distri",
		append([]string{
			"install",
			"-root=" + tmpdir,
		}, pkg...)...)
	install.Env = []string{"DISTRICFG=" + ctmpdir}
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return fmt.Errorf("%v: %v", install.Args, err)
	}
	return nil
}

func installHTTPMultipleVersions(ctx context.Context, tmpdir string, pkg ...string) error {
	// Create temporary repos which only hold one package (and its runtime
	// dependencies):
	addrs := make(map[string]string) // pkg → addr
	for _, pkg := range []string{"systemd-amd64-245-11", "systemd-amd64-100"} {
		rtmpdir, err := ioutil.TempDir("", "distritest")
		if err != nil {
			return err
		}
		defer os.RemoveAll(rtmpdir)
		if err := os.Mkdir(filepath.Join(rtmpdir, "pkg"), 0755); err != nil {
			return err
		}
		meta, err := pb.ReadMetaFile(filepath.Join(env.DefaultRepo, systemd+".meta.textproto"))
		if err != nil {
			return err
		}
		// Copy and rename the latest systemd to simulate two versions being present
		for _, dep := range append([]string{systemd}, meta.GetRuntimeDep()...) {
			cp := exec.CommandContext(ctx, "cp",
				filepath.Join(env.DefaultRepo, dep+".squashfs"),
				filepath.Join(env.DefaultRepo, dep+".meta.textproto"),
				filepath.Join(rtmpdir, "pkg"))
			cp.Stderr = os.Stderr
			if err := cp.Run(); err != nil {
				return fmt.Errorf("%v: %v", cp.Args, err)
			}
		}
		const sep = "-amd64-" // TODO: don’t hardcode amd64
		idx := strings.Index(pkg, sep)
		base, version := pkg[:idx+len(sep)-1], pkg[idx+len(sep):]
		for _, suffix := range []string{"squashfs", "meta.textproto"} {
			if err := os.Rename(
				filepath.Join(rtmpdir, "pkg", "systemd-amd64-245-11."+suffix),
				filepath.Join(rtmpdir, "pkg", pkg+"."+suffix)); err != nil {
				return err
			}
			if err := os.Symlink(pkg+"."+suffix, filepath.Join(rtmpdir, "pkg", base+"."+suffix)); err != nil {
				return err
			}
		}

		metaFn := filepath.Join(rtmpdir, "pkg", pkg+".meta.textproto")
		pm, err := pb.ReadMetaFile(metaFn)
		if err != nil {
			return err
		}
		pm.Version = proto.String(version)
		if err := ioutil.WriteFile(metaFn, []byte(proto.MarshalTextString(pm)), 0644); err != nil {
			return err
		}
		addr, cleanup, err := distritest.Export(ctx, rtmpdir)
		if err != nil {
			return err
		}
		defer cleanup()
		addrs[pkg] = addr
	}

	ctmpdir, err := ioutil.TempDir("", "distritest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ctmpdir)
	reposd := filepath.Join(ctmpdir, "repos.d")
	if err := os.Mkdir(reposd, 0755); err != nil {
		return err
	}
	for pkg, addr := range addrs {
		if err := ioutil.WriteFile(filepath.Join(reposd, pkg+".repo"), []byte("http://"+addr), 0644); err != nil {
			return err
		}
	}
	install := exec.CommandContext(ctx, "distri",
		append([]string{
			"install",
			"-root=" + tmpdir,
		}, pkg...)...)
	install.Env = []string{"DISTRICFG=" + ctmpdir}
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return fmt.Errorf("%v: %v", install.Args, err)
	}
	return nil
}

func TestInstall(t *testing.T) {
	t.Parallel()
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	// Wrap the parallel subtests in a group so that control flow (i.e. context
	// cancellation) blocks until all parallel subtests returned.
	t.Run("group", func(t *testing.T) {
		for _, tt := range []struct {
			desc        string
			installFunc func(ctx context.Context, tmpdir string, pkg ...string) error
			pkgsFull    []string
			pkgs        []string
		}{
			{
				desc:        "File",
				installFunc: installFile,
				pkgsFull:    []string{systemd},
				pkgs:        []string{systemd},
			},

			{
				desc:        "HTTP",
				installFunc: installHTTP,
				pkgsFull:    []string{systemd},
				pkgs:        []string{systemd},
			},

			{
				desc:        "HTTPResolveVersion",
				installFunc: installHTTP,
				pkgsFull:    []string{systemd},
				pkgs:        []string{"systemd-amd64"},
			},

			{
				desc:        "HTTPResolveAll",
				installFunc: installHTTP,
				pkgsFull:    []string{systemd},
				pkgs:        []string{"systemd"},
			},

			{
				desc:        "HTTPMultiplePkgs",
				installFunc: installHTTPMultiple,
				pkgsFull:    []string{systemd, bash},
				pkgs:        []string{systemd, bash},
			},

			{
				desc:        "HTTPMultipleVersions",
				installFunc: installHTTPMultipleVersions,
				pkgsFull:    []string{systemd},
				pkgs:        []string{"systemd"},
			},
		} {
			tt := tt // copy
			t.Run(tt.desc, func(t *testing.T) {
				t.Parallel()
				ctx, canc := context.WithCancel(ctx)
				defer canc()

				// install a package from DISTRIROOT/build/distri/pkg to a temporary directory
				tmpdir, err := ioutil.TempDir("", "integrationinstall")
				if err != nil {
					t.Fatal(err)
				}
				defer distritest.RemoveAll(t, tmpdir)

				if err := tt.installFunc(ctx, tmpdir, tt.pkgs...); err != nil {
					t.Fatal(err)
				}

				for _, pkg := range tt.pkgsFull {
					m, err := pb.ReadMetaFile(filepath.Join(env.DefaultRepo, pkg+".meta.textproto"))
					if err != nil {
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
	})
}

func TestInstallHooks(t *testing.T) {
	t.Parallel()
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	// install a package from DISTRIROOT/build/distri/pkg to a temporary directory
	tmpdir, err := ioutil.TempDir("", "integrationinstall")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, tmpdir)

	// TODO: refactor user namespace setup code (like in build.go) so that we
	// can test install with -root=/ (within the namespace) and test the linux
	// hook’s grub-mkconfig call.

	t.Run("distri1", func(t *testing.T) {
		if err := installFile(ctx, tmpdir, "distri1"); err != nil {
			t.Fatal(err)
		}

		initfn := filepath.Join(tmpdir, "init")
		st, err := os.Stat(initfn)
		if err != nil {
			t.Fatalf("distri1 hook not active: %v", err)
		}
		if got, want := st.Mode()&0755, os.FileMode(0755); got != want {
			t.Fatalf("unexpected file mode: got %v, want %v", got, want)
		}

		// Clobber the file
		if err := ioutil.WriteFile(initfn, nil, 0644); err != nil {
			t.Fatal(err)
		}

		// Remove the package so that it will be installed again
		if err := os.RemoveAll(filepath.Join(tmpdir, "roimg")); err != nil {
			t.Fatal(err)
		}
		if err := installFile(ctx, tmpdir, "distri1"); err != nil {
			t.Fatal(err)
		}

		st, err = os.Stat(initfn)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() == 0 {
			t.Fatalf("%s not updated: still clobbered", initfn)
		}
	})

	t.Run("linux", func(t *testing.T) {
		target, err := os.Readlink(filepath.Join(env.DefaultRepo, "linux-amd64.meta.textproto"))
		if err != nil {
			t.Fatal(err)
		}
		pv := distri.ParseVersion(target)
		version := fmt.Sprintf("%s-%d", pv.Upstream, pv.DistriRevision)
		if err := installFile(ctx, tmpdir, "linux"); err != nil {
			t.Fatal(err)
		}

		vmlinuz := filepath.Join(tmpdir, "boot", "vmlinuz-"+version)
		if _, err := os.Stat(vmlinuz); err != nil {
			t.Fatalf("linux hook not active: %v", err)
		}
	})
}

func TestInstallContentHooks(t *testing.T) {
	// Not marked t.Parallel(): uses os.Setenv to modify PATH
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	// install a package from DISTRIROOT/build/distri/pkg to a temporary directory
	tmpdir, err := ioutil.TempDir("", "integrationinstall")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, tmpdir)

	called := filepath.Join(tmpdir, "called")

	if err := ioutil.WriteFile(filepath.Join(tmpdir, "systemd-sysusers"), []byte("#!/bin/sh\ntouch "+called), 0755); err != nil {
		t.Fatal(err)
	}

	os.Setenv("PATH", tmpdir+":"+os.Getenv("PATH"))

	if err := installFile(ctx, tmpdir, "distri1"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(called); err == nil {
		t.Fatalf("systemd-sysusers unexpectedly called for distri1")
	}

	if err := installFile(ctx, tmpdir, "systemd"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(called); err != nil {
		t.Fatalf("systemd-sysusers not called for systemd")
	}
}

func BenchmarkInstallChrome(b *testing.B) {
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	addr, cleanup, err := distritest.Export(ctx, env.DefaultRepoRoot)
	if err != nil {
		b.Fatal(err)
	}
	defer cleanup()
	b.SetBytes(950232307) // TODO: find
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tmpdir, err := ioutil.TempDir("", "integrationinstall")
		if err != nil {
			b.Fatal(err)
		}
		defer distritest.RemoveAll(b, tmpdir)

		install := exec.CommandContext(ctx, "distri",
			append([]string{
				"install",
				"-root=" + tmpdir,
				"-repo=http://" + addr,
			}, "google-chrome")...)
		install.Stderr = os.Stderr
		install.Stdout = os.Stdout
		if err := install.Run(); err != nil {
			b.Fatalf("%v: %v", install.Args, err)
		}
	}
}
