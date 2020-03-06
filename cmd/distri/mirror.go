package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/distr1/distri/internal/fuse"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
)

const mirrorHelp = `distri mirror [-flags]

Make a package store fully usable as a repository
by bundling metadata from packages into meta.binaryproto.

This is not required for distri install to work, but e.g. for debugfs.

Example:
  % cd distri/build/distri/pkg
  % distri mirror
`

// TODO: have export automatically call mirror

func walk(rd *squashfs.Reader, dirInode squashfs.Inode, dir string) ([]string, error) {
	fis, err := rd.Readdir(dirInode)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, fi := range fis {
		if fi.Mode().IsRegular() {
			files = append(files, filepath.Join(dir, fi.Name()))
		}
		if fi.Mode().IsDir() {
			tmp, err := walk(rd, fi.Sys().(*squashfs.FileInfo).Inode, filepath.Join(dir, fi.Name()))
			if err != nil {
				return nil, err
			}
			files = append(files, tmp...)
		}
	}
	return files, nil
}

func mirror(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("mirror", flag.ExitOnError)
	fset.Usage = usage(fset, mirrorHelp)
	fset.Parse(args)

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
		for _, wk := range fuse.ExchangeDirs {
			wk = strings.TrimPrefix(wk, "/")
			inode, err := rd.LookupPath(wk)
			if err != nil {
				if _, ok := err.(*squashfs.FileNotFoundError); ok {
					continue
				}
				return err
			}

			files, err := walk(rd, inode, wk)
			if err != nil {
				return err
			}
			for _, fn := range files {
				mmp.WellKnownPath = append(mmp.WellKnownPath, fn)
			}
		}

		mm.Package = append(mm.Package, &mmp)
	}

	b, err := proto.Marshal(&mm)
	if err != nil {
		return err
	}

	if err := renameio.WriteFile("meta.binaryproto", b, 0644); err != nil {
		return err
	}
	log.Printf("wrote %d packages to meta.binaryproto (%d bytes)", len(mm.Package), len(b))

	return nil
}
