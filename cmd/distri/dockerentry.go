package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
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
	bash := exec.Command("/ro/bin/bash", args...)
	bash.Stdin = os.Stdin
	bash.Stdout = os.Stdout
	bash.Stderr = os.Stderr
	if err := bash.Run(); err != nil {
		return fmt.Errorf("%v: %v", bash.Args, err)
	}
	return nil
}
