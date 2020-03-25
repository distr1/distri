package addrfd

import (
	"flag"
	"log"
	"os"
)

var (
	addrfd = flag.Int("addrfd", -1, "File descriptor on which to print the picked address")
)

type PendingAddrfd struct {
	fd int
}

func (p *PendingAddrfd) MustWrite(addr string) {
	if p.fd == -1 {
		return
	}
	f := os.NewFile(uintptr(p.fd), "")
	if _, err := f.Write([]byte(addr)); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

func RegisterFlags(fset *flag.FlagSet) *PendingAddrfd {
	pending := &PendingAddrfd{}
	fset.IntVar(&pending.fd, "addrfd", -1, "File descriptor on which to print the picked address")
	return pending
}

// MustWrite communicates listening address addr to the parent process via the
// file descriptor number passed to -addrfd, if any. It must be called precisely
// once.
func MustWrite(addr string) {
	if *addrfd == -1 {
		return
	}
	f := os.NewFile(uintptr(*addrfd), "")
	if _, err := f.Write([]byte(addr)); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
