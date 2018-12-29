package main

import (
	"github.com/distr1/distri/pb"
)

func (b *buildctx) buildgomod(opts *pb.GomodBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	steps := [][]string{
		[]string{"/bin/sh", "-c", "d=${DISTRI_DESTDIR}/${DISTRI_PREFIX}/gopath/; mkdir -p $d && cp -r ${DISTRI_SOURCEDIR}/* $d"},
	}
	return stepsToProto(steps), env, nil
}
