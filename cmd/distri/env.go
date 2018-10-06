package main

import (
	"flag"
	"fmt"

	"github.com/stapelberg/zi/internal/env"
)

const envHelp = `TODO
`

func printenv(args []string) error {
	fset := flag.NewFlagSet("env", flag.ExitOnError)
	fset.Parse(args)
	fmt.Printf("DISTRIROOT=%q\n", env.DistriRoot)
	fmt.Printf("DEFAULTREPO=%q\n", env.DefaultRepo)
	return nil
}
