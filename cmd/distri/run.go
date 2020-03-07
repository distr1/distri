package main

import (
	"context"
	"flag"
	"fmt"
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
	"github.com/jacobsa/fuse"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
)

const runHelp = `distri run [-flags] <cmd>

Run a command in a new mount namespace in which a distri package store is
available under /ro.

Requires the distri binary to have capability CAP_SYS_ADMIN.

Example:
  % distri run i3status --version
  % distri run -pkgs=coreutils ls
`

func run(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		pkgs = fset.String("pkgs", "", "comma-separated list of packages to make available in the namespace. defaults to cmd[0]")
	)
	fset.Usage = usage(fset, runHelp)
	fset.Parse(args)

	if fset.NArg() < 1 {
		return xerrors.Errorf("syntax: distri run [-flags] <cmd>")
	}
	cmd := fset.Args()

	p := &build.Ctx{
		Arch: "amd64", // TODO: -cross flag
		Repo: env.DefaultRepo,
	}

	chrootDir, err := ioutil.TempDir("", "distri-patchchroot")
	if err != nil {
		return err
	}
	//defer os.RemoveAll(chrootDir)

	origdir := filepath.Join(chrootDir, "ORIG")
	if err := os.MkdirAll(origdir, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("/", origdir, "none", syscall.MS_BIND /*|syscall.MS_RDONLY*/, ""); err != nil {
		return xerrors.Errorf("mount: %w", err)
	}
	// TODO: don’t run os.RemoveAll if this fails! wipes out homedir
	defer syscall.Unmount(origdir, 0)

	fis, err := ioutil.ReadDir("/")
	if err != nil {
		return err
	}

	for _, fi := range fis {
		if !fi.IsDir() && (fi.Mode()&os.ModeSymlink) == 0 {
			log.Printf("skipping non-dir %q", fi.Name())
			continue
		}
		if fi.Name() == "ORIG" ||
			fi.Name() == "ro" ||
			fi.Name() == "tmp" ||
			fi.Name() == "bin" ||
			fi.Name() == "share" ||
			fi.Name() == "lib" ||
			fi.Name() == "include" ||
			fi.Name() == "sbin" ||
			fi.Name() == "usr" ||
			fi.Name() == "home" /* TODO */ {
			log.Printf("skipping %q", fi.Name())
			continue
		}
		oldname := "/ORIG/" + fi.Name()
		newname := filepath.Join(chrootDir, fi.Name())
		// TODO: should we bind mount /tmp instead of creating our own? would
		// that make things work with file systems that are mounted somewhere
		// underneath e.g. /sys?
		log.Printf("ln %s %s", oldname, newname)
		if err := os.Symlink(oldname, newname); err != nil {
			return err
		}
	}

	type symlink struct {
		oldname, newname string
	}
	for _, link := range []symlink{
		{"/", "usr"},
		{"/ro/bin", "bin"},
		{"/ro/share", "share"},
		{"/ro/lib", "lib"},
		{"/ro/include", "include"},
		{"/ro/sbin", "sbin"},
		{"/init", "entrypoint"},
	} {
		if err := os.Symlink(link.oldname, filepath.Join(chrootDir, link.newname)); err != nil {
			return err
		}
	}

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
	if *pkgs == "" {
		*pkgs = cmd[0]
	}
	deps = append(deps, strings.Split(*pkgs, ",")...)
	deps, err = p.Glob(env.DefaultRepo, deps)
	if err != nil {
		return err
	}

	deps, err = build.Resolve(env.DefaultRepo, deps, "")
	if err != nil {
		return err
	}
	depsdir := filepath.Join(chrootDir, "ro")
	if err := os.MkdirAll(depsdir, 0755); err != nil {
		return err
	}
	ctx, canc := context.WithCancel(context.Background())
	defer canc()
	if _, err := cmdfuse.Mount(ctx, []string{"-overlays=/bin", "-pkgs=" + strings.Join(deps, ","), depsdir}); err != nil {
		return xerrors.Errorf("fuse mount: %w", err)
	}
	defer fuse.Unmount(depsdir)

	const pad = 0
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 1, pad, pad, pad); err != nil {
		return fmt.Errorf("prctl: %v", err)
	}
	{
		os.Setenv("PATH", filepath.Join(depsdir, "bin"))
		lp, err := exec.LookPath(cmd[0])
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(chrootDir, lp)
		if err != nil {
			return err
		}
		cmd := exec.Command("/"+rel, cmd[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:                 unix.CLONE_NEWNS | unix.CLONE_NEWUSER,
			Chroot:                     chrootDir,
			GidMappingsEnableSetgroups: false,
		}
		if err := cmd.Run(); err != nil {
			return xerrors.Errorf("%v: %v", cmd.Args, err)
		}
	}

	return nil
}
