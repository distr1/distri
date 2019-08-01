package main

import (
	"log"
	"os"
	"os/exec"

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
	// TODO: need manual cycle break in gcc-libs
	// sed -i '/gcc-libs-amd64-8.2.0-1/d' gcc-libs-amd64-8.2.0-2.meta.textproto
	// sed -i '/mpfr-amd64-4.0.1-1/d' gcc-libs-amd64-8.2.0-2.meta.textproto
	// sed -i '/mpc-amd64-1.1.0-1/d' gcc-libs-amd64-8.2.0-2.meta.textproto
	// sed -i '/glibc-amd64-2.27-1/d' gcc-libs-amd64-8.2.0-2.meta.textproto
	// sed -i '/gmp-amd64-6.1.2-1/d' gcc-libs-amd64-8.2.0-2.meta.textproto

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

	// group 3
	{buildPkgArgv("file")},     // for glibc, zlib
	{buildPkgArgv("python3")},  // for openssl, zlib, libffi
	{buildPkgArgv("glib")},     // for util-linux
	{buildPkgArgv("elfutils")}, // for glibc, zlib
	{buildPkgArgv("patchelf")}, // for glibc
	{buildPkgArgv("ninja")},
	{buildPkgArgv("json-c")},       // for systemd
	{buildPkgArgv("libgcrypt")},    // for systemd
	{buildPkgArgv("libgpg-error")}, // for systemd
	{buildPkgArgv("libaio")},       // for lvm2

	// group 4
	{buildPkgArgv("meson")},
	{buildPkgArgv("systemd")},
	{buildPkgArgv("lvm2")},
	{buildPkgArgv("cryptsetup")},
}

func bootstrapFrom(old string) error {
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
		"libgcrypt",
		"libgpg-error",

		"lvm2",
		"json-c",
		"libaio",
	}

	// TODO: copy package meta+squashfs+link from <bootstrap_from>
	// (not using install because it installs into roimg/ and
	//  copies config files)
	_ = packageSet

	for _, step := range bootstrapSteps {
		s := exec.Command(step.argv[0], step.argv[1:]...)
		s.Stdout = os.Stdout
		s.Stderr = os.Stderr
		if err := s.Run(); err != nil {
			return xerrors.Errorf("%v: %v", s.Args, err)
		}
	}

	return nil
}
