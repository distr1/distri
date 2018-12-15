package main

import (
	"github.com/distr1/distri/pb"
)

func (b *buildctx) buildpython(opts *pb.PythonBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	steps := [][]string{
		[]string{"python3", "setup.py", "install", "--prefix=${ZI_PREFIX}", "--root=${ZI_DESTDIR}"},
	}
	return stepsToProto(steps), env, nil
}
