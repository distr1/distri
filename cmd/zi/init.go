package main

import (
	"log"
	"path/filepath"
	"strings"
	"syscall"
)

func pid1() error {
	log.Printf("mounting packages")

	// We need to mount /proc ourselves so that mount() can consult
	// /proc/self/mountinfo:
	if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_MGC_VAL, ""); err != nil {
		return err
	}

	matches, err := filepath.Glob("/ro/*.squashfs")
	if err != nil {
		return err
	}

	for idx, m := range matches {
		log.Printf("mounting package %d of %d: %q", idx, len(matches), m)
		// m is the full path to a squashfs image, e.g. /ro/strace-4.24.squashfs
		pkg := strings.TrimSuffix(filepath.Base(m), ".squashfs")
		if _, err := mount([]string{"-imgdir=/ro", pkg}); err != nil {
			return err
		}
	}

	const systemd = "/ro/systemd-239/buildoutput/lib/systemd/systemd" // TODO(later): glob?
	return syscall.Exec(systemd, []string{systemd}, nil)
}
