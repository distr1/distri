package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/repo"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
)

const listHelp = `distri list [-flags]

List all available distri packages of all configured repositories.

Example:
  % distri list
  % distri -repo https://repo.distr1.org/distri/jackherer list
`

func isNotExist(err error) bool {
	if _, ok := err.(*repo.ErrNotFound); ok {
		return true
	}
	return os.IsNotExist(err)
}
func hasPrefix(pkg string, prefix []string) bool {
	if len(prefix) == 0 {
		return true
	}
	for _, p := range prefix {
		if strings.HasPrefix(pkg, p) {
			return true
		}
	}
	return false
}

func list(ctx context.Context, listRepo string, prefix []string) error {
	var repos []distri.Repo
	if listRepo != "" {
		repos = []distri.Repo{{Path: listRepo, PkgPath: listRepo + "/pkg/"}}
	} else {
		var err error
		repos, err = env.Repos()
		if err != nil {
			return err
		}
	}
	// TODO: fetch metadata from repos concurrently
	metas := make(map[*pb.MirrorMeta]distri.Repo)
	for _, r := range repos {
		rd, err := repo.Reader(context.Background(), r, "meta.binaryproto", true /* cache */)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return err
		}
		b, err := ioutil.ReadAll(rd)
		rd.Close()
		if err != nil {
			return err
		}
		var pm pb.MirrorMeta
		if err := proto.Unmarshal(b, &pm); err != nil {
			return err
		}
		metas[&pm] = r
	}
	for m := range metas {
		for _, pkg := range m.GetPackage() {
			if !hasPrefix(pkg.GetName(), prefix) {
				continue
			}
			fmt.Println(pkg.GetName())
		}
	}
	return nil
}

func cmdlist(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("list", flag.ExitOnError)
	repo := fset.String("repo", "", "repository from which to install packages from. path (default TODO) or HTTP URL (e.g. TODO)")
	fset.Usage = usage(fset, listHelp)
	fset.Parse(args)

	return list(ctx, *repo, fset.Args())
}
