package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
)

const mirrorHelp = `Makes a package store usable as a mirror
by bundling metadata from packages into meta.binaryproto.
`

// TODO: have export automatically call mirror

func mirror(args []string) error {
	fset := flag.NewFlagSet("mirror", flag.ExitOnError)
	fset.Parse(args)
	log.Printf("TODO: mirror")

	var mm pb.MirrorMeta

	fis, err := ioutil.ReadDir(".")
	if err != nil {
		return err
	}
	for _, fi := range fis {
		if !strings.HasSuffix(fi.Name(), ".squashfs") {
			continue
		}
		pkg := strings.TrimSuffix(fi.Name(), ".squashfs")
		mmp := pb.MirrorMeta_Package{
			Name: proto.String(pkg),
		}

		f, err := os.Open(fi.Name())
		if err != nil {
			return err
		}
		rd, err := squashfs.NewReader(f)
		if err != nil {
			return err
		}
		for _, wk := range exchangeDirs {
			wk = strings.TrimPrefix(wk, "/")
			inode, err := lookupPath(rd, wk)
			if err != nil {
				if _, ok := err.(*fileNotFoundError); ok {
					continue
				}
				return err
			}
			sfis, err := rd.Readdir(inode)
			if err != nil {
				return fmt.Errorf("Readdir(%s, %s): %v", fi.Name(), wk, err)
			}
			for _, sfi := range sfis {
				mmp.WellKnownPath = append(mmp.WellKnownPath, filepath.Join(wk, sfi.Name()))
			}
		}

		mm.Package = append(mm.Package, &mmp)
	}

	b, err := proto.Marshal(&mm)
	if err != nil {
		return err
	}

	// TODO: write atomic
	if err := ioutil.WriteFile("meta.binaryproto", b, 0644); err != nil {
		return err
	}

	return nil
}
