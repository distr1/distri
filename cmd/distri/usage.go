package main

import (
	"flag"
	"fmt"
	"os"
)

func usage(fset *flag.FlagSet, helpText string) func() {
	return func() {
		fmt.Fprintln(os.Stderr, helpText)
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", fset.Name())
		fset.PrintDefaults()
	}
}
