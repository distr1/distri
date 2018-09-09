package main

import (
	"flag"
	"fmt"
	"io/ioutil"
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
		if err := os.MkdirAll(filepath.Join(*root, "bin"), 0755); err != nil {
			return err
		}
		binDir := filepath.Join(*root, pkg, "bin")
		fis, err := ioutil.ReadDir(binDir)
		if err != nil {
			return err
		}
		for _, fi := range fis {
			oldname := filepath.Join(binDir, fi.Name())
			newname := filepath.Join(*root, "bin", fi.Name())
			tmp, err := ioutil.TempFile(filepath.Dir(newname), "zi")
			if err != nil {
				return err
			}
			tmp.Close()
			if err := os.Remove(tmp.Name()); err != nil {
				return err
			}
			rel, err := filepath.Rel(filepath.Join(*root, "bin"), oldname)
			if err != nil {
				return err
			}
			if err := os.Symlink(rel, tmp.Name()); err != nil {
				return err
			}
			if err := os.Rename(tmp.Name(), newname); err != nil {
				return err
			}
		}

		// TODO: read meta.textproto, install runtime dependencies as well
	}
	return nil
}
