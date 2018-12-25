package main

import (
	"github.com/distr1/distri/pb"
)

func (b *buildctx) buildgomod(opts *pb.GomodBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	steps := [][]string{
		[]string{"/bin/sh", "-c", "d=${ZI_DESTDIR}/${ZI_PREFIX}/gopath/; mkdir -p $d && cp -r ${ZI_SOURCEDIR}/* $d"},
	}
	return stepsToProto(steps), env, nil
}
