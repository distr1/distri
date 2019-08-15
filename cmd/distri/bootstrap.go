package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"golang.org/x/xerrors"
)

type bootstrapStep struct {
	argv []string
}

func buildPkgArgv(pkg string) []string {
	return []string{"distri", "build", "-pkg=" + pkg}
}

var bootstrapSteps = []bootstrapStep{
	{buildPkgArgv("gmp")},
	{buildPkgArgv("mpfr")},
	{buildPkgArgv("mpc")},
	{buildPkgArgv("gcc")},
	{buildPkgArgv("gawk")},

	// TODO: parallelize builds within the same group
	// before: distri batch -bootstrap_from=$PWD/O.pkg/ 2>&1  10170,30s user 1774,90s system 350% cpu 56:51,08 total

	// group 1
	{buildPkgArgv("bc")},
	{buildPkgArgv("binutils")},
	{buildPkgArgv("m4")},
	{buildPkgArgv("sed")},
	{buildPkgArgv("zlib")},
	{buildPkgArgv("tar")},
	{buildPkgArgv("diffutils")},
	{buildPkgArgv("findutils")},
	{buildPkgArgv("ed")},
	{buildPkgArgv("texinfo")},
	{buildPkgArgv("grep")},
	{buildPkgArgv("gzip")},
	{buildPkgArgv("kmod")},
	{buildPkgArgv("libffi")},
	{buildPkgArgv("pam")},
	{buildPkgArgv("perl")},
	{buildPkgArgv("flex")},
	{buildPkgArgv("make")},
	{buildPkgArgv("libcap")},
	{buildPkgArgv("attr")},

	// group 2
	{buildPkgArgv("bison")},      // for m4
	{buildPkgArgv("util-linux")}, // for pam
	{buildPkgArgv("openssl")},    // for perl
	{buildPkgArgv("glibc")},      // for zlib
	{buildPkgArgv("coreutils")},  // for attr
	{buildPkgArgv("gperf")},
	{buildPkgArgv("musl")},
	{buildPkgArgv("popt")}, // for cryptsetup

	// group 3
	{buildPkgArgv("file")},     // for glibc, zlib
	{buildPkgArgv("python3")},  // for openssl, zlib, libffi
	{buildPkgArgv("glib")},     // for util-linux
	{buildPkgArgv("elfutils")}, // for glibc, zlib
	{buildPkgArgv("patchelf")}, // for glibc
	{buildPkgArgv("ninja")},
	{buildPkgArgv("autoconf")},     // for json-c
	{buildPkgArgv("json-c")},       // for systemd
	{buildPkgArgv("libgcrypt")},    // for systemd
	{buildPkgArgv("libgpg-error")}, // for systemd
	{buildPkgArgv("cryptsetup")},   // for systemd
	{buildPkgArgv("gettext")},      // for systemd
	{buildPkgArgv("which")},        // for lvm2
	{buildPkgArgv("libaio")},       // for lvm2

	// group 4
	{buildPkgArgv("meson")},
	{buildPkgArgv("systemd")},
	{buildPkgArgv("lvm2")},
	{buildPkgArgv("cryptsetup")},
}

func bootstrapFrom(old string, dryRun bool) error {
	log.Printf("bootstrapping from %s", old)
	packageSet := []string{
		"musl",
		"attr",
		"gcc-libs",
		"bash",
		"bc",
		"binutils",
		"bison",
		"coreutils",
		"diffutils",
		"ed",
		"elfutils",
		"file",
		"findutils",
		"flex",
		"gawk",
		"gcc",
		"glib",
		"glibc",
		"gmp",
		"gperf",
		"grep",
		"gzip",
		"help2man",
		"kmod",
		"libcap",
		"libffi",
		"linux",
		"m4",
		"make",
		"meson",
		"mpc",
		"mpfr",
		"ninja",
		"openssl",
		"pam",
		"patchelf",
		"perl",
		"pkg-config",
		"python3",
		"sed",
		"strace",
		"tar",
		"texinfo",
		"util-linux",
		"zlib",

		"systemd",
		"libudev",
		"libgcrypt",
		"libgpg-error",
		"gettext",    // for systemd
		"popt",       // for cryptsetup
		"cryptsetup", // for systemd

		"lvm2",
		"json-c",
		"libaio",
		"autoconf", // for json-c
		"which",    // for lvm2
	}

	for _, pkg := range packageSet {
		// We are not using buildctx.glob here because we intentionally want to
		// make available all versions (including older ones).
		matches, err := filepath.Glob(filepath.Join(old, pkg+"-amd64-*.squashfs"))
		if err != nil {
			return err
		}
		links := make(map[string]string) // package → version to link
		for _, m := range matches {
			pkg := strings.TrimSuffix(filepath.Base(m), ".squashfs")
			for _, ext := range []string{"meta.textproto", "squashfs"} {
				src := filepath.Join(old, pkg+"."+ext)
				dest := filepath.Join(env.DefaultRepo, pkg+"."+ext)
				if dryRun {
					log.Printf("cp %s %s", src, dest)
					continue
				}
				if err := copyFile(src, dest); err != nil {
					return err
				}
			}
			pv := distri.ParseVersion(pkg)
			oldname := pkg + ".meta.textproto"
			newname := filepath.Join(env.DefaultRepo, pv.Pkg+"-"+pv.Arch+".meta.textproto")
			if cur, ok := links[newname]; !ok || pv.DistriRevision > distri.ParseVersion(cur).DistriRevision {
				links[newname] = oldname
			}
		}
		for newname, oldname := range links {
			if dryRun {
				log.Printf("ln -s %s %s", oldname, newname)
				continue
			}
			if err := os.Symlink(oldname, newname); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}

	for _, step := range bootstrapSteps {
		if dryRun {
			log.Printf("%v", step.argv)
			continue
		}
		s := exec.Command(step.argv[0], step.argv[1:]...)
		s.Stdout = os.Stdout
		s.Stderr = os.Stderr
		if err := s.Run(); err != nil {
			return xerrors.Errorf("%v: %v", s.Args, err)
		}
	}

	return nil
}
