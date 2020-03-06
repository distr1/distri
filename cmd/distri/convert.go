package main

import (
	"context"
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
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
)

const convertHelp = `distri convert [-flags]

Convert a tarball to a distri SquashFS image.

Useful during distri development only.
`

// stringsFromByteSlice converts a sequence of attributes to a []string.
// On Linux, each entry is a NULL-terminated string.
func stringsFromByteSlice(buf []byte) []string {
	var result []string
	off := 0
	for i, b := range buf {
		if b == 0 {
			result = append(result, string(buf[off:i]))
			off = i + 1
		}
	}
	return result
}

func readXattrs(fd int) ([]squashfs.Xattr, error) {
	sz, err := unix.Flistxattr(fd, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sz)
	sz, err = unix.Flistxattr(fd, buf)
	if err != nil {
		return nil, err
	}
	var attrs []squashfs.Xattr
	attrnames := stringsFromByteSlice(buf)
	for _, attr := range attrnames {
		sz, err := unix.Fgetxattr(fd, attr, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, sz)
		sz, err = unix.Fgetxattr(fd, attr, buf)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, squashfs.XattrFromAttr(attr, buf))
	}
	return attrs, nil
}

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
			attrs, err := readXattrs(int(in.Fd()))
			if err != nil {
				return err
			}
			f, err := w.File(fi.Name(), fi.ModTime(), uint16(fi.Sys().(*syscall.Stat_t).Mode), attrs)
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

func convert(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("convert", flag.ExitOnError)
	var pkg = fset.String("pkg", "", "path to tar.gz package to convert to squashfs")
	fset.Usage = usage(fset, convertHelp)
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
