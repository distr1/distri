package squashfs

import (
	"bytes"
	"flag"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var fsImagePath = flag.String("fs_image_path", "", "Store the SquashFS test file system in the specified path for manual inspection")

func TestUnsquashfs(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("unsquashfs"); err != nil {
		t.Skip("unsquashfs not found in $PATH")
	}

	var (
		f   *os.File
		err error
	)
	if *fsImagePath != "" {
		f, err = os.Create(*fsImagePath)
	} else {
		f, err = ioutil.TempFile("", "squashfs")
		if err == nil {
			defer os.Remove(f.Name())
		}
	}
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewWriter(f, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ff, err := w.Root.File("hellö wörld", time.Now(), unix.S_IRUSR|unix.S_IRGRP|unix.S_IROTH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ff.Write([]byte("hello world!")); err != nil {
		t.Fatal(err)
	}
	if err := ff.Close(); err != nil {
		t.Fatal(err)
	}

	ff, err = w.Root.File("leer", time.Now(), unix.S_IRUSR|unix.S_IRGRP|unix.S_IROTH)
	if err != nil {
		t.Fatal(err)
	}
	if err := ff.Close(); err != nil {
		t.Fatal(err)
	}

	ff, err = w.Root.File("second file", time.Now(), unix.S_IRUSR|unix.S_IXUSR|
		unix.S_IRGRP|unix.S_IXGRP|
		unix.S_IROTH|unix.S_IXOTH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ff.Write([]byte("NON.\n")); err != nil {
		t.Fatal(err)
	}
	if err := ff.Close(); err != nil {
		t.Fatal(err)
	}

	if err := w.Root.Symlink("second file", "second link", time.Now(), unix.S_IRUSR|unix.S_IRGRP|unix.S_IROTH); err != nil {
		t.Fatal(err)
	}

	subdir := w.Root.Directory("subdir", time.Now())

	subsubdir := subdir.Directory("deep", time.Now())
	ff, err = subsubdir.File("yo", time.Now(), unix.S_IRUSR|unix.S_IRGRP|unix.S_IROTH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ff.Write([]byte("foo\n")); err != nil {
		t.Fatal(err)
	}
	if err := ff.Close(); err != nil {
		t.Fatal(err)
	}
	if err := subsubdir.Flush(); err != nil {
		t.Fatal(err)
	}

	// TODO: write another file in subdir now, will result in invalid parent inode

	ff, err = subdir.File("third file (in subdir)", time.Now(), unix.S_IRUSR|unix.S_IRGRP|unix.S_IROTH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ff.Write([]byte("contents\n")); err != nil {
		t.Fatal(err)
	}
	if err := ff.Close(); err != nil {
		t.Fatal(err)
	}

	if err := subdir.Flush(); err != nil {
		t.Fatal(err)
	}
	ff, err = w.Root.File("testbin", time.Now(), unix.S_IRUSR|unix.S_IXUSR|
		unix.S_IRGRP|unix.S_IXGRP|
		unix.S_IROTH|unix.S_IXOTH)
	if err != nil {
		t.Fatal(err)
	}
	zf, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer zf.Close()
	if _, err := io.Copy(ff, zf); err != nil {
		t.Fatal(err)
	}
	if err := ff.Close(); err != nil {
		t.Fatal(err)
	}

	if err := w.Root.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Extract our generated file system using unsquashfs(1)
	out, err := ioutil.TempDir("", "unsquashfs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(out)
	cmd := exec.Command("unsquashfs", "-d", filepath.Join(out, "x"), f.Name())
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	fbin, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}

	// Verify the extracted files match our expectations.
	for _, entry := range []struct {
		path     string
		contents io.Reader
	}{
		{"leer", strings.NewReader("")},
		{"hellö wörld", strings.NewReader("hello world!")},
		{"testbin", fbin},
		{"subdir/third file (in subdir)", strings.NewReader("contents\n")},
	} {
		entry := entry // copy
		t.Run(entry.path, func(t *testing.T) {
			t.Parallel()
			in, err := os.Open(filepath.Join(out, "x", entry.path))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ioutil.ReadAll(in)
			if err != nil {
				t.Fatal(err)
			}
			want, err := ioutil.ReadAll(entry.contents)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("path %q differs", entry.path)
			}
		})
	}
}
