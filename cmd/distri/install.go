package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/stapelberg/zi/internal/squashfs"
	"golang.org/x/exp/mmap"
	"golang.org/x/sync/errgroup"
)

const installHelp = `TODO
`

func unpackDir(dest string, rd *squashfs.Reader, inode squashfs.Inode) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	fis, err := rd.Readdir(inode)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		destName := filepath.Join(dest, fi.Name())
		fileInode := fi.Sys().(*squashfs.FileInfo).Inode
		if fi.IsDir() {
			if err := unpackDir(destName, rd, fileInode); err != nil {
				return err
			}
		} else if fi.Mode()&os.ModeSymlink > 0 {
			target, err := rd.ReadLink(fileInode)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, destName); err != nil {
				// TODO: if exists, re-create if target differs
				return err
			}
		} else if fi.Mode().IsRegular() {
			fr, err := rd.FileReader(fileInode)
			if err != nil {
				return err
			}
			f, err := os.OpenFile(destName, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, fr); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		} else {
			log.Printf("ERROR: unsupported SquashFS file type: %+v", fi.Mode())
		}
	}
	return nil
}

func install1(root, store, pkg string, first bool) error {
	// TODO: http download
	// resp, err := http.Get(file)
	// if err != nil {
	// 	return err
	// }
	// defer resp.Body.Close()
	// if got, want := resp.StatusCode, http.StatusOK; got != want {
	// 	return fmt.Errorf("HTTP status %v", resp.Status)
	// }

	if _, err := os.Stat(filepath.Join(root, "roimg", pkg+".squashfs")); err == nil {
		return nil // package already installed
	}

	tmpDir := filepath.Join(root, "roimg", "tmp", "."+pkg+fmt.Sprintf("%d", os.Getpid()))
	if err := os.Mkdir(tmpDir, 0755); err != nil {
		if os.IsExist(err) {
			return nil // another goroutine is installing this package
		}
		return err
	}

	log.Printf("installing package %q to root %s", pkg, root)

	for _, fn := range []string{pkg + ".squashfs", pkg + ".meta.textproto"} {
		f, err := os.Create(filepath.Join(tmpDir, fn))
		if err != nil {
			return err
		}
		in, err := os.Open(filepath.Join(store, fn))
		if err != nil {
			return err
		}
		defer in.Close()
		if _, err := io.Copy(f, in); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	// first is true only on the first installation of the package (regardless
	// of its version).
	if first {
		readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
		if err != nil {
			return fmt.Errorf("copying /etc: %v", err)
		}

		rd, err := squashfs.NewReader(readerAt)
		if err != nil {
			return err
		}

		fis, err := rd.Readdir(rd.RootInode())
		if err != nil {
			return err
		}
		for _, fi := range fis {
			if fi.Name() != "etc" {
				continue
			}
			log.Printf("copying %s/etc", pkg)
			if err := unpackDir(filepath.Join(root, "etc"), rd, fi.Sys().(*squashfs.FileInfo).Inode); err != nil {
				return fmt.Errorf("copying /etc: %v", err)
			}
			break
		}
	}

	// First meta, then image: the fuse daemon considers the image canonical, so
	// it must go last.
	for _, fn := range []string{pkg + ".meta.textproto", pkg + ".squashfs"} {
		if err := os.Rename(filepath.Join(tmpDir, fn), filepath.Join(root, "roimg", fn)); err != nil {
			return err
		}
	}

	if err := os.Remove(tmpDir); err != nil {
		return err
	}

	return nil
}

func installTransitively1(root, store, pkg string) error {
	meta := filepath.Join(store, pkg+".meta.textproto")
	pm, err := readMeta(meta)
	if err != nil {
		return err
	}
	// TODO(later): we could write out b here and save 1 HTTP request
	pkgs := append([]string{pkg}, pm.GetRuntimeDep()...)
	log.Printf("resolved %s to %v", pkg, pkgs)

	// TODO: figure out if this is the first installation by checking existence
	// in the corresponding pkgset file
	first := true

	// download all packages with maximum concurrency for the time being
	var eg errgroup.Group
	for _, pkg := range pkgs {
		pkg := pkg //copy
		eg.Go(func() error {
			if err := install1(root, store, pkg, first); err != nil {
				return fmt.Errorf("installing %s: %v", pkg, err)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// ** TODO signal fuse daemon that a new package is now present

	return nil
}

func install(args []string) error {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")

		store = fset.String("store", "", "where to install packages from (file system path)")

		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Parse(args)
	if fset.NArg() < 1 {
		return fmt.Errorf("syntax: install [options] <package> [<package>...]")
	}

	// TODO: lock to ensure only one process modifies roimg at a time

	tmpDir := filepath.Join(*root, "roimg", "tmp")

	// Remove stale work directories of previously interrupted/crashed processes.
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}

	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return err
	}

	var eg errgroup.Group
	for _, pkg := range fset.Args() {
		pkg := pkg // copy
		eg.Go(func() error { return installTransitively1(*root, *store, pkg) })
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	return nil
}
