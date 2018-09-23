package main

import (
	"flag"
	"fmt"
)

const envHelp = `TODO
`

func env(args []string) error {
	fset := flag.NewFlagSet("env", flag.ExitOnError)
	fset.Parse(args)
	fmt.Printf("DISTRIROOT=%q\n", distriRoot)
	fmt.Printf("IMGDIR=%q\n", defaultImgDir)
	return nil
}
