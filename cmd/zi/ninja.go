package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"text/template"

	"github.com/golang/protobuf/proto"
	"github.com/stapelberg/zi/pb"
)

// TODO: add an implicit dependency (syntax: “ | foo”) on the zi build tool itself
// TODO: add implicit dependencies for the package deps
const ninjaTemplate = `
rule pkg
  command = cd $$(dirname $in) && zi build

{{ range .Packages }}
build {{ .OutputFile }}: pkg {{ .BuildFile }} | {{ range .BuildDeps }}{{ . }} {{ end }}
{{ end }}
`

var ninjaTmpl = template.Must(template.New("build.ninja").Parse(ninjaTemplate))

func ninja(args []string) error {
	fset := flag.NewFlagSet("ninja", flag.ExitOnError)
	fset.Parse(args)

	ziDir := filepath.Join(os.Getenv("HOME"), "zi")
	pkgsDir := filepath.Join(ziDir, "pkgs")
	fis, err := ioutil.ReadDir(pkgsDir)
	if err != nil {
		return err
	}

	type packageInfo struct {
		Name    string // e.g. busybox
		Version string // e.g. 1.29.2

		// build definition file, relative to the zi directory,
		// e.g. pkgs/busybox/build.textproto
		BuildFile string

		// resulting file, relative to the zi directory,
		// e.g. build/zi/pkg/busybox-1.29.2.squashfs
		OutputFile string

		// build dependency file names, relative to the zi directory,
		// e.g. [build/zi/pkg/glibc-2.27.squashfs, build/zi/pkg/zlib-1.13.squashfs]
		BuildDeps []string
	}

	pkgs := make([]packageInfo, len(fis))
	for idx, fi := range fis {
		pkg := fi.Name()

		// TODO(later): parallelize?
		c, err := ioutil.ReadFile(filepath.Join(pkgsDir, fi.Name(), "build.textproto"))
		if err != nil {
			return err
		}
		var buildProto pb.Build
		if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
			return err
		}

		version := buildProto.GetVersion()

		deps := make([]string, 0, len(buildProto.GetDep()))
		for _, dep := range buildProto.GetDep() {
			if dep == pkg+"-"+version {
				continue // ninja does not support cyclical dependencies
			}
			deps = append(deps, filepath.Join("build", "zi", "pkg", dep+".squashfs"))
		}

		pkgs[idx] = packageInfo{
			Name:       pkg,
			Version:    version,
			BuildFile:  filepath.Join("pkgs", pkg, "build.textproto"),
			OutputFile: filepath.Join("build", "zi", "pkg", pkg+"-"+version+".squashfs"),
			BuildDeps:  deps,
		}
	}

	f, err := ioutil.TempFile(ziDir, "zi-ninja")
	if err != nil {
		return err
	}
	if err := ninjaTmpl.Execute(f, struct {
		Packages []packageInfo
	}{
		Packages: pkgs,
	}); err != nil {
		return err
	}
	ninjaFile := filepath.Join(ziDir, "build.ninja")
	if err := os.Rename(f.Name(), ninjaFile); err != nil {
		return err
	}

	log.Printf("ninja build file %s written", ninjaFile)

	return nil
}
