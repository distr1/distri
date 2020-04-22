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
	"strconv"
	"strings"
	"time"

	"github.com/distr1/distri/internal/install"
	"github.com/distr1/distri/pb"
	"golang.org/x/xerrors"
)

const updateHelp = `distri update [-flags]

Update installed packages.

Example:
  % distri update
`

func update(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("update", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")

		repo   = fset.String("repo", "", "repository from which to install packages from. path (default TODO) or HTTP URL (e.g. TODO)")
		pkgset = fset.String("pkgset", "", "if non-empty, a package set to update")
	)
	fset.Usage = usage(fset, updateHelp)
	fset.Parse(args)

	updateStart := time.Now()
	if v, err := strconv.ParseInt(os.Getenv("UPDATE_START"), 0, 64); err == nil {
		updateStart = time.Unix(v, 0)
	}

	if os.Getenv("DISTRI_REEXEC") != "1" {
		if err := persistFileListing(fileListingFileName(*root, updateStart, "files.before.txt"), filepath.Join(*root, "roimg")); err != nil {
			return err
		}

		c := &install.Ctx{}
		if err := c.Packages([]string{"distri1"}, *root, *repo+"/pkg", false); err != nil {
			return err
		}

		cmd := exec.Command(os.Args[0], append([]string{"update"}, args...)...)
		log.Printf("re-executing %v", cmd.Args)
		// TODO: clean the environment
		cmd.Env = append(os.Environ(),
			"DISTRI_REEXEC=1",
			fmt.Sprintf("UPDATE_START=%d", updateStart.Unix()))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return xerrors.Errorf("%v: %v", cmd.Args, err)
		}
		return nil
	}

	c := &install.Ctx{}
	if err := c.Packages([]string{"base"}, *root, *repo+"/pkg", false); err != nil {
		return err
	}

	var pkgs []string
	if *pkgset != "" {
		b, err := ioutil.ReadFile(filepath.Join(*root, "etc", "distri", "pkgset.d", *pkgset+".pkgset"))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			pkgs = append(pkgs, line)
		}
	} else {
		// find all packages present on the system
		fis, err := ioutil.ReadDir(filepath.Join(*root, "roimg"))
		if err != nil {
			return err
		}
		for _, fi := range fis {
			if !strings.HasSuffix(fi.Name(), ".meta.textproto") {
				continue
			}
			m, err := pb.ReadMetaFile(filepath.Join(*root, "roimg", fi.Name()))
			if err != nil {
				return err
			}
			pkgs = append(pkgs, m.GetSourcePkg())
		}
	}

	if len(pkgs) == 0 {
		return nil
	}

	c = &install.Ctx{}
	if err := c.Packages(pkgs, *root, *repo+"/pkg", true); err != nil {
		// try to persist an after file listing (best effort)
		if err := persistFileListing(fileListingFileName(*root, updateStart, "files.after.txt"), filepath.Join(*root, "roimg")); err != nil {
			log.Println(err)
		}
		return err
	}

	if err := persistFileListing(fileListingFileName(*root, updateStart, "files.after.txt"), filepath.Join(*root, "roimg")); err != nil {
		return err
	}

	return nil
}

func fileListingFileName(root string, timestamp time.Time, basename string) string {
	return filepath.Join(root, "var", "log", "distri", fmt.Sprintf("update-%v", timestamp.Unix()), basename)
}

func persistFileListing(destfn string, dir string) error {
	if err := os.MkdirAll(filepath.Dir(destfn), 0755); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ioutil.WriteFile(destfn, nil, 0644)
		}
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(destfn, []byte(strings.Join(names, "\n")+"\n"), 0644)
}
