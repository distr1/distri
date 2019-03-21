package main

import (
	"flag"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"golang.org/x/xerrors"
)

const unpackHelp = `TODO
`

func unpack(args []string) error {
	fset := flag.NewFlagSet("unpack", flag.ExitOnError)
	fset.Parse(args)

	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	if buildDir := filepath.Join(env.DistriRoot, "build"); !strings.HasPrefix(wd, buildDir+"/") {
		return xerrors.Errorf("run unpack in a subdirectory of %s", buildDir)
	}
	pkg := filepath.Base(wd)

	pkgDir := "../../pkgs/" + pkg
	c, err := ioutil.ReadFile(filepath.Join(pkgDir, "build.textproto"))
	if err != nil {
		return xerrors.Errorf("reading accompanying build.textproto: %v", err)
	}
	var buildProto pb.Build
	if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
		return err
	}

	b := &buildctx{
		Proto:     &buildProto,
		PkgDir:    pkgDir,
		Pkg:       pkg,
		Version:   buildProto.GetVersion(),
		SourceDir: trimArchiveSuffix(filepath.Base(buildProto.GetSource())),
	}
	if err := b.extract(); err != nil {
		return xerrors.Errorf("extract: %v", err)
	}
	return nil
}
