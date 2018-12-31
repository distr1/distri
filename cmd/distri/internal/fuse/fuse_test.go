package fuse_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/distr1/distri/cmd/distri/internal/fuse"
	"github.com/distr1/distri/internal/cp"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"google.golang.org/grpc"
)

func TestFUSE(t *testing.T) {
	ctx, canc := context.WithCancel(context.Background())
	defer canc()

	repo, err := ioutil.TempDir("", "distrifuse-repo")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(repo)

	meta := func(srcpkg, version string) string {
		return fmt.Sprintf("source_pkg: %q\nversion: %q", srcpkg, version)
	}
	addPackage := func(srcpkg, destpkg, metaOverride string) {
		src, err := filepath.EvalSymlinks(filepath.Join(env.DefaultRepo, srcpkg+".meta.textproto"))
		if err != nil {
			t.Fatal(err)
		}
		srcbase := strings.TrimSuffix(src, ".meta.textproto")
		for _, suffix := range []string{".squashfs", ".meta.textproto"} {
			if err := cp.File(srcbase+suffix, filepath.Join(repo, destpkg+suffix)); err != nil {
				t.Fatal(err)
			}
		}
		if metaOverride != "" {
			if err := ioutil.WriteFile(filepath.Join(repo, destpkg+".meta.textproto"), []byte(metaOverride), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	addPackage("less-amd64", "less-amd64-530", meta("less", "530"))
	addPackage("less-amd64", "less-amd64-530-2", meta("less", "530-2"))

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

	t.Run("ExchangeDir", func(t *testing.T) {
		target, err := os.Readlink(tmpdir + "/bin/less")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := target, "../less-amd64-530-2/bin/less"; got != want {
			t.Fatalf("Readlink(bin/less) = %v, want %v", got, want)
		}
	})

	ctl, err := os.Readlink(tmpdir + "/ctl")
	if err != nil {
		t.Fatal(err)
	}

	conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	cl := pb.NewFUSEClient(conn)

	t.Run("AddNewPackage", func(t *testing.T) {
		addPackage("bash-amd64", "bash-amd64-1", meta("bash", "1"))

		if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
			t.Fatal(err)
		}

		target, err := os.Readlink(tmpdir + "/bin/bash")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := target, "../bash-amd64-1/bin/bash"; got != want {
			t.Fatalf("Readlink(bin/bash) = %v, want %v", got, want)
		}
	})

	t.Run("AddNewVersion", func(t *testing.T) {
		addPackage("less-amd64", "less-amd64-530-3", meta("less", "530-3"))

		if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
			t.Fatal(err)
		}

		target, err := os.Readlink(tmpdir + "/bin/less")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := target, "../less-amd64-530-3/bin/less"; got != want {
			t.Fatalf("Readlink(bin/less) = %v, want %v", got, want)
		}
	})

	// TODO: delete the newer package
}
