package main

import (
	"flag"
	"fmt"
)

func env(args []string) error {
	fset := flag.NewFlagSet("env", flag.ExitOnError)
	fset.Parse(args)
	fmt.Printf("DISTRIROOT=%q\n", distriRoot)
	fmt.Printf("IMGDIR=%q\n", defaultImgDir)
	return nil
}
