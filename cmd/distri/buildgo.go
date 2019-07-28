package main

import (
	"net/url"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/pb"
)

func goPkgToImportPath(pkg string) string {
	importPath := strings.TrimPrefix(pkg, "go-")
	importPath = strings.ReplaceAll(importPath, "-", "/")
	importPath = strings.ReplaceAll(importPath, "//", "-")
	return importPath
}

func (b *buildctx) buildgo(opts *pb.GoBuilder, env []string, deps []string, source string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	// Add replace directives to go.mod for the transitive closure of
	// dependencies, instructing the go tool to select the version we made
	// available as a build dependency.
	replace := make([]string, 0, len(deps))
	for _, dep := range deps {
		pv := distri.ParseVersion(dep)
		if !strings.HasPrefix(pv.Pkg, "go-") {
			continue
		}
		importPath := goPkgToImportPath(pv.Pkg)
		replace = append(replace, "-replace "+importPath+"="+importPath+"@"+pv.Upstream)
	}

	importPath := opts.GetImportPath()
	if importPath == "" {
		if u, err := url.Parse(source); err == nil {
			importPath = u.Host + u.Path
			if idx := strings.Index(importPath, "@v"); idx > -1 {
				importPath = importPath[:idx]
			}
		}
	}

	gotool := func(args string) []string {
		return []string{"/bin/sh", "-c", "GOSUMDB=off GOCACHE=/tmp/throwaway GOPATH=/tmp/gopath GOPROXY=off " + args}
	}

	steps := [][]string{
		// TODO: do we need this? []string{"/bin/sh", "-c", "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/gopath/; mkdir -p $d && cp -r ${DISTRI_SOURCEDIR}/* $d"},

		// Put together a GOPATH with a writable module cache, because “go
		// install” uses a side lock located in the module cache around go.mod
		// and go.sum updates, which are currently required. See also:
		// https://github.com/golang/go/issues/33326
		[]string{"/bin/sh", "-c", "MODCACHE=/tmp/gopath/pkg/mod/cache; mkdir -p $MODCACHE && ln -s /ro/gopath/pkg/mod/cache/download $MODCACHE && for dir in $(ls -d /ro/gopath/pkg/mod/* | grep -v '/cache$'); do ln -s $dir /tmp/gopath/pkg/mod/; done"},

		// Make a writable source dir copy so that we can update go.mod
		[]string{"/bin/sh", "-c", "cp -T -ar ${DISTRI_SOURCEDIR}/pkg/mod/" + importPath + "@v*/ ."},

		// Overwrite all versions with latest (will be resolved with the following go install):
		gotool("go mod edit " + strings.Join(replace, " ")),

		gotool("GOBIN=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/bin go install -v " + opts.GetInstall()),
	}

	return stepsToProto(steps), env, nil
}
