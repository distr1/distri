package main

import (
	"flag"
	"fmt"

	"github.com/distr1/distri/internal/env"
)

const envHelp = `distri env [-flags]

Display distri variables.

Example:
  % distri env
`

func printenv(args []string) error {
	fset := flag.NewFlagSet("env", flag.ExitOnError)
	fset.Usage = usage(fset, envHelp)
	fset.Parse(args)
	fmt.Printf("DISTRIROOT=%q\n", env.DistriRoot)
	fmt.Printf("DISTRICFG=%q\n", env.DistriConfig)
	fmt.Printf("DEFAULTREPO=%q\n", env.DefaultRepo)
	return nil
}
