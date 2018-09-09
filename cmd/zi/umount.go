package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"syscall"
)

func umount(args []string) error {
	fset := flag.NewFlagSet("umount", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/ro",
			"TODO")
	)

	fset.Parse(args)
	if fset.NArg() != 1 {
		return fmt.Errorf("syntax: umount <package>")
	}
	pkg := fset.Arg(0)

	mountpoint := filepath.Join(*root, pkg)
	if err := syscall.Unmount(mountpoint, 0); err != nil {
		log.Printf("unmounting %s failed: %v", mountpoint, err)
	}

	return nil
}
