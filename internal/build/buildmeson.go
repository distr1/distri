package build

import (
	"strconv"

	"github.com/distr1/distri/pb"
)

func (b *Ctx) buildmeson(opts *pb.MesonBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	var steps [][]string
	steps = append(steps, [][]string{
		append([]string{
			"meson",
			"--prefix=${DISTRI_PREFIX}",
			"--sysconfdir=/etc",
			".", // build dir
			"${DISTRI_SOURCEDIR}",
		}, opts.GetExtraMesonFlag()...),
	}...)

	steps = append(steps, [][]string{
		// It makes sense to pass an explicit -j argument to ninja, as within
		// the build environment there is no /proc, and ninja falls back to only
		// 3 jobs.
		[]string{"ninja", "-v", "-j", strconv.Itoa(b.Jobs)},
		[]string{
			"/bin/sh",
			"-c",
			"DESTDIR=${DISTRI_DESTDIR} ninja -v -j " + strconv.Itoa(b.Jobs) + " install",
		},
	}...)
	return stepsToProto(steps), env, nil
}
