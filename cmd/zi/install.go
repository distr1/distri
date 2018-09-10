package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

func install(args []string) error {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/ro",
			"TODO")

		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Parse(args)
	if fset.NArg() < 1 {
		return fmt.Errorf("syntax: install <package> [<package>...]")
	}

	// TODO: obtain package from configured mirror
	for _, pkg := range fset.Args() {
		log.Printf("installing package %q to root %s", pkg, *root)
		// TODO(later): unpack in pure Go to get rid of the tar dependency
		//cmd := exec.Command("tar", "xf", filepath.Join(os.Getenv("HOME"), "zi/build/zi/pkg/", pkg+".tar.gz"), "--no-same-owner")
		cmd := exec.Command("unsquashfs", "-force", "-d", filepath.Join(*root, pkg), filepath.Join(os.Getenv("HOME"), "zi/build/zi/pkg/", pkg+".squashfs"))
		cmd.Dir = *root
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%v: %v", cmd.Args, err)
		}

		// Link <root>/<pkg>-<version>/bin/ entries to <root>/bin:
		if err := symlinkfarm(*root, pkg, "bin"); err != nil {
			return err
		}
		if err := symlinkfarm(*root, pkg, "buildoutput/lib/systemd/system"); err != nil {
			return err
		}

		// TODO: read meta.textproto, install runtime dependencies as well
	}
	return nil
}
