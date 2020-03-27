package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/env"
	cmdfuse "github.com/distr1/distri/internal/fuse"
	"github.com/distr1/distri/pb"
	"github.com/jacobsa/fuse"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
	"google.golang.org/protobuf/encoding/prototext"
)

const patchHelp = `distri patch [-flags]

Interactively create a patch for a package.

distri patch spawns a shell in a temporary directory with the
upstream sources. Any modifications you do to the source will
be persisted into the specified patch file.

Example:
  % distri patch -pkg=i3status fix-build.patch
`

type patchctx struct {
	build.Ctx
}

func (p *patchctx) fullName() string {
	return p.Pkg + "-" + p.Arch + "-" + p.Version
}

func patchJob(job string) error {
	var p patchctx
	if err := json.Unmarshal([]byte(job), &p); err != nil {
		return err
	}

	//log.Printf("(subproc) getuid = %v, effective = %v", unix.Getuid(), unix.Geteuid())

	if err := os.Symlink("/ro/bin", filepath.Join(p.ChrootDir, "/bin")); err != nil {
		return err
	}

	// Set up device nodes under /dev:
	{
		dev := filepath.Join(p.ChrootDir, "dev")
		if err := os.MkdirAll(dev, 0755); err != nil {
			return err
		}
		for _, fn := range []string{"null", "tty"} {
			if err := ioutil.WriteFile(filepath.Join(dev, fn), nil, 0644); err != nil {
				return err
			}
			if err := syscall.Mount(filepath.Join("/dev", fn), filepath.Join(dev, fn), "none", syscall.MS_BIND, ""); err != nil {
				return err
			}
		}
	}

	if err := unix.Chroot(p.ChrootDir); err != nil {
		return xerrors.Errorf("chroot(%s): %v", p.ChrootDir, err)
	}

	if err := os.Chdir(filepath.Join("/usr/src", p.fullName())); err != nil {
		return err
	}

	cmd := exec.Command("/bin/zsh", "-i")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

func patch(ctx context.Context, args []string) error {
	//log.SetFlags(log.LstdFlags | log.Lshortfile)
	fset := flag.NewFlagSet("patch", flag.ExitOnError)
	var (
		pkg = fset.String("pkg", "", "package to patch")
	)
	fset.Usage = usage(fset, patchHelp)
	fset.Parse(args)

	if job := os.Getenv("DISTRI_PATCH_JOB"); job != "" {
		return patchJob(job)
	}

	if fset.NArg() != 1 {
		return xerrors.Errorf("syntax: distri patch [options] <patchfile>")
	}
	patchfile := fset.Arg(0)

	if *pkg == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if pkgs := filepath.Join(env.DistriRoot, "pkgs"); filepath.Dir(wd) != pkgs {
			return xerrors.Errorf("either run distri patch inside of %s or specify -pkg", pkgs)
		}
		*pkg = filepath.Base(wd)
	}
	log.Printf("patching package %s, persisting to %s", *pkg, patchfile)

	buildProtoPath := filepath.Join(env.DistriRoot, "pkgs", *pkg, "build.textproto")
	c, err := ioutil.ReadFile(buildProtoPath)
	if err != nil {
		return err
	}
	var buildProto pb.Build
	if err := prototext.Unmarshal(c, &buildProto); err != nil {
		return xerrors.Errorf("reading %s: %v", buildProtoPath, err)
	}

	p := &patchctx{
		build.Ctx{
			Pkg:     *pkg,
			Arch:    "amd64", // TODO: -cross flag
			Version: buildProto.GetVersion(),
			Proto:   &buildProto,
		},
	}

	if os.Getenv("DISTRI_PATCH_PROCESS") != "1" {
		chrootDir, err := ioutil.TempDir("", "distri-patchchroot")
		if err != nil {
			return err
		}
		defer os.RemoveAll(chrootDir)
		p.ChrootDir = chrootDir

		// Mount overlay file system:
		workdir, err := ioutil.TempDir("", "distri-patch-work")
		if err != nil {
			return err
		}
		defer os.RemoveAll(workdir)
		upperdir, err := ioutil.TempDir("", "distri-patch-upper")
		if err != nil {
			return err
		}
		defer os.RemoveAll(upperdir)
		lowerdir := filepath.Join(env.DistriRoot, "build", p.Pkg, build.TrimArchiveSuffix(filepath.Base(p.Proto.GetSource())))
		target := filepath.Join(p.ChrootDir, "usr", "src", p.fullName())
		if err := os.MkdirAll(target, 0755); err != nil {
			return xerrors.Errorf("MkdirAll(%s) = %v", target, err)
		}
		opts := strings.Join([]string{
			"lowerdir=" + lowerdir,
			"upperdir=" + upperdir,
			"workdir=" + workdir,
		}, ",")
		if err := syscall.Mount("overlay", target, "overlay", 0, opts); err != nil {
			return xerrors.Errorf("mount: %v", err)
		}
		defer syscall.Unmount(target, 0)

		// mount fuse
		deps := []string{
			"bash",
			"coreutils",
			"sed",
			"grep",
			"gawk",
			"emacs",
			"zsh",
			"findutils",
		}
		deps, err = p.Glob(env.DefaultRepo, deps)
		if err != nil {
			return err
		}

		deps, err = build.Resolve(env.DefaultRepo, deps, "")
		if err != nil {
			return err
		}
		depsdir := filepath.Join(p.ChrootDir, "ro")
		if err := os.MkdirAll(depsdir, 0755); err != nil {
			return err
		}
		ctx, canc := context.WithCancel(context.Background())
		defer canc()
		if _, err := cmdfuse.Mount(ctx, []string{"-overlays=/bin", "-pkgs=" + strings.Join(deps, ","), depsdir}); err != nil {
			return err
		}
		defer fuse.Unmount(depsdir)

		enc, err := json.Marshal(p)
		if err != nil {
			return err
		}

		{
			exe, err := os.Readlink("/proc/self/exe")
			if err != nil {
				return err
			}
			atarget := filepath.Join(p.ChrootDir, exe)
			if err := os.MkdirAll(filepath.Dir(atarget), 0755); err != nil {
				return err
			}
			if err := ioutil.WriteFile(atarget, nil, 0644); err != nil {
				return err
			}
			if err := syscall.Mount(exe, atarget, "none", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				return err
			}
		}

		{
			devnull := filepath.Join(p.ChrootDir, "dev", "null")
			if err := os.MkdirAll(filepath.Dir(devnull), 0755); err != nil {
				return err
			}
			if err := ioutil.WriteFile(devnull, nil, 0644); err != nil {
				return err
			}

			if err := syscall.Mount("/dev/null", devnull, "none", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				return err
			}
		}

		// Set up /etc/passwd (required by e.g. python3):
		{
			etc := filepath.Join(p.ChrootDir, "etc")
			if err := os.MkdirAll(etc, 0755); err != nil {
				return err
			}
			if err := ioutil.WriteFile(filepath.Join(etc, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh"), 0644); err != nil {
				return err
			}
			if err := ioutil.WriteFile(filepath.Join(etc, "group"), []byte("root:x:0"), 0644); err != nil {
				return err
			}
			if err := ioutil.WriteFile(filepath.Join(etc, "suid-debug"), []byte("1"), 0644); err != nil {
				return err
			}
		}

		const pad = 0
		unix.Prctl(unix.PR_SET_DUMPABLE, 1, pad, pad, pad)
		cmd := exec.Command(os.Args[0], "patch")
		cmd.Dir = "/"
		// TODO: clean the environment
		cmd.Env = append(os.Environ(), "DISTRI_PATCH_PROCESS=1", "DISTRI_PATCH_JOB="+string(enc))
		cmd.Stdin = os.Stdin // for interactive debugging
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: unix.CLONE_NEWNS | unix.CLONE_NEWUSER,
			// Unshareflags will only work in Go 1.13:
			// https://github.com/golang/go/issues/29789
			// Not sure whether using Unshareflags is any better than
			// Cloneflags, particularly when distri(1) has elevated capabilities.
			// Unshareflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWUSER,
			GidMappingsEnableSetgroups: false,
			UidMappings: []syscall.SysProcIDMap{
				{
					ContainerID: 0,
					HostID:      syscall.Getuid(),
					Size:        1,
				},
			},
			GidMappings: []syscall.SysProcIDMap{
				{
					ContainerID: 0,
					HostID:      syscall.Getgid(),
					Size:        1,
				},
			},
		}
		if err := cmd.Run(); err != nil {
			return xerrors.Errorf("%v: %v", cmd.Args, err)
		}

		// Generate a patch out of the modifications
		tmpdir, err := ioutil.TempDir("", "distri-patch-diff")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpdir)

		if err := os.Symlink(lowerdir, filepath.Join(tmpdir, "old")); err != nil {
			return err
		}
		if err := os.Symlink(upperdir, filepath.Join(tmpdir, "new")); err != nil {
			return err
		}
		var patch bytes.Buffer
		err = filepath.Walk(upperdir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				rel := strings.TrimPrefix(path, upperdir+"/")
				old := "old/" + rel
				if _, err := os.Stat(filepath.Join(tmpdir, old)); err != nil {
					old = "/dev/null"
				}
				diff := exec.Command("diff", "-u", old, "new/"+rel)
				diff.Dir = tmpdir
				diff.Stdout = &patch
				diff.Stderr = os.Stderr
				if err := diff.Run(); err != nil {
					if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
						// files are different, which is what we expect
					} else {
						return xerrors.Errorf("%v: %v", diff.Args, err)
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		fn := filepath.Join(env.DistriRoot, "pkgs", *pkg, patchfile)
		if err := ioutil.WriteFile(fn, patch.Bytes(), 0644); err != nil {
			return err
		}

		return nil
	}

	return nil
}
