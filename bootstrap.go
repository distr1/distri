// +build ignore

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
)

type bootstrapctx struct {
	cross    string
	hermetic bool
}

func (b *bootstrapctx) build(ctx context.Context, pkgs ...string) error {
	for _, pkg := range pkgs {
		log.Printf("building package %s", pkg)
		build := exec.CommandContext(ctx,
			"distri",
			"build",
			"-cross="+b.cross,
			"-hermetic="+strconv.FormatBool(b.hermetic),
			"-pkg="+pkg)
		build.Stdin = os.Stdin
		build.Stdout = os.Stdout
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			return fmt.Errorf("%v: %v", build.Args, err)
		}
	}
	return nil
}

func logic(ctx context.Context) error {
	var b bootstrapctx
	// TODO: de-duplicate the flag help texts with build.go
	// TODO: insert all supported architectures into the flag help text
	flag.StringVar(&b.cross, "cross", "", "If non-empty, cross-build for the specified architecture (e.g. i686)")
	flag.BoolVar(&b.hermetic, "hermetic", true, "build hermetically (if false, host dependencies are used)")
	flag.Parse()
	var (
		glibcSet = []string{"glibc"}
		gccSet   = []string{"gmp", "mpfr", "mpc", "gcc"}
		makeSet  = []string{"make"}
		bashSet  = []string{"bash"}
		restSet  = []string{
			"attr",
			"autoconf",
			"bc",
			"binutils",
			"bison",
			"coreutils",
			"cryptsetup",
			"diffutils",
			"ed",
			"elfutils",
			"file",
			"findutils",
			"flex",
			"gawk",
			"gettext",
			"glib",
			"gperf",
			"grep",
			"gzip",
			"help2man",
			"json-c",
			"kmod",
			"libaio",
			"libcap",
			"libffi",
			"libgcrypt",
			"libgpg-error",
			"rsync",
			"linux",
			"lvm2",
			"m4",
			"meson",
			"musl",
			"ncurses",
			"ninja",
			"openssl",
			"pam",
			"patchelf",
			"pcre",
			"perl",
			"pkg-config",
			"popt",
			"python3",
			"sed",
			"strace",
			"systemd",
			"tar",
			"texinfo",
			"util-linux",
			"which",
			"zlib",
		}
	)
	for _, pkgset := range [][]string{
		glibcSet,
		gccSet,
		makeSet,
		bashSet,
		restSet,
	} {
		if err := b.build(ctx, pkgset...); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	// TODO: cancel-correct context
	if err := logic(context.Background()); err != nil {
		log.Fatal(err)
	}
}
