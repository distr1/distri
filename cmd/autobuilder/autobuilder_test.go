package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	job := flag.String("job", "", "TODO")
	flag.Parse()
	// Duplicate main() machinery so that we can test parts of the code which
	// re-exec the process.
	if *job != "" {
		if err := runJob(*job); err != nil {
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
	tempdir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempdir)

	for _, subdir := range []string{"pkg", "debug"} {
		if err := os.MkdirAll(filepath.Join(tempdir, "distri", "sha", subdir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("sha", filepath.Join(tempdir, "distri", "master")); err != nil {
		t.Fatal(err)
	}

	a := &autobuilder{
		repo:   "TODO",
		branch: "master",
		srvDir: tempdir,
		dryRun: true,
	}
	const commit = "HEAD"
	if err := a.runCommit(commit); err != nil {
		t.Fatal(err)
	}
	logFile := mustGlob1(t, filepath.Join(tempdir, "buildlogs", commit, "*", "stdout.log"))
	b, err := ioutil.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[debug] sh -c cd pkgs/i3status && distri
[batch] distri batch -dry_run
[image] sh -c mkdir -p $DESTDIR/img && make image DISKIMG=$DESTDIR/img/distri-disk.img
[image-serial] sh -c mkdir -p $DESTDIR/img && make image serial=1 DISKIMG=$DESTDIR/img/distri-qemu-serial.img
[image-gce] sh -c mkdir -p $DESTDIR/img && make gceimage GCSDISKIMG=$DESTDIR/img/distri-gce.tar.gz
[docker] sh -c make dockertar | tar tf -
[docs] sh -c make docs DOCSDIR=$DESTDIR/docs
[cp-destdir] sh -c cp --link -r -f -a build/distri/* $DESTDIR/
`
	if got := string(b); got != want {
		t.Fatalf("unexpected build log: got %q, want %q", got, want)
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
		for _, stamp := range []string{"debug", "image", "image-serial", "cp-destdir"} {
			stampFile := filepath.Join(tempdir, "work", commit, "stamp."+stamp)
			log.Printf("writing stampFile %q", stampFile)
			if err := ioutil.WriteFile(stampFile, nil, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.runCommit(commit); err != nil {
			t.Fatal(err)
		}
		logFile := mustGlob1(t, filepath.Join(tempdir, "buildlogs", commit, "*", "stdout.log"))
		b, err := ioutil.ReadFile(logFile)
		if err != nil {
			t.Fatal(err)
		}
		const want = `[debug] already built, skipping
[batch] distri batch -dry_run
[image] already built, skipping
[image-serial] already built, skipping
[image-gce] sh -c mkdir -p $DESTDIR/img && make gceimage GCSDISKIMG=$DESTDIR/img/distri-gce.tar.gz
[docker] sh -c make dockertar | tar tf -
[docs] sh -c make docs DOCSDIR=$DESTDIR/docs
[cp-destdir] already built, skipping
`
		if got := string(b); got != want {
			t.Fatalf("unexpected build log: got %q, want %q", got, want)
		}
	})
}
