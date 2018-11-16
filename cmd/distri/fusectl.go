package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/distr1/distri/pb"
	"google.golang.org/grpc"
)

const fusectlHelp = `TODO
`

func fusectl(args []string) error {
	fset := flag.NewFlagSet("fusectl", flag.ExitOnError)
	var ()
	fset.Parse(args)

	ctl, err := os.Readlink("/ro/ctl")
	if err != nil {
		return err
	}

	log.Printf("connecting to %s", ctl)
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	cl := pb.NewFUSEClient(conn)
	resp, err := cl.Ping(ctx, &pb.PingRequest{})
	if err != nil {
		return err
	}
	log.Printf("resp: %+v", resp)

	return nil
}
