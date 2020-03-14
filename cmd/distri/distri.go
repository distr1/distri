package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strconv"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/fuse"
	internaltrace "github.com/distr1/distri/internal/trace"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"

	_ "net/http/pprof"
)

var (
	debug      = flag.Bool("debug", false, "enable debug mode: format error messages with additional detail")
	cpuprofile = flag.String("cpuprofile", "", "path to store a CPU profile at")
	memprofile = flag.String("memprofile", "", "path to store a memory profile at")
	tracefile  = flag.String("tracefile", "", "path to store a trace at")
	ctracefile = flag.String("ctracefile", "", "path to store a chrome trace event file at (load in chrome://tracing)")
	httpListen = flag.String("listen", "", "host:port to listen on for HTTP")
)

func bumpRlimitNOFILE() error {
	// The smaller of the two is the highest which Linux will let us set:
	// https://github.com/torvalds/linux/blob/2be7d348fe924f0c5583c6a805bd42cecda93104/kernel/sys.c#L1526-L1541
	var fileMax, nrOpen uint64
	{
		b, err := ioutil.ReadFile("/proc/sys/fs/file-max")
		if err != nil {
			return err
		}
		fileMax, err = strconv.ParseUint(strings.TrimSpace(string(b)), 0, 64)
		if err != nil {
			return err
		}
	}
	{
		b, err := ioutil.ReadFile("/proc/sys/fs/nr_open")
		if err != nil {
			return err
		}
		nrOpen, err = strconv.ParseUint(strings.TrimSpace(string(b)), 0, 64)
		if err != nil {
			return err
		}
	}
	max := fileMax
	if nrOpen < max {
		max = nrOpen
	}
	set := unix.Rlimit{
		Max: max,
		Cur: max,
	}
	return unix.Setrlimit(unix.RLIMIT_NOFILE, &set)
}

func funcmain() error {
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			return err
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *tracefile != "" {
		f, err := os.Create(*tracefile)
		if err != nil {
			return err
		}
		trace.Start(f)
		defer trace.Stop()
	}

	if *ctracefile != "" {
		f, err := os.Create(*ctracefile)
		if err != nil {
			return err
		}
		internaltrace.Sink(f)
	}

	if *httpListen != "" {
		go http.ListenAndServe(*httpListen, nil)
	}

	if os.Args[0] == "/entrypoint" {
		if err := entrypoint(); err != nil {
			return err
		}
		return nil
	}

	if os.Getpid() == 1 {
		if err := pid1(); err != nil {
			return err
		}
		return nil
	}

	type cmd struct {
		fn func(ctx context.Context, args []string) error
	}
	verbs := map[string]cmd{
		"build": {cmdbuild},
		// TODO: remove this once we build to SquashFS by default
		"convert":  {convert},
		"pack":     {pack},
		"scaffold": {scaffold},
		"install":  {cmdinstall},
		"fuse": {func(ctx context.Context, args []string) error {
			if err := bumpRlimitNOFILE(); err != nil {
				log.Printf("Warning: bumping RLIMIT_NOFILE failed: %v", err)
			}
			join, err := fuse.Mount(ctx, args)
			if err != nil {
				return err
			}
			if err := join(ctx); err != nil {
				return xerrors.Errorf("Join: %w", err)
			}
			return nil
		}},
		"fusectl": {fusectl},
		"export":  {export},
		"env":     {printenv},
		"mirror":  {mirror},
		"batch":   {cmdbatch},
		"log":     {showlog},
		"unpack":  {unpack},
		"update":  {update},
		"gc":      {gc},
		"patch":   {patch},
		"bump":    {bump},
		"builder": {builder},
		"reset":   {reset},
		"run":     {run},
		"initrd":  {initrd},
		"list":    {cmdlist},
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
			fmt.Fprintf(os.Stderr, "\tinitrd   - pack a distri initramfs\n")
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
	ctx, canc := distri.InterruptibleContext()
	defer canc()
	v, ok := verbs[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n", verb)
		fmt.Fprintf(os.Stderr, "syntax: distri <command> [options]\n")
		os.Exit(2)
	}
	if err := v.fn(ctx, args); err != nil {
		if *memprofile != "" {
			f, err := os.Create(*memprofile)
			if err != nil {
				log.Fatal("could not create memory profile: ", err)
			}
			defer f.Close()
			runtime.GC() // get up-to-date statistics
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Fatal("could not write memory profile: ", err)
			}
		}
		if *debug {
			return fmt.Errorf("%s: %+v\n", verb, err)
		} else {
			return fmt.Errorf("%s: %v\n", verb, err)
		}
	}

	return distri.RunAtExit()
}

func main() {
	if err := funcmain(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
