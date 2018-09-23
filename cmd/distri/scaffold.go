package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/golang/protobuf/proto"
	"github.com/stapelberg/zi/pb"
)

var buildTmpl = template.Must(template.New("").Parse(`source: "{{.Source}}"
hash: "{{.Hash}}"
version: "{{.Version}}"

{{.Builder}}: <>

# build dependencies:
`))

const (
	scaffoldC = iota
	scaffoldPerl
)

func scaffold(args []string) error {
	fset := flag.NewFlagSet("scaffold", flag.ExitOnError)
	fset.Parse(args)
	if fset.NArg() != 1 {
		return fmt.Errorf("syntax: scaffold <url>")
	}
	u := fset.Arg(0)
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("could not parse URL %q: %v", u, err)
	}
	var scaffoldType int
	if parsed.Host == "cpan.metacpan.org" {
		scaffoldType = scaffoldPerl
	}

	pkg := filepath.Base(u)
	for _, suffix := range []string{"gz", "lz", "xz", "bz2", "tar"} {
		pkg = strings.TrimSuffix(pkg, "."+suffix)
	}
	idx := strings.LastIndex(pkg, "-")
	if idx == -1 {
		return fmt.Errorf("could not segment %q into <name>-<version>", pkg)
	}
	name := strings.ToLower(pkg[:idx])
	version := pkg[idx+1:]
	if scaffoldType == scaffoldPerl {
		name = "perl-" + pkg[:idx]
	}

	b := &buildctx{
		Proto: &pb.Build{
			Source: proto.String(u),
		},
	}
	builddir := filepath.Join(distriRoot, "build", name)
	if err := os.MkdirAll(builddir, 0755); err != nil {
		return err
	}
	if err := os.Chdir(builddir); err != nil {
		return err
	}
	fn := filepath.Base(u)
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
	if scaffoldType == scaffoldPerl {
		builder = "perlbuilder"
	}
	var buf bytes.Buffer
	if err := buildTmpl.Execute(&buf, struct {
		Source  string
		Hash    string
		Version string
		Builder string
	}{
		Source:  u,
		Hash:    fmt.Sprintf("%x", h.Sum(nil)),
		Version: version,
		Builder: builder,
	}); err != nil {
		return err
	}

	pkgdir := filepath.Join(distriRoot, "pkgs", name)
	if err := os.MkdirAll(pkgdir, 0755); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(pkgdir, "build.textproto"), buf.Bytes(), 0644); err != nil {
		return err
	}

	return nil
}
