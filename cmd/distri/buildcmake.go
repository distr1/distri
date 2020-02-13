package main

import (
	"runtime"
	"strconv"

	"github.com/distr1/distri/pb"
)

func (b *buildctx) buildcmake(opts *pb.CMakeBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	dir := "${DISTRI_SOURCEDIR}"
	var steps [][]string
	if opts.GetCopyToBuilddir() {
		dir = "."
		steps = [][]string{
			[]string{"cp", "-T", "-ar", "${DISTRI_SOURCEDIR}/", "."},
		}
	}
	steps = append(steps, [][]string{
		append([]string{
			"/bin/cmake",
			dir,
			"-DCMAKE_INSTALL_PREFIX:PATH=${DISTRI_PREFIX}",
			"-DCMAKE_VERBOSE_MAKEFILE:BOOL=ON",
			"-G", "Ninja",
		}, opts.GetExtraCmakeFlag()...),
		// It makes sense to pass an explicit -j argument to ninja, as within
		// the build environment there is no /proc, and ninja falls back to only
		// 3 jobs.
		[]string{"ninja", "-v", "-j", strconv.Itoa(runtime.NumCPU())},
		[]string{
			"/bin/sh",
			"-c",
			"DESTDIR=${DISTRI_DESTDIR} ninja -v -j " + strconv.Itoa(runtime.NumCPU()) + " install",
		},
	}...)
	return stepsToProto(steps), env, nil
}
