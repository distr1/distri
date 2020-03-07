package distri

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// InterruptibleContext returns a context which is canceled when the program is
// interrupted (i.e. receiving SIGINT or SIGTERM).
func InterruptibleContext() (context.Context, context.CancelFunc) {
	ctx, canc := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		// Subsequent signals will result in immediate termination, which is
		// useful in case cleanup hangs:
		signal.Stop(sig)
		canc()
	}()
	return ctx, canc
}
