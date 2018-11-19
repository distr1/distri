package main

import (
	"flag"
	"fmt"

	"github.com/distr1/distri/internal/env"
)

const envHelp = `TODO
`

func printenv(args []string) error {
	fset := flag.NewFlagSet("env", flag.ExitOnError)
	fset.Parse(args)
	fmt.Printf("DISTRIROOT=%q\n", env.DistriRoot)
	fmt.Printf("DISTRICFG=%q\n", env.DistriConfig)
	fmt.Printf("DEFAULTREPO=%q\n", env.DefaultRepo)
	return nil
}
