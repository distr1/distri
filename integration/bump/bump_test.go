package bump_test

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/pb"
	"google.golang.org/protobuf/encoding/prototext"
)

var files = map[string]string{
	"pkgs/test/build.textproto": `
source: "empty://"
hash: ""
version: "530-1"

dep: "systemd"
`,

	"pkgs/lvm2/build.textproto": `
source: "empty://"
hash: ""
version: "2.03.00-3"
`,

	"pkgs/systemd/build.textproto": `
source: "empty://"
hash: ""
version: "239-7"

dep: "lvm2"
`,

	"pkgs/unaffected/build.textproto": `
source: "empty://"
hash: ""
version: "1-1"
`,

	"build/distri/pkg/test-amd64-530-1.meta.textproto":     `source_pkg: "test"`,
	"build/distri/pkg/lvm2-amd64-2.03.00-3.meta.textproto": `source_pkg: "lvm2"`,
	"build/distri/pkg/systemd-amd64-239-8.meta.textproto":  `source_pkg: "systemd"`,
	"build/distri/pkg/unaffected-amd64-1-1.meta.textproto": `source_pkg: "unaffected"`,
}

func TestBump(t *testing.T) {
	t.Parallel()

	ctx, canc := distri.InterruptibleContext()
	defer canc()

	distriroot, err := ioutil.TempDir("", "integrationbump")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, distriroot)

	// Write package build instructions:
	for path, content := range files {
		fullpath := filepath.Join(distriroot, path)
		if err := os.MkdirAll(filepath.Dir(fullpath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := ioutil.WriteFile(
			fullpath,
			[]byte(content),
			0644); err != nil {
			t.Fatal(err)
		}
	}

	build := exec.CommandContext(ctx, "distri", "bump", "-all", "-w")
	build.Dir = filepath.Join(distriroot, "pkgs/test")
	build.Env = []string{"DISTRIROOT=" + distriroot}
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("%v: %v", build.Args, err)
	}

	var bld pb.Build
	b, err := ioutil.ReadFile(filepath.Join(distriroot, "pkgs/test/build.textproto"))
	if err != nil {
		t.Fatal(err)
	}
	if err := prototext.Unmarshal(b, &bld); err != nil {
		t.Fatal(err)
	}
	if got, want := bld.GetVersion(), "530-2"; got != want {
		t.Errorf("bump: got %q, want %q", got, want)
	}
}

func TestBumpRdeps(t *testing.T) {
	t.Parallel()

	ctx, canc := distri.InterruptibleContext()
	defer canc()

	distriroot, err := ioutil.TempDir("", "integrationbump")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, distriroot)

	// Write package build instructions:
	for path, content := range files {
		fullpath := filepath.Join(distriroot, path)
		if err := os.MkdirAll(filepath.Dir(fullpath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := ioutil.WriteFile(
			fullpath,
			[]byte(content),
			0644); err != nil {
			t.Fatal(err)
		}
	}

	build := exec.CommandContext(ctx, "distri", "bump", "-w", "lvm2")
	build.Env = []string{"DISTRIROOT=" + distriroot}
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("%v: %v", build.Args, err)
	}

	for _, tt := range []struct {
		pkg  string
		want string
	}{
		{pkg: "lvm2", want: "2.03.00-4"},
		{pkg: "systemd", want: "239-8"},
		{pkg: "test", want: "530-2"},
		{pkg: "unaffected", want: "1-1"},
	} {
		t.Run(tt.pkg, func(t *testing.T) {
			var bld pb.Build
			b, err := ioutil.ReadFile(filepath.Join(distriroot, "pkgs", tt.pkg, "build.textproto"))
			if err != nil {
				t.Fatal(err)
			}
			if err := prototext.Unmarshal(b, &bld); err != nil {
				t.Fatal(err)
			}
			if got, want := bld.GetVersion(), tt.want; got != want {
				t.Errorf("bump: got %q, want %q", got, want)
			}
		})
	}
}
