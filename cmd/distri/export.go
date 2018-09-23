package main

import (
	"flag"
	"log"
	"net/http"
)

const exportHelp = `TODO
`

func export(args []string) error {
	fset := flag.NewFlagSet("export", flag.ExitOnError)
	var (
		listen = fset.String("listen", ":7080", "[host]:port listen address for exporting the distri store")
	)
	fset.Parse(args)
	log.Printf("exporting %s on %s", defaultImgDir, *listen)

	http.Handle("/", http.FileServer(http.Dir(defaultImgDir)))
	return http.ListenAndServe(*listen, nil)
}
