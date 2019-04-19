package main

import "github.com/distr1/distri/pb"

func (b *buildctx) buildcmake(opts *pb.CMakeBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	steps := [][]string{
		[]string{"/bin/cmake", "${DISTRI_SOURCEDIR}",
			"-DCMAKE_INSTALL_PREFIX:PATH=${DISTRI_PREFIX}",
			"-DCMAKE_VERBOSE_MAKEFILE:BOOL=ON",
		},
		[]string{"make", "-j8"},
		[]string{"make", "install", "DESTDIR=${DISTRI_DESTDIR}", "PREFIX=${DISTRI_PREFIX}"},
	}
	return stepsToProto(steps), env, nil
}
