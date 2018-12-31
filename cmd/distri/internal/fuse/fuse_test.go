package fuse_test

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/distr1/distri/cmd/distri/internal/fuse"
	"github.com/distr1/distri/internal/cp"
	"github.com/distr1/distri/internal/env"
)

func TestFUSE(t *testing.T) {
	ctx, canc := context.WithCancel(context.Background())
	defer canc()

	repo, err := ioutil.TempDir("", "distrifuse-repo")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(repo)

	target, err := filepath.EvalSymlinks(filepath.Join(env.DefaultRepo, "less-amd64.meta.textproto"))
	if err != nil {
		t.Fatal(err)
	}
	srcbase := strings.TrimSuffix(target, ".meta.textproto")

	const (
		lessMeta = `
source_pkg: "less"
version: "530-2"
`

		less2Meta = `
source_pkg: "less"
version: "530-2"
`
	)
	for _, base := range []string{
		"less-amd64-530",
		"less-amd64-530-2",
	} {
		src := srcbase + ".squashfs"
		dest := filepath.Join(repo, base+".squashfs")
		if err := cp.File(src, dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := ioutil.WriteFile(filepath.Join(repo, "less-amd64-530.meta.textproto"), []byte(lessMeta), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(repo, "less-amd64-530-2.meta.textproto"), []byte(less2Meta), 0644); err != nil {
		t.Fatal(err)
	}

	tmpdir, err := ioutil.TempDir("", "distrifuse")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	join, err := fuse.Mount([]string{
		"-repo=" + repo,
		tmpdir,
	})
	if err != nil {
		t.Fatalf("fuse.Mount(%s): %v", tmpdir, err)
	}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		err := join(ctx)
		if err != nil && err != context.Canceled {
			t.Fatalf("join: %v", err)
		}
	}()
	defer func() {
		canc()
		<-joined
	}()

	if _, err := os.Stat(tmpdir + "/less-amd64-530/bin/less"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(tmpdir + "/less-amd64-530-2/bin/less"); err != nil {
		t.Fatal(err)
	}

	target, err = os.Readlink(tmpdir + "/bin/less")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := target, "../less-amd64-530-2/bin/less"; err != nil {
		t.Fatalf("Readlink(bin/less) = %v, want %v", got, want)
	}

	// TODO: delete the newer package
}
