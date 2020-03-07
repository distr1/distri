package batch_test

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/batch"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/distritest/buildtest"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/protocolbuffers/txtpbfmt/ast"
	"github.com/protocolbuffers/txtpbfmt/parser"
)

func TestRebuild(t *testing.T) {
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	distriroot, err := ioutil.TempDir("", "integrationbuild")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, distriroot)

	// arranges for these inputs
	// - libxcb 1.13-5 (meta/squashfs)
	// - libxcb 1.13-6 (build.textproto)
	// - i3 4.17-7 (meta/squashfs)
	// - i3 4.17-7 (build.textproto)

	// Copy build dependencies (and their build.textproto) into our temporary DISTRIROOT:
	repo := filepath.Join(distriroot, "build", "distri", "pkg")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	pkgs := filepath.Join(distriroot, "pkgs")
	if err := os.MkdirAll(pkgs, 0755); err != nil {
		t.Fatal(err)
	}
	deps := buildtest.Builddeps(t, "libxcb", "i3")
	log.Printf("copying %d build dependencies to distritest.DistriRoot=%v:", len(deps), distritest.DistriRoot)
	for _, dep := range deps {
		log.Printf("  %v", dep)
		metaTextproto := filepath.Join(env.DefaultRepo, dep+".meta.textproto")
		cp := exec.CommandContext(ctx, "cp",
			filepath.Join(env.DefaultRepo, dep+".squashfs"),
			metaTextproto,
			repo)
		cp.Stderr = os.Stderr
		if err := cp.Run(); err != nil {
			t.Fatalf("%v: %v", cp.Args, err)
		}

		meta, err := pb.ReadMetaFile(metaTextproto)
		if err != nil {
			t.Fatal(err)
		}
		pkgDir := filepath.Join(env.DistriRoot, "pkgs", meta.GetSourcePkg())
		cp = exec.Command("cp", "-r", pkgDir, pkgs)
		cp.Stderr = os.Stderr
		if err := cp.Run(); err != nil {
			t.Fatalf("%v: %v", cp.Args, err)
		}
	}

	// bump distri revision of libxcb
	{
		buildFilePath := filepath.Join(distriroot, "pkgs/libxcb/build.textproto")
		existing, err := ioutil.ReadFile(buildFilePath)
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := parser.Parse(existing)
		if err != nil {
			t.Fatal(err)
		}

		replaceStringVal := func(nodes []*ast.Node, repl func(string) string) (modified bool, _ error) {
			if got, want := len(nodes), 1; got != want {
				return false, fmt.Errorf("malformed build file: %s: got %d version keys, want %d", buildFilePath, got, want)
			}
			values := nodes[0].Values
			if got, want := len(values), 1; got != want {
				return false, fmt.Errorf("malformed build file: %s: got %d Values, want %d", buildFilePath, got, want)
			}
			unq, err := strconv.Unquote(values[0].Value)
			if err != nil {
				return false, err
			}
			val := strconv.QuoteToASCII(repl(unq))
			if val != values[0].Value {
				values[0].Value = val
				return true, nil
			}
			return false, nil
		}
		path := func(last string) []*ast.Node { return ast.GetFromPath(nodes, []string{last}) }
		_, err = replaceStringVal(path("version"), func(val string) string {
			pv := distri.ParseVersion(val)
			pv.DistriRevision++
			return pv.Upstream + "-" + strconv.FormatInt(pv.DistriRevision, 10)
		})
		if err != nil {
			t.Fatal(err)

		}
		if err := ioutil.WriteFile(buildFilePath, []byte(parser.Pretty(nodes, 0)), 0644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	bctx := &batch.Ctx{
		Log:        log.New(&buf, "", 0 /* no time and date for easier processing */),
		DistriRoot: distriroot,
		DefaultBuildCtx: &build.Ctx{
			Arch: "amd64", // TODO
			Repo: repo,
		},
	}
	const dryRun = true
	if err := bctx.Build(ctx, dryRun, false, false, runtime.NumCPU()); err != nil {
		t.Fatal(err)
	}

	// Ensure at least libxcb itself and reverse-dependency i3 are re-built.
	var got []string
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "  build ") {
			continue
		}
		pkg := strings.TrimPrefix(line, "  build ")
		got = append(got, pkg)
	}
	sort.Strings(got)
	want := []string{"i3", "libxcb"}
	opts := []cmp.Option{cmpopts.IgnoreSliceElements(func(pkg string) bool {
		return pkg != "i3" && pkg != "libxcb"
	})}
	if diff := cmp.Diff(want, got, opts...); diff != "" {
		t.Fatalf("distri batch: unexpected package rebuild list: diff (-want +got):\n%s", diff)
	}
}
