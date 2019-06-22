package main

import "github.com/distr1/distri/pb"

func (b *buildctx) buildcmake(opts *pb.CMakeBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	steps := [][]string{
		append([]string{
			"/bin/cmake",
			"${DISTRI_SOURCEDIR}",
			"-DCMAKE_INSTALL_PREFIX:PATH=${DISTRI_PREFIX}",
			"-DCMAKE_VERBOSE_MAKEFILE:BOOL=ON",
			"-G", "Ninja",
		}, opts.GetExtraCmakeFlag()...),
		[]string{"ninja", "-v"},
		[]string{
			"/bin/sh",
			"-c",
			"DESTDIR=${DISTRI_DESTDIR} ninja -v install",
		},
	}
	return stepsToProto(steps), env, nil
}
