package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"sync/atomic"

	"github.com/distr1/distri/cmd/distri/internal/fuse"
	"golang.org/x/xerrors"

	_ "github.com/distr1/distri/internal/oninterrupt"

	_ "net/http/pprof"
)

var (
	debug      = flag.Bool("debug", false, "enable debug mode: format error messages with additional detail")
	cpuprofile = flag.String("cpuprofile", "", "path to store a CPU profile at")
	tracefile  = flag.String("tracefile", "", "path to store a trace at")
	httpListen = flag.String("listen", "", "host:port to listen on for HTTP")
)

var atExit struct {
	sync.Mutex
	fns    []func() error
	closed uint32
}

func registerAtExit(fn func() error) {
	if atomic.LoadUint32(&atExit.closed) != 0 {
		panic("BUG: registerAtExit must not be called from an atExit func")
	}
	atExit.Lock()
	defer atExit.Unlock()
	atExit.fns = append(atExit.fns, fn)
}

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

	if *httpListen != "" {
		go http.ListenAndServe(*httpListen, nil)
	}

	if os.Args[0] == "/entrypoint" {
		if err := entrypoint(); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}

	if os.Getpid() == 1 {
		if err := pid1(); err != nil {
			log.Fatal(err)
		}
	}

	type cmd struct {
		fn func(args []string) error
	}
	verbs := map[string]cmd{
		"build": {build},
		"mount": {func(args []string) error {
			_, err := mount(args)
			return err
		}},
		"umount": {umount},
		// TODO: remove this once we build to SquashFS by default
		"convert":  {convert},
		"pack":     {pack},
		"scaffold": {scaffold},
		"install":  {install},
		"fuse": {func(args []string) error {
			join, err := fuse.Mount(args)
			if err != nil {
				return err
			}
			if err := join(context.Background()); err != nil {
				return xerrors.Errorf("Join: %w", err)
			}
			return nil
		}},
		"fusectl": {fusectl},
		"export":  {export},
		"env":     {printenv},
		"mirror":  {mirror},
		"batch":   {batch},
		"log":     {showlog},
		"unpack":  {unpack},
		"update":  {update},
		"gc":      {gc},
		"patch":   {patch},
		"bump":    {bump},
		"builder": {builder},
		"reset":   {reset},
		"run":     {run},
	}

	args := flag.Args()
	verb := "build"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}

	if verb == "help" {
		if len(args) != 1 {
			fmt.Fprintf(os.Stderr, "distri [-flags] <command> [-flags] <args>\n")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "To get help on any command, use distri <command> -help or distri help <command>.\n")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "Installation commands:\n")
			fmt.Fprintf(os.Stderr, "\tinstall  - install a distri package from a repository\n")
			fmt.Fprintf(os.Stderr, "\tupdate   - update installed packages\n")
			fmt.Fprintf(os.Stderr, "\treset    - reset packages to before an update\n")
			fmt.Fprintf(os.Stderr, "\tgc       - garbage collect unreferenced packages\n")
			fmt.Fprintf(os.Stderr, "\tpack     - pack a distri system image\n")
			fmt.Fprintf(os.Stderr, "\trun      - run a command in a mount namespace with /ro\n")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "Package build commands:\n")
			fmt.Fprintf(os.Stderr, "\tbuild    - build a distri package\n")
			fmt.Fprintf(os.Stderr, "\tscaffold - generate distri package build instructions\n")
			fmt.Fprintf(os.Stderr, "\tpatch    - interactively create a patch for a package\n")
			fmt.Fprintf(os.Stderr, "\tlog      - show package build log (local)\n")
			fmt.Fprintf(os.Stderr, "\tbump     - increase revision of package and rdeps\n")
			fmt.Fprintf(os.Stderr, "\tbatch    - build all distri packages\n")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "Package store commands:\n")
			fmt.Fprintf(os.Stderr, "\texport   - serve local package store to others\n")
			fmt.Fprintf(os.Stderr, "\tmirror   - make a package store usable as a repository\n")
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
	if err := v.fn(args); err != nil {
		if *debug {
			fmt.Fprintf(os.Stderr, "%s: %+v\n", verb, err)
		} else {
			fmt.Fprintf(os.Stderr, "%s: %v\n", verb, err)
		}
		os.Exit(1)
	}

	atomic.StoreUint32(&atExit.closed, 1)
	for _, fn := range atExit.fns {
		if err := fn(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
