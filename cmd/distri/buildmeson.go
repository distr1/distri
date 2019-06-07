package main

import "github.com/distr1/distri/pb"

func (b *buildctx) buildmeson(opts *pb.MesonBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	var steps [][]string
	steps = append(steps, [][]string{
		append([]string{
			"meson",
			// TODO: should we set -Drootprefix or is that fuse-specific?
			"--prefix=${DISTRI_PREFIX}",
			".", // build dir
			"${DISTRI_SOURCEDIR}",
		}, opts.GetExtraMesonFlag()...),
	}...)

	steps = append(steps, [][]string{
		[]string{"ninja", "-v"},
		[]string{
			"/bin/sh",
			"-c",
			"DESTDIR=${DISTRI_DESTDIR} ninja -v install",
		},
	}...)
	return stepsToProto(steps), env, nil
}
