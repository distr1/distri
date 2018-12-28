package main

import "github.com/distr1/distri/pb"

func (b *buildctx) buildcmake(opts *pb.CMakeBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	steps := [][]string{
		[]string{"/bin/cmake", "${ZI_SOURCEDIR}", "-DCMAKE_INSTALL_PREFIX:PATH=${ZI_PREFIX}"},
		[]string{"make", "-j8", "V=1"},
		[]string{"make", "install", "DESTDIR=${ZI_DESTDIR}", "PREFIX=${ZI_PREFIX}"},
	}
	return stepsToProto(steps), env, nil
}
