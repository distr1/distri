package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"runtime/trace"

	"github.com/distr1/distri/cmd/distri/internal/fuse"

	_ "github.com/distr1/distri/internal/oninterrupt"
)

var (
	cpuprofile = flag.String("cpuprofile", "", "path to store a CPU profile at")
	tracefile  = flag.String("tracefile", "", "path to store a trace at")
)

func main() {
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *tracefile != "" {
		f, err := os.Create(*tracefile)
		if err != nil {
			log.Fatal(err)
		}
		trace.Start(f)
		defer trace.Stop()
	}

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
	case "help":
		helpFlag := []string{"-help"}
		type cmd struct {
			helpText string
			helpFunc func()
		}
		verbs := map[string]cmd{
			"build":    {buildHelp, func() { build(helpFlag) }},
			"mount":    {mountHelp, func() { mount(helpFlag) }},
			"umount":   {umountHelp, func() { umount(helpFlag) }},
			"pack":     {packHelp, func() { pack(helpFlag) }},
			"scaffold": {scaffoldHelp, func() { scaffold(helpFlag) }},
			"install":  {installHelp, func() { install(helpFlag) }},
			"fuse":     {fuse.Help, func() { fuse.Mount(helpFlag) }},
			"fusectl":  {fusectlHelp, func() { fusectl(helpFlag) }},
			"export":   {exportHelp, func() { export(helpFlag) }},
			"env":      {envHelp, func() { printenv(helpFlag) }},
			"mirror":   {mirrorHelp, func() { mirror(helpFlag) }},
			"batch":    {batchHelp, func() { batch(helpFlag) }},
			"log":      {logHelp, func() { showlog(helpFlag) }},
			"unpack":   {unpackHelp, func() { unpack(helpFlag) }},
			"update":   {updateHelp, func() { update(helpFlag) }},
			"gc":       {gcHelp, func() { gc(helpFlag) }},
			"patch":    {patchHelp, func() { patch(helpFlag) }},
		}
		if len(args) != 1 {
			fmt.Fprintf(os.Stderr, "syntax: %s help <verb>\n", os.Args[0])
			fmt.Fprintf(os.Stderr, "\n")
			fmt.Fprintf(os.Stderr, "Verbs:\n")
			fmt.Fprintf(os.Stderr, "\tbuild - build a distri package\n")
			// TODO: complete short descriptions
			os.Exit(2)
		}

		verb, ok := verbs[args[0]]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "%s", verb.helpText)
		verb.helpFunc()

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
		join, err := fuse.Mount(args)
		if err != nil {
			log.Fatal(err)
		}
		if err := join(context.Background()); err != nil {
			log.Fatal(err)
		}

	case "fusectl":
		if err := fusectl(args); err != nil {
			log.Fatal(err)
		}

	case "export":
		if err := export(args); err != nil {
			log.Fatal(err)
		}

	case "env":
		if err := printenv(args); err != nil {
			log.Fatal(err)
		}

	case "mirror":
		if err := mirror(args); err != nil {
			log.Fatal(err)
		}

	case "batch":
		if err := batch(args); err != nil {
			log.Fatal(err)
		}

	case "log":
		if err := showlog(args); err != nil {
			log.Fatal(err)
		}

	case "unpack":
		if err := unpack(args); err != nil {
			log.Fatal(err)
		}

	case "update":
		if err := update(args); err != nil {
			log.Fatal(err)
		}

	case "gc":
		if err := gc(args); err != nil {
			log.Fatal(err)
		}

	case "patch":
		if err := patch(args); err != nil {
			log.Fatal(err)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", verb)
		fmt.Fprintf(os.Stderr, "syntax: distri <command> [options]\n")
		os.Exit(2)
	}
}
