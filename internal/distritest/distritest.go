package distritest

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"testing"
)

func Export(ctx context.Context, repo string) (addr string, cleanup func(), _ error) {
	export := exec.CommandContext(ctx, "distri",
		"-addrfd=3", // Go dup2()s ExtraFiles to 3 and onwards
		"export",
		"-listen=localhost:0",
		// Disable gzip: the gzipped.FileServer package is already tested, and
		// uncompressing these files makes the test run significantly slower.
		"-gzip=false",
		"-repo="+repo,
	)
	r, w, err := os.Pipe()
	export.Stderr = os.Stderr
	export.Stdout = os.Stdout
	export.ExtraFiles = []*os.File{w}
	if err := export.Start(); err != nil {
		return "", nil, fmt.Errorf("%v: %v", export.Args, err)
	}
	cleanup = func() {
		export.Process.Kill()
		export.Wait()
	}

	// Close the write end of the pipe in the parent process.
	if err := w.Close(); err != nil {
		return "", nil, err
	}

	// Read the listening address from the pipe. A successful read also serves
	// as readiness notification.
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return "", nil, err
	}
	return string(b), cleanup, nil
}

// RemoveAll wraps os.RemoveAll and fails the test on failure.
func RemoveAll(t testing.TB, path string) {
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
