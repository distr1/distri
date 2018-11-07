package main

import (
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"syscall"
)

func pid1() error {
	log.Printf("mounting packages")

	// TODO: start fuse in separate process, make argv[0] be '@' as per
	// https://www.freedesktop.org/wiki/Software/systemd/RootStorageDaemons/

	// We need to mount /proc ourselves so that mount() can consult
	// /proc/self/mountinfo:
	if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_MGC_VAL, ""); err != nil {
		log.Printf("mounting /proc failed: %v", err)
	}

	for _, pkg := range []string{"fuse-3.2.6", "glibc-2.27"} {
		if err := os.Symlink("/roimg/"+pkg+".squashfs", "/ro/"+pkg+".squashfs"); err != nil && !os.IsExist(err) {
			return err
		}
		if err := os.Symlink("/roimg/"+pkg+".meta.textproto", "/ro/"+pkg+".meta.textproto"); err != nil && !os.IsExist(err) {
			return err
		}
	}
	if _, err := mount([]string{"-repo=/ro", "fuse-3.2.6"}); err != nil {
		return err
	}

	r, w, err := os.Pipe() // for readiness notification
	if err != nil {
		return err
	}

	fuse := exec.Command("/init", "fuse", "-repo=/roimg", "-readiness=3", "/ro")
	fuse.ExtraFiles = []*os.File{w}
	fuse.Env = []string{
		"PATH=/ro/fuse-3.2.6/out/bin",
		// Set TZ= so that the time package does not try to open /etc/localtime,
		// which is a symlink into /ro, which would deadlock when called from
		// the FUSE request handler.
		"TZ=",
	}
	fuse.Stderr = os.Stderr
	fuse.Stdout = os.Stdout
	if err := fuse.Start(); err != nil {
		return err
	}

	// Close the write end of the pipe in the parent process.
	if err := w.Close(); err != nil {
		return err
	}

	// Wait until the read end of the pipe returns EOF
	if _, err := ioutil.ReadAll(r); err != nil {
		return err
	}

	log.Printf("starting systemd")

	// matches, err := filepath.Glob("/ro/*.squashfs")
	// if err != nil {
	// 	return err
	// }

	// for idx, m := range matches {
	// 	log.Printf("mounting package %d of %d: %q", idx, len(matches), m)
	// 	// m is the full path to a squashfs image, e.g. /ro/strace-4.24.squashfs
	// 	pkg := strings.TrimSuffix(filepath.Base(m), ".squashfs")
	// 	if _, err := mount([]string{"-repo=/ro", pkg}); err != nil {
	// 		return err
	// 	}
	// }

	const systemd = "/ro/systemd-239/out/lib/systemd/systemd" // TODO(later): glob?
	return syscall.Exec(systemd, []string{systemd}, nil)
}
