package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/checkupstream"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
	"github.com/protocolbuffers/txtpbfmt/ast"
	"github.com/protocolbuffers/txtpbfmt/parser"
	"golang.org/x/xerrors"
)

const scaffoldHelp = `distri scaffold [-flags] <upstream-source-url>

Generate distri package build instructions from an upstream source.

Example:
  % distri scaffold https://releases.pagure.org/xmlto/xmlto-0.0.28.tar.bz2
  % distri build -pkg xmlto
`

var buildTmpl = template.Must(template.New("").Parse(`source: "{{.Source}}"
hash: "{{.Hash}}"
version: "{{.Version}}-1"

{{.Builder}}: {}

# build dependencies:
`))

func nameFromURL(parsed *url.URL, scaffoldType int) (name string, version string, _ error) {
	if parsed.Host == "github.com" {
		parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
		_ = parts[0] // org/user
		name = parts[1]
		_ = parts[2] // “archive”
		version = build.TrimArchiveSuffix(strings.TrimPrefix(parts[3], "v"))
		return name, version, nil
	}
	pkg := build.TrimArchiveSuffix(filepath.Base(parsed.String()))
	pkg = strings.ReplaceAll(pkg, "_", "-")
	idx := strings.LastIndex(pkg, "-")
	if idx == -1 {
		return "", "", xerrors.Errorf("could not segment %q into <name>-<version>", pkg)
	}

	name = strings.ToLower(pkg[:idx])
	version = pkg[idx+1:]
	if scaffoldType == scaffoldPerl {
		name = "perl-" + pkg[:idx]
	}
	return name, version, nil
}

const (
	scaffoldC = iota
	scaffoldPerl
	scaffoldGomod
)

type scaffoldctx struct {
	ScaffoldType int    // e.g. scaffoldPerl
	SourceURL    string // e.g. “https://ftp.gnu.org/pub/gcc-8.2.0.tar.gz”
	Name         string // e.g. “gcc”
	Version      string // e.g. “8.2.0”
}

func (c *scaffoldctx) buildFileExisting(buildFilePath string, hash string, existing []byte) ([]byte, error) {
	nodes, err := parser.Parse(existing)
	if err != nil {
		return nil, err
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
	var mod bool
	modVersion, err := replaceStringVal(path("version"), func(val string) string {
		pv := distri.ParseVersion(val)
		pv.Upstream = c.Version
		return pv.Upstream + "-" + strconv.FormatInt(pv.DistriRevision, 10)
	})
	if err != nil {
		return nil, err
	}
	mod = mod || modVersion

	modSource, err := replaceStringVal(path("source"), func(string) string { return c.SourceURL })
	if err != nil {
		return nil, err
	}
	mod = mod || modSource

	modHash, err := replaceStringVal(path("hash"), func(string) string { return hash })
	if err != nil {
		return nil, err
	}
	mod = mod || modHash
	if mod {
		if _, err := replaceStringVal(path("version"), func(val string) string {
			pv := distri.ParseVersion(val)
			pv.DistriRevision++
			return pv.Upstream + "-" + strconv.FormatInt(pv.DistriRevision, 10)
		}); err != nil {
			return nil, err
		}
	}

	return []byte(parser.Pretty(nodes, 0)), nil
}

func (c *scaffoldctx) buildFile(hash string) ([]byte, error) {
	buildFilePath := filepath.Join(env.DistriRoot, "pkgs", c.Name, "build.textproto")
	if existing, err := ioutil.ReadFile(buildFilePath); err == nil {
		return c.buildFileExisting(buildFilePath, hash, existing)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	builder := "cbuilder"
	switch c.ScaffoldType {
	case scaffoldPerl:
		builder = "perlbuilder"
	case scaffoldGomod:
		builder = "gomodbuilder"
	}
	var buf bytes.Buffer
	if err := buildTmpl.Execute(&buf, struct {
		Source  string
		Hash    string
		Version string
		Builder string
	}{
		Source:  c.SourceURL,
		Hash:    hash,
		Version: c.Version,
		Builder: builder,
	}); err != nil {
		return nil, err
	}
	b, err := parser.Format(buf.Bytes())
	if err != nil {
		return nil, err
	}
	return b, nil
}

func download1(name, sourceURL string) (string, error) {
	b := &build.Ctx{
		Proto: &pb.Build{
			Source: proto.String(sourceURL),
		},
		Repo: env.DefaultRepo,
	}
	builddir := filepath.Join(env.DistriRoot, "build", name)
	if err := os.MkdirAll(builddir, 0755); err != nil {
		return "", err
	}
	if err := os.Chdir(builddir); err != nil {
		return "", err
	}
	fn := filepath.Base(sourceURL)
	u, err := url.Parse(sourceURL)
	if err != nil {
		return "", fmt.Errorf("url.Parse: %v", err)
	}
	if u.Scheme == "distri+gomod" {
		fn += ".tar.gz"
	}

	if err := b.Download(fn); err != nil {
		return "", err
	}
	return b.Hash(fn)
}

func (c *scaffoldctx) scaffold1() error {
	hash, err := download1(c.Name, c.SourceURL)
	if err != nil {
		return err
	}
	buf, err := c.buildFile(hash)
	if err != nil {
		return err
	}

	pkgdir := filepath.Join(env.DistriRoot, "pkgs", c.Name)
	if err := os.MkdirAll(pkgdir, 0755); err != nil {
		return err
	}
	if err := renameio.WriteFile(filepath.Join(pkgdir, "build.textproto"), buf, 0644); err != nil {
		return err
	}
	return nil
}

func scaffoldGo(gomod string) error {
	dir, err := filepath.Abs(filepath.Dir(gomod))
	if err != nil {
		return err
	}
	gotool := exec.Command("go", "list", "-m", "-json", "all")
	gotool.Dir = dir
	gotool.Stderr = os.Stderr
	b, err := gotool.Output()
	if err != nil {
		return xerrors.Errorf("%v: %v", gotool.Args, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var mod struct {
			Path    string
			Version string
			Main    bool
		}
		if err := dec.Decode(&mod); err != nil {
			return err
		}
		if mod.Main {
			continue
		}
		name := "go-" + strings.Replace(mod.Path, "-", "--", -1)
		name = strings.Replace(name, "/", "-", -1)
		c := scaffoldctx{
			ScaffoldType: scaffoldGomod,
			SourceURL:    fmt.Sprintf("distri+gomod://%s@%s", mod.Path, mod.Version),
			Name:         name,
			Version:      mod.Version,
		}
		if err := c.scaffold1(); err != nil {
			return err
		}
	}
	return nil
}

func scaffoldPull(pkg, buildFilePath string, dryRun bool) error {
	b, err := ioutil.ReadFile(buildFilePath)
	if err != nil {
		return err
	}
	nodes, err := parser.Parse(b)
	if err != nil {
		return err
	}
	stringVal := func(path ...string) (string, error) {
		nodes := ast.GetFromPath(nodes, path)
		if got, want := len(nodes), 1; got != want {
			return "", fmt.Errorf("malformed build file: %s: got %d version keys, want %d", buildFilePath, got, want)
		}
		values := nodes[0].Values
		if got, want := len(values), 1; got != want {
			return "", fmt.Errorf("malformed build file: %s: got %d Values, want %d", buildFilePath, got, want)
		}
		return strconv.Unquote(values[0].Value)
	}
	version, err := stringVal("version")
	if err != nil {
		return err
	}
	upstream := distri.ParseVersion(version).Upstream

	remoteSource, remoteHash, remoteVersion, err := checkupstream.Check(nodes)
	if err != nil {
		return err
	}

	if remoteVersion == upstream {
		log.Printf("up to date: %s", remoteVersion)
		return nil // up to date
	}
	log.Printf("not up to date: updating from %s to %s", upstream, remoteVersion)

	if remoteHash == "" {
		var err error
		remoteHash, err = download1(pkg, remoteSource)
		if err != nil {
			return err
		}
	}

	val := strconv.QuoteToASCII(remoteSource)
	ast.GetFromPath(nodes, []string{"source"})[0].Values[0].Value = val

	val = strconv.QuoteToASCII(remoteHash)
	ast.GetFromPath(nodes, []string{"hash"})[0].Values[0].Value = val

	pv := distri.ParseVersion(version)
	if pv.Upstream != remoteVersion {
		pv.Upstream = remoteVersion
		pv.DistriRevision++
		val := strconv.QuoteToASCII(pv.Upstream + "-" + strconv.FormatInt(pv.DistriRevision, 10))
		ast.GetFromPath(nodes, []string{"version"})[0].Values[0].Value = val
	}

	if dryRun {
		os.Exit(2) // outdated
	}

	buf := []byte(parser.Pretty(nodes, 0))
	if bytes.Equal(buf, b) {
		return nil
	}
	if err := renameio.WriteFile(buildFilePath, buf, 0644); err != nil {
		return err
	}

	return nil
}

func scaffold(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("scaffold", flag.ExitOnError)
	var (
		name    = fset.String("name", "", "If non-empty and specified with -version, overrides the detected package name")
		version = fset.String("version", "", "If non-empty and specified with -name, overrides the detected package version")
		gomod   = fset.String("gomod", "", "if non-empty, a path to a go.mod file from which to take targets to scaffold")
		pull    = fset.String("pull", "", "if non-empty, package name to update to its latest version")
		dryRun  = fset.Bool("dry_run", false, "dry run")
	)
	fset.Usage = usage(fset, scaffoldHelp)
	fset.Parse(args)
	if *gomod != "" {
		return scaffoldGo(*gomod)
	}
	if *pull != "" {
		buildFilePath := filepath.Join(env.DistriRoot, "pkgs", *pull, "build.textproto")
		return scaffoldPull(*pull, buildFilePath, *dryRun)
	}
	if fset.NArg() != 1 {
		return xerrors.Errorf("syntax: scaffold <url>")
	}
	u := fset.Arg(0)
	parsed, err := url.Parse(u)
	if err != nil {
		return xerrors.Errorf("could not parse URL %q: %v", u, err)
	}
	var scaffoldType int
	if parsed.Host == "cpan.metacpan.org" {
		scaffoldType = scaffoldPerl
	}

	if *name == "" || *version == "" {
		var err error
		*name, *version, err = nameFromURL(parsed, scaffoldType)
		if err != nil {
			return xerrors.Errorf("nameFromURL: %w", err)
		}
	}

	c := scaffoldctx{
		ScaffoldType: scaffoldType,
		SourceURL:    u,
		Name:         *name,
		Version:      *version,
	}
	return c.scaffold1()
}
