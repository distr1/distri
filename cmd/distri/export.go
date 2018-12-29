package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"github.com/distr1/distri/internal/addrfd"
	"github.com/distr1/distri/internal/env"
	"github.com/lpar/gzipped"
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
		repo   = fset.String("repo", env.DefaultRepoRoot, "repository to serve")
	)
	fset.Parse(args)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	server := &http.Server{Addr: addr}
	log.Printf("exporting %s on %s", *repo, addr)

	if *gzip {
		http.Handle("/", gzipped.FileServer(http.Dir(*repo)))
	} else {
		http.Handle("/", http.FileServer(http.Dir(*repo)))
	}

	addrfd.MustWrite(addr)
	return server.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
}
