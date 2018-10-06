package addrfd

import (
	"flag"
	"log"
	"os"
)

var (
	addrfd = flag.Int("addrfd", -1, "File descriptor on which to print the picked address")
)

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
