package main

import (
	"context"
	"flag"
	"log"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
)

const listHelp = `distri list [-flags]

List all available distri packages of all configured repositories.

Example:
  % distri list
  % distri -repo https://repo.distr1.org/distri/jackherer list
`

func list(ctx context.Context, repo string) error {
	var repos []distri.Repo
	if repo != "" {
		repos = []distri.Repo{{Path: repo}}
	} else {
		var err error
		repos, err = env.Repos()
		if err != nil {
			return err
		}
	}
	log.Printf("TODO: fetch meta from repos %v", repos)
	return nil
}

func cmdlist(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("list", flag.ExitOnError)
	repo := fset.String("repo", "", "repository from which to install packages from. path (default TODO) or HTTP URL (e.g. TODO)")
	fset.Usage = usage(fset, listHelp)
	fset.Parse(args)

	return list(ctx, *repo)
}
