package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/distr1/distri"
	"github.com/google/go-cmp/cmp"
)

func TestMain(m *testing.M) {
	job := flag.String("job", "", "TODO")
	flag.Parse()
	// Duplicate main() machinery so that we can test parts of the code which
	// re-exec the process.
	if *job != "" {
		if err := runJob(context.Background(), *job); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}
	os.Exit(m.Run())
}

func TestAutobuilder(t *testing.T) {

}

func mustGlob1(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(matches), 1; got != want {
		t.Fatalf("unexpected number of glob results: got %d, want %d", got, want)
	}
	return matches[0]
}

func TestAutobuilderCommands(t *testing.T) {
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	tempdir, err := ioutil.TempDir("", "distri-autobuilder-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(tempdir); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}()

	for _, subdir := range []string{"pkg", "debug", "src"} {
		if err := os.MkdirAll(filepath.Join(tempdir, "distri", "sha", subdir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("sha", filepath.Join(tempdir, "distri", "master")); err != nil {
		t.Fatal(err)
	}

	repo := func() string {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}()
	const commit = "HEAD"
	a := &autobuilder{
		repo:   "file://" + repo,
		branch: "master",
		srvDir: tempdir,
		dryRun: true,
		//rebuild: commit,
	}
	if err := a.runCommit(ctx, commit); err != nil {
		t.Fatal(err)
	}
	logFile := mustGlob1(t, filepath.Join(tempdir, "buildlogs", commit, "*", "stdout.log"))
	b, err := ioutil.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[smoke-quick] distri build -pkg=srcfs
[smoke-c] distri build -pkg=make
[batch] distri batch -dry_run
[mirror-pkg] sh -c cd build/distri/pkg && distri mirror
[mirror-debug] sh -c cd build/distri/debug && distri mirror
[mirror-src] sh -c cd build/distri/src && distri mirror
[cp-destdir] sh -c cp --link -r -f -a build/distri/* $DESTDIR/
[image] sh -c mkdir -p $DESTDIR/img && make image DISKIMG=$DESTDIR/img/distri-disk.img
[image-serial] sh -c mkdir -p $DESTDIR/img && make image serial=1 DISKIMG=$DESTDIR/img/distri-qemu-serial.img
[image-gce] sh -c mkdir -p $DESTDIR/img && make gceimage GCEDISKIMG=$DESTDIR/img/distri-gce.tar.gz
[docker] sh -c make dockertar | tar tf -
[docs] sh -c make docs DOCSDIR=$DESTDIR/docs
`
	if diff := cmp.Diff(want, string(b)); diff != "" {
		t.Fatalf("unexpected build log: diff (-want +got):\n%s", diff)
	}
	if err := os.RemoveAll(mustGlob1(t, filepath.Join(tempdir, "buildlogs", commit, "*"))); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(tempdir, "distri", commit)); err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(tempdir, "distri", "master")
	if err := os.Remove(master); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sha", master); err != nil {
		t.Fatal(err)
	}

	t.Run("SkipBootstrapWithStamp", func(t *testing.T) {
		for _, step := range steps {
			stamp := step.stamp
			stampFile := filepath.Join(tempdir, "work", commit, "stamp."+stamp)
			log.Printf("writing stampFile %q", stampFile)
			if err := ioutil.WriteFile(stampFile, nil, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.runCommit(ctx, commit); err != nil {
			t.Fatal(err)
		}
		logFile := mustGlob1(t, filepath.Join(tempdir, "buildlogs", commit, "*", "stdout.log"))
		b, err := ioutil.ReadFile(logFile)
		if err != nil {
			t.Fatal(err)
		}
		const want = `[smoke-quick] already built, skipping
[smoke-c] already built, skipping
[batch] already built, skipping
[mirror-pkg] already built, skipping
[mirror-debug] already built, skipping
[mirror-src] already built, skipping
[cp-destdir] already built, skipping
[image] already built, skipping
[image-serial] already built, skipping
[image-gce] already built, skipping
[docker] already built, skipping
[docs] already built, skipping
`
		if diff := cmp.Diff(want, string(b)); diff != "" {
			t.Fatalf("unexpected build log: diff (-want +got):\n%s", diff)
		}
	})
}
