package main

import (
	"flag"
	"log"
)

const bumpHelp = `distri bump [-flags]

bump increases the version number of the specified packages and their reverse
dependencies.
`

func bump(args []string) error {
	fset := flag.NewFlagSet("bump", flag.ExitOnError)
	var (
		write = flag.Bool("w", false, "write changes (default is a dry run)")
	)
	fset.Usage = usage(fset, bumpHelp)
	fset.Parse(args)

	log.Printf("TODO: implement bump, w = %v", *write)

	return nil
}
