package main

import (
	"flag"
	"log"
	"net/http"
)

func export(args []string) error {
	fset := flag.NewFlagSet("export", flag.ExitOnError)
	var (
		listen = fset.String("listen", ":7080", "[host]:port listen address for exporting the zi store")
	)
	fset.Parse(args)
	log.Printf("TODO: export")

	http.Handle("/", http.FileServer(http.Dir("/home/michael/zi/build/zi/pkg/")))
	return http.ListenAndServe(*listen, nil)
}
