package main

import (
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func pid1() error {
	log.Printf("mounting packages")

	// TODO: start fuse in separate process, make argv[0] be '@' as per
	// https://www.freedesktop.org/wiki/Software/systemd/RootStorageDaemons/

	// We need to mount /proc ourselves so that mount() can consult
	// /proc/self/mountinfo:
	if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_MGC_VAL, ""); err != nil {
		return err
	}

	for _, pkg := range []string{"fuse-3.2.6", "glibc-2.27"} {
		if err := os.Symlink("/roimg/"+pkg+".squashfs", "/ro/"+pkg+".squashfs"); err != nil && !os.IsExist(err) {
			return err
		}
		if err := os.Symlink("/roimg/"+pkg+".meta.textproto", "/ro/"+pkg+".meta.textproto"); err != nil && !os.IsExist(err) {
			return err
		}
	}
	if _, err := mount([]string{"-imgdir=/ro", "fuse-3.2.6"}); err != nil {
		return err
	}

	// TODO: readiness notification, see dcs-localdcs
	fuse := exec.Command("/init", "fuse", "-imgdir=/roimg", "/ro")
	fuse.Env = []string{"PATH=/ro/fuse-3.2.6/buildoutput/bin"}
	fuse.Stderr = os.Stderr
	fuse.Stdout = os.Stdout
	if err := fuse.Start(); err != nil {
		return err
	}

	log.Printf("waiting for fuse to start...")
	time.Sleep(2 * time.Second)

	log.Printf("starting systemd")

	// matches, err := filepath.Glob("/ro/*.squashfs")
	// if err != nil {
	// 	return err
	// }

	// for idx, m := range matches {
	// 	log.Printf("mounting package %d of %d: %q", idx, len(matches), m)
	// 	// m is the full path to a squashfs image, e.g. /ro/strace-4.24.squashfs
	// 	pkg := strings.TrimSuffix(filepath.Base(m), ".squashfs")
	// 	if _, err := mount([]string{"-imgdir=/ro", pkg}); err != nil {
	// 		return err
	// 	}
	// }

	const systemd = "/ro/systemd-239/buildoutput/lib/systemd/systemd" // TODO(later): glob?
	return syscall.Exec(systemd, []string{systemd}, nil)
}
