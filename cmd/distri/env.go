package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/distr1/distri/internal/env"
)

const envHelp = `distri env [-flags]

Display distri variables.

Example:
  % distri env
`

func printenv(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("env", flag.ExitOnError)
	fset.Usage = usage(fset, envHelp)
	fset.Parse(args)
	if fset.NArg() > 0 {
		switch fset.Arg(0) {
		case "DISTRIROOT":
			fmt.Println(env.DistriRoot)
		case "DISTRICFG":
			fmt.Println(env.DistriConfig)
		case "DEFAULTREPO":
			fmt.Println(env.DefaultRepo)
		}
		return nil
	}
	fmt.Printf("DISTRIROOT=%q\n", env.DistriRoot)
	fmt.Printf("DISTRICFG=%q\n", env.DistriConfig)
	fmt.Printf("DEFAULTREPO=%q\n", env.DefaultRepo)
	fmt.Printf("DEFAULTREPOROOT=%q\n", env.DefaultRepoRoot)
	return nil
}
