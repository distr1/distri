package main

import (
	"flag"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/distr1/distri/internal/squashfs"
	"golang.org/x/xerrors"
)

func cp(w *squashfs.Directory, dir string) error {
	//log.Printf("cp(%s)", dir)
	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		//log.Printf("file %s, mode %#o (raw %#o)", fi.Name(), fi.Mode(), fi.Sys().(*syscall.Stat_t).Mode)
		if fi.IsDir() {
			subdir := w.Directory(fi.Name(), fi.ModTime())
			if err := cp(subdir, filepath.Join(dir, fi.Name())); err != nil {
				return err
			}
		} else if fi.Mode().IsRegular() {
			in, err := os.Open(filepath.Join(dir, fi.Name()))
			if err != nil {
				return err
			}
			defer in.Close()
			f, err := w.File(fi.Name(), fi.ModTime(), uint16(fi.Sys().(*syscall.Stat_t).Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, in); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			in.Close()
		} else if fi.Mode()&os.ModeSymlink != 0 {
			dest, err := os.Readlink(filepath.Join(dir, fi.Name()))
			if err != nil {
				return err
			}
			if err := w.Symlink(dest, fi.Name(), fi.ModTime(), fi.Mode().Perm()); err != nil {
				return err
			}
		} else {
			log.Printf("ERROR: unsupported file: %v", filepath.Join(dir, fi.Name()))
		}
	}
	return w.Flush()
}

func convert(args []string) error {
	fset := flag.NewFlagSet("convert", flag.ExitOnError)
	var pkg = fset.String("pkg", "", "path to tar.gz package to convert to squashfs")
	fset.Parse(args)
	if *pkg == "" {
		return xerrors.Errorf("required: -pkg")
	}
	log.Printf("converting %s to SquashFS", *pkg)
	tmp, err := ioutil.TempDir("", "convert")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	log.Printf("extracting")
	cmd := exec.Command("tar", "xf", *pkg, "--strip-components=1")
	cmd.Dir = tmp
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %v", cmd.Args, err)
	}

	log.Printf("packing")
	out, err := ioutil.TempFile("", "convert")
	if err != nil {
		return err
	}

	w, err := squashfs.NewWriter(out, time.Now())
	if err != nil {
		return err
	}

	if err := cp(w.Root, tmp); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}
	converted := strings.TrimSuffix(*pkg, ".tar.gz") + ".squashfs"
	if err := os.Rename(out.Name(), converted); err != nil {
		return err
	}
	log.Printf("converted into %s", converted)
	return nil
}
