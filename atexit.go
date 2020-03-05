package distri

import (
	"sync"
	"sync/atomic"
)

var atExit struct {
	sync.Mutex
	fns    []func() error
	closed uint32
}

func RegisterAtExit(fn func() error) {
	if atomic.LoadUint32(&atExit.closed) != 0 {
		panic("BUG: RegisterAtExit must not be called from an atExit func")
	}
	atExit.Lock()
	defer atExit.Unlock()
	atExit.fns = append(atExit.fns, fn)
}

func RunAtExit() error {
	atomic.StoreUint32(&atExit.closed, 1)
	for _, fn := range atExit.fns {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}
