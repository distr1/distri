package bump_test

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/distr1/distri/pb"
	"github.com/gogo/protobuf/proto"
)

const buildTextproto = `
source: "empty://"
hash: ""
version: "530-1"
`

func TestBump(t *testing.T) {
	t.Parallel()

	distriroot, err := ioutil.TempDir("", "integrationbump")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(distriroot)

	// Write package build instructions:
	pkgDir := filepath.Join(distriroot, "pkgs", "test")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(
		filepath.Join(pkgDir, "build.textproto"),
		[]byte(buildTextproto),
		0644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("distri", "bump", "-all", "-w")
	build.Dir = pkgDir
	build.Env = []string{"DISTRIROOT=" + distriroot}
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("%v: %v", build.Args, err)
	}

	var bld pb.Build
	b, err := ioutil.ReadFile(filepath.Join(pkgDir, "build.textproto"))
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.UnmarshalText(string(b), &bld); err != nil {
		t.Fatal(err)
	}
	if got, want := bld.GetVersion(), "530-2"; got != want {
		t.Errorf("bump: got %q, want %q", got, want)
	}
}
