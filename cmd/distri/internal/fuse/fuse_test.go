package fuse_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/distr1/distri/cmd/distri/internal/fuse"
	"github.com/distr1/distri/internal/cp"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

func TestFUSE(t *testing.T) {
	t.Parallel()

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

		// TODO: drop cache instead of waiting for it to expire
		time.Sleep(2 * fuse.VirtualFileExpiration) // ensure cache expired

		target, err := os.Readlink(tmpdir + "/bin/less")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := target, "../less-amd64-530-3/bin/less"; got != want {
			t.Fatalf("Readlink(bin/less) = %v, want %v", got, want)
		}
	})

	t.Run("DeleteVersion", func(t *testing.T) {
		for _, suffix := range []string{".meta.textproto", ".squashfs"} {
			if err := os.Remove(filepath.Join(repo, "less-amd64-530-3"+suffix)); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
			t.Fatal(err)
		}

		// TODO: drop cache instead of waiting for it to expire
		time.Sleep(2 * fuse.VirtualFileExpiration) // ensure cache expired

		target, err := os.Readlink(tmpdir + "/bin/less")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := target, "../less-amd64-530-2/bin/less"; got != want {
			t.Fatalf("Readlink(bin/less) = %v, want %v", got, want)
		}
	})
}

func TestXattr(t *testing.T) {
	t.Parallel()

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

	addPackage("i3status-amd64", "i3status-amd64-2.12", meta("i3status", "2.12"))

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

	f, err := os.Open(tmpdir + "/i3status-amd64-2.12/out/bin/i3status")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sz, err := unix.Flistxattr(int(f.Fd()), nil)
	if err != nil {
		t.Fatalf("flistxattr: %v", err)
	}

	if sz == 0 {
		t.Fatalf("no extended attributes found")
	}
	buf := make([]byte, sz)
	if _, err := unix.Flistxattr(int(f.Fd()), buf); err != nil {
		t.Fatalf("flistxattr: %v", err)
	}
	attrnames := stringsFromByteSlice(buf)
	if got, want := len(attrnames), 1; got != want {
		t.Fatalf("unexpected number of attributes: got %d, want %d", got, want)
	}
	attr := attrnames[0]
	sz, err = unix.Fgetxattr(int(f.Fd()), attr, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, sz)
	sz, err = unix.Fgetxattr(int(f.Fd()), attr, buf)
	if err != nil {
		t.Fatal(err)
	}
	got := squashfs.XattrFromAttr(attr, buf)
	want := squashfs.Xattr{
		Type:     2,
		FullName: "capability",
		Value:    []byte{1, 0, 0, 2, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected attribute: diff (-want +got):\n%s", diff)
	}
}

// stringsFromByteSlice converts a sequence of attributes to a []string.
// On Linux, each entry is a NULL-terminated string.
func stringsFromByteSlice(buf []byte) []string {
	var result []string
	off := 0
	for i, b := range buf {
		if b == 0 {
			result = append(result, string(buf[off:i]))
			off = i + 1
		}
	}
	return result
}
