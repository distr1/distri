package oninterrupt

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// onInterrupt allows subcommands to register cleanup handlers which shall be
// run on receiving SIGINT, e.g. reverting temporary CPU frequency scaling
// governor changes.
var (
	onInterruptMu sync.Mutex
	onInterrupt   []func()
)

func init() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		signal := <-c
		onInterruptMu.Lock()
		for _, f := range onInterrupt {
			f()
		}
		onInterruptMu.Unlock()
		// TODO: replace by cancelling a context:
		// https://medium.com/@matryer/make-ctrl-c-cancel-the-context-context-bd006a8ad6ff
		if sig, ok := signal.(*syscall.Signal); ok {
			os.Exit(128 + int(*sig))
		}
		os.Exit(1) // generic EXIT_FAILURE
	}()
}

func Register(cb func()) {
	onInterruptMu.Lock()
	defer onInterruptMu.Unlock()
	onInterrupt = append(onInterrupt, cb)
}
