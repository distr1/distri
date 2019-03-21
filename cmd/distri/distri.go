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
	"golang.org/x/xerrors"

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

	type cmd struct {
		helpText string
		fn       func(args []string) error
	}
	verbs := map[string]cmd{
		"build": {buildHelp, build},
		"mount": {mountHelp, func(args []string) error {
			_, err := mount(args)
			return err
		}},
		"umount": {umountHelp, umount},
		// TODO: remove this once we build to SquashFS by default
		"convert":  {convertHelp, convert},
		"pack":     {packHelp, pack},
		"scaffold": {scaffoldHelp, scaffold},
		"install":  {installHelp, install},
		"fuse": {fuse.Help, func(args []string) error {
			join, err := fuse.Mount(args)
			if err != nil {
				return err
			}
			if err := join(context.Background()); err != nil {
				return xerrors.Errorf("Join: %w", err)
			}
			return nil
		}},
		"fusectl": {fusectlHelp, fusectl},
		"export":  {exportHelp, export},
		"env":     {envHelp, printenv},
		"mirror":  {mirrorHelp, mirror},
		"batch":   {batchHelp, batch},
		"log":     {logHelp, showlog},
		"unpack":  {unpackHelp, unpack},
		"update":  {updateHelp, update},
		"gc":      {gcHelp, gc},
		"patch":   {patchHelp, patch},
	}

	args := flag.Args()
	verb := "build"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}

	if verb == "help" {
		if len(args) != 1 {
			fmt.Fprintf(os.Stderr, "syntax: distri help <verb>\n")
			fmt.Fprintf(os.Stderr, "\n")
			fmt.Fprintf(os.Stderr, "Verbs:\n")
			fmt.Fprintf(os.Stderr, "\tbuild - build a distri package\n")
			// TODO: complete short descriptions
			os.Exit(2)
		}
		verb = args[0]
		args = []string{"-help"}
	}
	v, ok := verbs[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n", verb)
		fmt.Fprintf(os.Stderr, "syntax: distri <command> [options]\n")
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "%s", v.helpText)
	if err := v.fn(args); err != nil {
		fmt.Printf("%s: %+v\n", verb, err)
		os.Exit(1)
	}
}
