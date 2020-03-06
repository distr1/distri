package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/distr1/distri/pb"
	"google.golang.org/grpc"
)

const fusectlHelp = `distri fusectl [-flags]

Send a control instruction to the FUSE file system.

Typically only used under the covers, or for debugging.

Example:
  % distri fusectl -scan_packages
`

func fusectl(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("fusectl", flag.ExitOnError)
	var (
		mkdirAll     = fset.String("mkdirall", "", "if non-empty, sends a MkdirAll request")
		scanPackages = fset.Bool("scan_packages", false, "sends a ScanPackages request")
	)
	fset.Usage = usage(fset, fusectlHelp)
	fset.Parse(args)

	ctl, err := os.Readlink("/ro/ctl")
	if err != nil {
		return err
	}

	log.Printf("connecting to %s", ctl)
	conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	cl := pb.NewFUSEClient(conn)
	if *mkdirAll != "" {
		if _, err := cl.MkdirAll(ctx, &pb.MkdirAllRequest{Dir: mkdirAll}); err != nil {
			return err
		}
	} else if *scanPackages {
		if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
			return err
		}
	} else {
		resp, err := cl.Ping(ctx, &pb.PingRequest{})
		if err != nil {
			return err
		}
		log.Printf("resp: %+v", resp)
	}

	return nil
}
