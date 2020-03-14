package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/distr1/distri"
)

func branchExists(ctx context.Context, branchName string) (bool, error) {
	branches := exec.CommandContext(ctx, "git", "branch", "--format", "%(refname:short)")
	branches.Stderr = os.Stderr
	out, err := branches.Output()
	if err != nil {
		return false, err
	}
	existing := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, branch := range existing {
		if branch == branchName {
			return true, nil
		}
	}
	return false, nil
}

type release struct {
	branchName string
}

func (rel *release) release(ctx context.Context) error {
	exists, err := branchExists(ctx, rel.branchName)
	if err != nil {
		return err
	}
	if exists {
		log.Printf("git branch %q already exists, not creating", rel.branchName)
	} else {
		log.Printf("creating git branch %q", rel.branchName)
		create := exec.CommandContext(ctx, "git", "checkout", "-b", rel.branchName)
		create.Stdout = os.Stdout
		create.Stderr = os.Stderr
		if err := create.Run(); err != nil {
			return fmt.Errorf("%v: %v", create.Args, err)
		}
	}

	batch := exec.CommandContext(ctx, "distri", "batch")
	batch.Stdout = os.Stdout
	batch.Stderr = os.Stderr
	if err := batch.Run(); err != nil {
		// Packages which FTBFS are fixed (preferred) or removed from the release branch
		return fmt.Errorf("%v: %v", batch.Args, err)
	}

	// TODO: automate this: All items in “Cool Things To Try” section must be manually verified working with the branch’s disk image
	// cp --link --recursive <git-HEAD> <branchName>
	return nil
}

func main() {
	ctx, canc := distri.InterruptibleContext()
	defer canc()
	var rel release
	flag.StringVar(&rel.branchName, "name", "jackherer", "distri release name")
	flag.Parse()
	if err := rel.release(ctx); err != nil {
		log.Fatal(err)
	}
}
