package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cavaliercoder/go-cpio"
	"github.com/google/renameio"
	"github.com/klauspost/pgzip"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const initrdHelp = `distri initrd [-flags]

Generates an initramfs image (formerly known as initrd) for the specified Linux
kernel (-release) and atomically overwrite the specified output path (-output).

The expected runtime of this command is ≈1s.

Example:
  % distri initrd -release 5.4.6 -output /boot/initramfs-5.4.6-11.img
`

type cpioFile struct {
	Header cpio.Header
	Bytes  []byte
}

func filter(base, path string) bool {
	const (
		keep   = false
		remove = true
	)
	rel := strings.TrimPrefix(path, base)
	if !strings.HasPrefix(rel, "/kernel/") {
		return keep // only filter modules, not metadata files
	}
	if strings.HasPrefix(rel, "/kernel/fs/") &&
		!strings.HasPrefix(rel, "/kernel/fs/nls") {
		return keep // file systems
	}
	if strings.HasPrefix(rel, "/kernel/crypto/") ||
		rel == "/kernel/drivers/md/dm-crypt.ko" ||
		rel == "/kernel/drivers/md/dm-integrity.ko" {
		return keep // disk encryption
	}
	if strings.HasPrefix(rel, "/kernel/drivers/md/") ||
		strings.HasPrefix(rel, "/kernel/lib/") {
		return keep // device mapper
	}
	if strings.Contains(rel, "sd_mod") ||
		strings.Contains(rel, "sr_mod") ||
		strings.Contains(rel, "usb_storage") ||
		strings.Contains(rel, "firewire-sbp2") ||
		strings.Contains(rel, "block") ||
		strings.Contains(rel, "scsi") ||
		strings.Contains(rel, "fusion") ||
		strings.Contains(rel, "nvme") ||
		strings.Contains(rel, "mmc") ||
		strings.Contains(rel, "tifm_") ||
		strings.Contains(rel, "virtio") ||
		strings.HasPrefix(rel, "/kernel/drivers/ata/") ||
		strings.HasPrefix(rel, "/kernel/drivers/usb/host/") ||
		strings.HasPrefix(rel, "/kernel/drivers/usb/storage/") ||
		strings.HasPrefix(rel, "/kernel/drivers/firewire/") {
		return keep // block devices
	}
	if strings.HasPrefix(rel, "/kernel/drivers/hid/") ||
		strings.HasPrefix(rel, "/kernel/drivers/input/keyboard/") ||
		strings.HasPrefix(rel, "/kernel/drivers/input/serio/") ||
		strings.Contains(rel, "usbhid") {
		return keep // keyboard input
	}
	return remove
}

func slurpModules(release string) ([]cpioFile, error) {
	var (
		tmpMu sync.Mutex
		tmp   = make(map[string]cpioFile)
		eg    errgroup.Group
	)

	// TODO: we don’t need all modules! which ones can we skip?
	// - see /usr/lib/initcpio/install/block and /filesystems and /encrypt

	// TODO(later): can use a more efficient iteration than filepath.Walk
	// TODO: read directly from the underlying dir instead of the exchange dir
	// TODO: read directly from the underlying image to avoid FUSE layer?
	base := filepath.Join("/ro/lib/modules", release)
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil {
			return nil
		}
		if path == filepath.Join(base, "source") ||
			path == filepath.Join(base, "build") {
			return nil // skip source dirs
		}
		if filter(base, path) {
			return nil // filtered
		}
		name := strings.TrimPrefix(path, "/ro/")
		mode := info.Mode()
		if info.IsDir() {
			return nil
		}
		eg.Go(func() error {
			b, err := ioutil.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					log.Printf("skipping %v", path)
					return nil // skip
				}
				return err
			}
			tmpMu.Lock()
			defer tmpMu.Unlock()
			tmp[path] = cpioFile{
				Header: cpio.Header{
					Name: name,
					Mode: cpio.FileMode(mode.Perm()),
					Size: int64(len(b)),
				},
				Bytes: b,
			}
			return nil
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(tmp))
	for key := range tmp {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	results := make([]cpioFile, 0, len(tmp))
	for _, key := range keys {
		results = append(results, tmp[key])
	}
	return results, nil
}

func slurpUncompressed(dir string) ([]cpioFile, error) {
	var (
		tmpMu sync.Mutex
		tmp   = make(map[string]cpioFile)
		eg    errgroup.Group
	)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil {
			return nil
		}
		name := strings.TrimPrefix(path, "/")
		mode := info.Mode()
		if info.IsDir() {
			if path == dir {
				return nil // skip root, already created using iw.mkdir()
			}
			tmpMu.Lock()
			defer tmpMu.Unlock()
			tmp[path] = cpioFile{
				Header: cpio.Header{
					Name: name + "/",
					Mode: cpio.ModeDir | 0755,
				},
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			st, err := os.Stat(path)
			if err != nil {
				return err
			}
			if st.Mode().IsDir() {
				log.Printf("skipping symlink to dir %v", path)
				return nil // TODO: recurse into symlinked directories
			}
		}
		eg.Go(func() error {
			b, err := ioutil.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					log.Printf("skipping %v", path)
					return nil // skip
				}
				return err
			}
			if strings.HasSuffix(name, ".gz") {
				name = strings.TrimSuffix(name, ".gz")
				rd, err := gzip.NewReader(bytes.NewReader(b))
				if err != nil {
					return err
				}
				defer rd.Close()
				var uncompressed bytes.Buffer
				if _, err := io.Copy(&uncompressed, rd); err != nil {
					return err
				}
				b = uncompressed.Bytes()
			}
			tmpMu.Lock()
			defer tmpMu.Unlock()
			tmp[path] = cpioFile{
				Header: cpio.Header{
					Name: name,
					Mode: cpio.FileMode(mode.Perm()),
					Size: int64(len(b)),
				},
				Bytes: b,
			}
			return nil
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(tmp))
	for key := range tmp {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	results := make([]cpioFile, 0, len(tmp))
	for _, key := range keys {
		results = append(results, tmp[key])
	}
	return results, nil
}

func pkgRootDir(fn string) string {
	if !strings.HasPrefix(fn, "/ro/") {
		return ""
	}
	fn = strings.TrimPrefix(fn, "/ro/")
	if idx := strings.Index(fn, "/"); idx > -1 {
		fn = fn[:idx]
	}
	return "/ro/" + fn
}

type initrdWriter struct {
	verbose bool
	wr      *cpio.Writer
	dirs    map[string]bool
	files   map[string]bool
}

func (i *initrdWriter) mkdir(dir string) error {
	if i.dirs[dir+"/"] {
		return nil // fast path
	}
	parts := strings.Split(dir, string(os.PathSeparator))
	for idx, part := range parts {
		sub := filepath.Join(strings.Join(parts[:idx], "/"), part) + "/"
		if i.dirs[sub] {
			continue
		}
		i.dirs[sub] = true
		if err := i.wr.WriteHeader(&cpio.Header{
			Name: sub,
			Mode: cpio.ModeDir | 0755,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (i *initrdWriter) mirror(fn string) error {
	if i.verbose {
		log.Printf("mirror(%v)", fn)
	}
	st, err := os.Lstat(fn)
	if err != nil {
		return err
	}
	if err := i.mkdir(strings.TrimPrefix(filepath.Dir(fn), "/")); err != nil {
		return err
	}
	name := strings.TrimPrefix(fn, "/")
	if i.files[name] {
		return nil // already stored
	}
	i.files[name] = true
	if st.Mode()&os.ModeSymlink == 0 {
		// destination (regular file), just copy it
		f, err := os.Open(fn)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := i.wr.WriteHeader(&cpio.Header{
			Name: name,
			Mode: cpio.FileMode(st.Mode().Perm()),
			Size: st.Size(),
		}); err != nil {
			return err
		}
		if _, err := io.Copy(i.wr, f); err != nil {
			return err
		}
		return nil
	}

	// symbolic link, resolve dynamic libraries
	target, err := os.Readlink(fn)
	if err != nil {
		return err
	}
	if err := i.wr.WriteHeader(&cpio.Header{
		Name: name,
		Mode: cpio.ModeSymlink | 0644,
		Size: int64(len(target)),
	}); err != nil {
		return err
	}
	if _, err := i.wr.Write([]byte(target)); err != nil {
		return err
	}
	if !strings.HasPrefix(target, "/") {
		target = filepath.Join(filepath.Dir(fn), target)
	}
	if err := i.mirror(target); err != nil {
		return err
	}

	ef, err := elf.Open(fn)
	if err != nil {
		return err
	}
	defer ef.Close()

	rp, err := ef.DynString(elf.DT_RPATH)
	if err != nil {
		return err
	}
	if i.verbose {
		log.Printf("[%v] rpath: %v", fn, rp)
	}
	// Explode the ELF binary RPATH into a flat []string together with the
	// package’s lib directory and last resort exchange directory /ro/lib by
	// joining, then splitting:
	libpath := strings.Split(strings.Join(append(append([]string{pkgRootDir(fn) + "/lib"}, rp...), "/ro/lib"), ":"), ":")

	// TODO: do this concurrently (workqueue)
	libs, err := ef.ImportedLibraries()
	if err != nil {
		return err
	}
	if i.verbose {
		log.Printf("[%v] libs: %v", fn, libs)
	}
	for _, lib := range libs {
		// TODO: don’t hardcode this
		if lib == "ld-linux-x86-64.so.2" {
			continue
		}
		// TODO(perf): cache readdir and do lookup w/o filesystem access?
		if i.verbose {
			log.Printf("libpath: %v", libpath)
		}
		for _, libdir := range libpath {
			if _, err := os.Stat(filepath.Join(libdir, lib)); err != nil {
				continue
			}
			if err := i.mirror(filepath.Join(libdir, lib)); err != nil {
				return err
			}
			break
		}
	}

	return nil
}

func copyFileCPIO(wr *cpio.Writer, dst, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := wr.WriteHeader(&cpio.Header{
		Name: dst,
		Mode: cpio.FileMode(fi.Mode().Perm()),
		Size: fi.Size(),
	}); err != nil {
		return err
	}
	if _, err := io.Copy(wr, f); err != nil {
		return err
	}
	return nil
}

var sectionNotFound = errors.New("ELF section not found")

func sectionContents(s *elf.Section) (string, error) {
	if s == nil {
		return "", sectionNotFound
	}
	b, err := s.Data()
	if err != nil {
		return "", err
	}
	return string(b[:bytes.IndexByte(b, '\x00')]), nil
}

func readDistriFilename(fn string) (string, error) {
	f, err := elf.Open(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return sectionContents(f.Section("distrifilename"))
}

// fn is a file within /ro/bin, e.g. /ro/bin/cryptsetup
func copyDistriBinaryToCPIO(iw *initrdWriter, destname, fn string) error {
	target, err := filepath.EvalSymlinks(fn)
	if err != nil {
		return err
	}

	dfn, err := readDistriFilename(target)
	if err != nil {
		if err != sectionNotFound {
			return err
		}
		// Either we are using the resolved path already (e.g. for minitrd,
		// which is discovered alongside the distri binary) or the program does
		// not use a wrapper program.
		dfn = target
	}

	if iw.verbose {
		log.Printf("target: %q", target)
	}

	// the binary itself:
	if err := copyFileCPIO(iw.wr, destname, dfn); err != nil {
		return err
	}

	f, err := elf.Open(dfn)
	if err != nil {
		return err
	}
	defer f.Close()

	if interp, err := sectionContents(f.Section(".interp")); err == nil {
		if iw.verbose {
			log.Printf("interp: %q", interp)
		}
		if err := iw.mirror(interp); err != nil {
			return err
		}
	}

	libs, err := f.ImportedLibraries()
	if err != nil {
		return err
	}
	for _, lib := range libs {
		if err := iw.mirror(filepath.Join(pkgRootDir(target), "lib", lib)); err != nil {
			return err
		}
	}

	return nil
}

func release() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		fmt.Fprintf(os.Stderr, "initrd: %v\n", err)
		os.Exit(1)
	}
	return string(uts.Release[:bytes.IndexByte(uts.Release[:], 0)])
}

func initrd(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("initrd", flag.ExitOnError)
	var (
		linuxRelease = fset.String("release", release(), "Linux kernel version to generate initrd for")
		outputPath   = fset.String("output", "/tmp/initrd", "path to write the initrd to")
		verbose      = fset.Bool("verbose", false, "print verbose messages")
	//dryRun    = fset.Bool("dry_run", false, "only print packages which would otherwise be built")
	)
	fset.Usage = usage(fset, initrdHelp)
	fset.Parse(args)

	// TODO: print all inputs to the initrd

	start := time.Now()
	var buf bytes.Buffer
	wr := cpio.NewWriter(&buf)
	iw := &initrdWriter{
		verbose: *verbose,
		wr:      wr,
		dirs:    make(map[string]bool),
		files:   make(map[string]bool),
	}

	if err := iw.mkdir("lib/modules"); err != nil {
		return err
	}
	mods, err := slurpModules(*linuxRelease)
	if err != nil {
		return err
	}
	for _, mod := range mods {
		if err := iw.mkdir(strings.TrimPrefix(filepath.Dir(mod.Header.Name), "/")); err != nil {
			return err
		}
		if err := wr.WriteHeader(&mod.Header); err != nil {
			return err
		}
		if _, err := wr.Write(mod.Bytes); err != nil {
			return err
		}
	}
	log.Printf("kernel modules in %v", time.Since(start))

	minitrd := filepath.Join()
	if exe, err := os.Executable(); err == nil {
		// find minitrd next to the distri executable
		if abs, err := filepath.Abs(exe); err == nil {
			minitrd = filepath.Join(filepath.Dir(abs), "minitrd")
		}
	}
	if _, err := os.Stat(minitrd); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		minitrd, err = exec.LookPath("minitrd")
		if err != nil {
			return err
		}
	}
	log.Printf("using minitrd %s", minitrd)
	if err := copyDistriBinaryToCPIO(iw, "init", minitrd); err != nil {
		return err
	}
	if _, err := os.Stat("/tmp/sh"); err == nil {
		// TODO: package busybox
		if err := copyFileCPIO(wr, "sh", "/tmp/sh"); err != nil {
			return err
		}
	}

	// Copy /etc/localtime for log messages with the correct time zone
	if err := iw.mkdir("etc"); err != nil {
		return err
	}
	if err := copyFileCPIO(wr, "etc/localtime", "/etc/localtime"); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}

	if err := copyDistriBinaryToCPIO(iw, "cryptsetup", "/ro/bin/cryptsetup"); err != nil {
		return err
	}

	if err := copyDistriBinaryToCPIO(iw, "vgchange", "/ro/bin/vgchange"); err != nil {
		return err
	}

	if err := copyDistriBinaryToCPIO(iw, "vgmknodes", "/ro/bin/vgmknodes"); err != nil {
		return err
	}

	// If the GCC runtime library is not present, cryptsetup fails at runtime:
	// libgcc_s.so.1 must be installed for pthread_cancel to work
	if err := iw.mirror("/ro/lib64/libgcc_s.so.1"); err != nil {
		return err
	}

	if err := copyDistriBinaryToCPIO(iw, "setfont", "/ro/bin/setfont"); err != nil {
		return err
	}

	if err := iw.mkdir("ro/bin"); err != nil {
		return err
	}

	if err := copyDistriBinaryToCPIO(iw, "ro/bin/modprobe", "/ro/bin/modprobe"); err != nil {
		return err
	}

	{
		start := time.Now()
		if err := iw.mkdir("ro/share/consolefonts"); err != nil {
			return err
		}
		fonts, err := slurpUncompressed("/ro/share/consolefonts")
		if err != nil {
			return err
		}
		for _, font := range fonts {
			if err := wr.WriteHeader(&font.Header); err != nil {
				return err
			}
			if _, err := wr.Write(font.Bytes); err != nil {
				return err
			}
		}
		log.Printf("console fonts in %v", time.Since(start))
	}

	// TODO: remove: only for debugging
	if err := copyDistriBinaryToCPIO(iw, "strace", "/ro/bin/strace"); err != nil {
		return err
	}

	if err := copyDistriBinaryToCPIO(iw, "loadkeys", "/ro/bin/loadkeys"); err != nil {
		return err
	}

	{
		start := time.Now()
		if err := iw.mkdir("ro/share/keymaps"); err != nil {
			return err
		}
		keymaps, err := slurpUncompressed("/ro/share/keymaps")
		if err != nil {
			return err
		}
		for _, keymap := range keymaps {
			if err := wr.WriteHeader(&keymap.Header); err != nil {
				return err
			}
			if _, err := wr.Write(keymap.Bytes); err != nil {
				return err
			}
		}
		log.Printf("keymaps in %v", time.Since(start))
	}

	if err := wr.Close(); err != nil {
		return err
	}
	out, err := renameio.TempFile("", *outputPath)
	if err != nil {
		return err
	}
	defer out.Cleanup()
	zw := pgzip.NewWriter(out)
	if _, err := io.Copy(zw, &buf); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := out.CloseAtomicallyReplace(); err != nil {
		return err
	}

	log.Printf("written in %v", time.Since(start))
	return nil
}
