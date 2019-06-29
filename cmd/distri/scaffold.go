package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"golang.org/x/xerrors"
)

const scaffoldHelp = `TODO
`

var buildTmpl = template.Must(template.New("").Parse(`source: "{{.Source}}"
hash: "{{.Hash}}"
version: "{{.Version}}-1"

{{.Builder}}: <>

# build dependencies:
`))

func nameFromURL(parsed *url.URL, scaffoldType int) (name string, version string, _ error) {
	if parsed.Host == "github.com" {
		parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
		_ = parts[0] // org/user
		name = parts[1]
		_ = parts[2] // “archive”
		version = trimArchiveSuffix(strings.TrimPrefix(parts[3], "v"))
		return name, version, nil
	}
	pkg := trimArchiveSuffix(filepath.Base(parsed.String()))
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

func (c *scaffoldctx) scaffold1() error {
	b := &buildctx{
		Proto: &pb.Build{
			Source: proto.String(c.SourceURL),
		},
	}
	builddir := filepath.Join(env.DistriRoot, "build", c.Name)
	if err := os.MkdirAll(builddir, 0755); err != nil {
		return err
	}
	if err := os.Chdir(builddir); err != nil {
		return err
	}
	fn := filepath.Base(c.SourceURL)
	if c.ScaffoldType == scaffoldGomod {
		fn += ".tar.gz"
	}
	if err := b.download(fn); err != nil {
		return err
	}

	h := sha256.New()
	f, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return err
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
		Hash:    fmt.Sprintf("%x", h.Sum(nil)),
		Version: c.Version,
		Builder: builder,
	}); err != nil {
		return err
	}

	pkgdir := filepath.Join(env.DistriRoot, "pkgs", c.Name)
	if err := os.MkdirAll(pkgdir, 0755); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(pkgdir, "build.textproto"), buf.Bytes(), 0644); err != nil {
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

func scaffold(args []string) error {
	fset := flag.NewFlagSet("scaffold", flag.ExitOnError)
	var (
		name    = fset.String("name", "", "If non-empty and specified with -version, overrides the detected package name")
		version = fset.String("version", "", "If non-empty and specified with -name, overrides the detected package version")
		gomod   = fset.String("gomod", "", "if non-empty, a path to a go.mod file from which to take targets to scaffold")
	)
	fset.Parse(args)
	if *gomod != "" {
		return scaffoldGo(*gomod)
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
