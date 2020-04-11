package main

import (
	"errors"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"sort"
	"syscall"

	"github.com/distr1/distri"
)

func bootfuse() error {
	// TODO: start fuse in separate process, make argv[0] be '@' as per
	// https://www.freedesktop.org/wiki/Software/systemd/RootStorageDaemons/

	r, w, err := os.Pipe() // for readiness notification
	if err != nil {
		return err
	}

	fuse := exec.Command("/init", "fuse", "-repo=/roimg", "-readiness=3", "/ro")
	fuse.ExtraFiles = []*os.File{w}
	fuse.Env = []string{
		// Set TZ= so that the time package does not try to open /etc/localtime,
		// which is a symlink into /ro, which would deadlock when called from
		// the FUSE request handler.
		"TZ=",
		"TMPDIR=/ro-tmp",
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

	return nil
}

func findLatestSystemd() (string, error) {
	dir, err := os.Open("/ro")
	if err != nil {
		return "", err
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return "", err
	}
	var systemds []string
	for _, n := range names {
		pv := distri.ParseVersion(n)
		if pv.Pkg != "systemd" {
			continue
		}
		systemds = append(systemds, pv.String())
	}
	if len(systemds) == 0 {
		return "", errors.New("no systemd packages found in /ro")
	}
	sort.Slice(systemds, func(i, j int) bool {
		return distri.PackageRevisionLess(systemds[i], systemds[j])
	})
	pkg := systemds[len(systemds)-1] // most recent
	return "/ro/" + pkg + "/out/lib/systemd/systemd", nil
}

func pid1() error {
	log.Printf("FUSE-mounting package store /roimg on /ro")

	if err := bootfuse(); err != nil {
		return err
	}

	systemd, err := findLatestSystemd()
	if err != nil {
		log.Printf("find latest systemd: %v", err)
		// fall-through:
	}
	if systemd == "" {
		// Fall back to compile-time latest version:
		systemd = "/ro/systemd-amd64-245-11/out/lib/systemd/systemd"
	}

	log.Printf("starting systemd %s", systemd)

	return syscall.Exec(systemd, []string{systemd}, nil)
}
