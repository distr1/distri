package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/lpar/gzipped"
)

const exportHelp = `TODO
`

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
	return http.ListenAndServe(*listen, nil)
}
