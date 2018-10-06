package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"github.com/lpar/gzipped"
	"github.com/stapelberg/zi/internal/addrfd"
)

const exportHelp = `TODO
`

// Copied from src/net/http/server.go
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	return tc, nil
}

func export(args []string) error {
	fset := flag.NewFlagSet("export", flag.ExitOnError)
	var (
		listen = fset.String("listen", ":7080", "[host]:port listen address for exporting the distri store")
		gzip   = fset.Bool("gzip", true, "serve .gz files (if they exist). Typically desired on all networks but local loopback")
	)
	fset.Parse(args)
	log.Printf("exporting %s on %s", defaultImgDir, *listen)

	if *gzip {
		http.Handle("/", gzipped.FileServer(http.Dir(defaultImgDir)))
	} else {
		http.Handle("/", http.FileServer(http.Dir(defaultImgDir)))
	}

	server := &http.Server{Addr: *listen}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	addrfd.MustWrite(ln.Addr().String())
	return server.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
}
