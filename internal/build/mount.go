package build

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
)

func mountpoint(fn string) bool {
	b, err := ioutil.ReadFile("/proc/self/mountinfo")
	if err != nil {
		panic(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.Split(line, " ")
		if len(parts) < 5 {
			continue
		}
		if parts[4] == fn {
			return true
		}
	}
	return false
}

func mount1(mountpoint, pkg, src string) error {
	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		return err
	}

	// Find the next free loop device:
	const (
		LOOP_CTL_GET_FREE = 0x4c82
		LOOP_SET_FD       = 0x4c00
		LOOP_SET_STATUS64 = 0x4c04
	)

	loopctl, err := os.Open("/dev/loop-control")
	if err != nil {
		return err
	}
	defer loopctl.Close()
	free, _, errno := unix.Syscall(unix.SYS_IOCTL, loopctl.Fd(), LOOP_CTL_GET_FREE, 0)
	if errno != 0 {
		return errno
	}
	loopctl.Close()
	log.Printf("next free: %d", free)

	img, err := os.OpenFile(src, os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}

	loopdev := fmt.Sprintf("/dev/loop%d", free)
	loop, err := os.OpenFile(loopdev, os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}
	defer loop.Close()
	// TODO: get this into x/sys/unix
	type LoopInfo64 struct {
		device         uint64
		inode          uint64
		rdevice        uint64
		offset         uint64
		sizeLimit      uint64
		number         uint32
		encryptType    uint32
		encryptKeySize uint32
		flags          uint32
		filename       [64]byte
		cryptname      [64]byte
		encryptkey     [32]byte
		init           [2]uint64
	}
	const (
		LO_FLAGS_READ_ONLY = 1
		LO_FLAGS_AUTOCLEAR = 4 // loop device will autodestruct on last close
	)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_FD, uintptr(img.Fd())); errno != 0 {
		return errno
	}
	var filename [64]byte
	copy(filename[:], []byte(pkg))
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_STATUS64, uintptr(unsafe.Pointer(&LoopInfo64{
		flags:    LO_FLAGS_AUTOCLEAR | LO_FLAGS_READ_ONLY,
		filename: filename,
	}))); errno != 0 {
		return errno
	}

	if err := syscall.Mount(loopdev, mountpoint, "squashfs", syscall.MS_MGC_VAL, ""); err != nil {
		return err
	}

	return nil
}

func mount(args []string) (cleanup func(), _ error) {
	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/ro",
			"TODO")
		repo = fset.String("repo", env.DefaultRepo, "TODO")
		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Parse(args)
	if fset.NArg() != 1 {
		return nil, xerrors.Errorf("syntax: mount <package>")
	}
	pkg := fset.Arg(0)

	// TODO: glob package so that users can use “mount systemd” instead of
	// “mount systemd-239”? alternatively: tab completion

	meta, err := pb.ReadMetaFile(filepath.Join(*repo, pkg+".meta.textproto"))
	if err != nil {
		return nil, err
	}
	var deps []string
	for _, dep := range meta.GetRuntimeDep() {
		if dep == pkg {
			continue // skip circular dependencies, e.g. gcc depends on itself
		}
		if !mountpoint(filepath.Join(*root, dep)) {
			mountpoint := filepath.Join(*root, dep)
			src := filepath.Join(*repo, dep+".squashfs")
			if err := mount1(mountpoint, dep, src); err != nil {
				return nil, err
			}
			log.Printf("mounted %s (run-time dependency of %s)", mountpoint, pkg)
			deps = append(deps, dep)
		}
	}

	mountpoint := filepath.Join(*root, pkg)
	src := filepath.Join(*repo, pkg+".squashfs")
	if err := mount1(mountpoint, pkg, src); err != nil {
		return nil, err
	}

	if err := symlinkfarm(*root, pkg, "bin"); err != nil {
		return nil, err
	}
	if err := symlinkfarm(*root, pkg, "out/lib/systemd/system"); err != nil {
		return nil, err
	}

	log.Printf("mounted %s", mountpoint)

	return func() {
		if err := syscall.Unmount(mountpoint, 0); err != nil {
			log.Printf("unmounting %s failed: %v", mountpoint, err)
		}
		for _, dep := range deps {
			mountpoint := filepath.Join(*root, dep)
			if err := syscall.Unmount(mountpoint, 0); err != nil {
				log.Printf("unmounting %s failed: %v", mountpoint, err)
			}
		}
	}, nil
}
