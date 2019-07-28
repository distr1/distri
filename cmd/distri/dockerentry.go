package main

import (
	"log"
	"os"
	"syscall"
)

func entrypoint() error {
	log.Printf("FUSE-mounting package store /roimg on /ro")

	if err := bootfuse(); err != nil {
		return err
	}

	os.Setenv("TERMINFO", "/ro/share/terminfo") // TODO: file issue
	os.Setenv("PATH", "/bin")
	var args []string
	if len(os.Args) > 2 {
		// Strip not just os.Args[0] (/entrypoint),
		// but strip also os.Args[1] (sh).
		// This is how e.g. the debian Docker container behaves.
		args = os.Args[2:]
	}
	const bash = "/ro/bin/bash"
	return syscall.Exec(bash, append([]string{bash}, args...), os.Environ())
}
