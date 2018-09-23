package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Environment
var (
	distriRoot    = findDistriRoot()
	defaultImgDir = filepath.Join(distriRoot, "build/distri/pkg")
)

func findDistriRoot() string {
	env := os.Getenv("DISTRIROOT")
	if env != "" {
		return env
	}
	return os.ExpandEnv("$HOME/distri") // default
}

func main() {
	flag.Parse()

	if os.Getpid() == 1 {
		if err := pid1(); err != nil {
			log.Fatal(err)
		}
	}

	args := flag.Args()
	verb := "build" // TODO: change default to install
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}

	switch verb {
	case "build":
		if err := build(args); err != nil {
			log.Fatal(err)
		}

	// TODO: remove this once we build to SquashFS by default
	case "convert":
		if err := convert(args); err != nil {
			log.Fatal(err)
		}

	case "mount":
		if _, err := mount(args); err != nil {
			log.Fatal(err)
		}

	case "umount":
		if err := umount(args); err != nil {
			log.Fatal(err)
		}

	case "ninja":
		if err := ninja(args); err != nil {
			log.Fatal(err)
		}

	case "pack":
		if err := pack(args); err != nil {
			log.Fatal(err)
		}

	case "scaffold":
		if err := scaffold(args); err != nil {
			log.Fatal(err)
		}

	case "install":
		if err := install(args); err != nil {
			log.Fatal(err)
		}

	case "fuse":
		if err := mountfuse(args); err != nil {
			log.Fatal(err)
		}

	case "export":
		if err := export(args); err != nil {
			log.Fatal(err)
		}

	case "env":
		if err := env(args); err != nil {
			log.Fatal(err)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", verb)
		fmt.Fprintf(os.Stderr, "syntax: distri <command> [options]\n")
		os.Exit(2)
	}
}
