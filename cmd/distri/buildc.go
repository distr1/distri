package main

import (
	"github.com/stapelberg/zi/pb"
)

func (b *buildctx) buildc(opts *pb.CBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	// e.g. ncurses needs DESTDIR in the configure step, too, so just export it for all steps.
	env = append(env, b.substitute("DESTDIR=${ZI_DESTDIR}"))

	var steps [][]string
	if opts.GetCopyToBuilddir() {
		steps = [][]string{
			[]string{"cp", "-T", "-ar", "${ZI_SOURCEDIR}/", "."},
			append([]string{"./configure", "--prefix=${ZI_PREFIX}", "--sysconfdir=/etc", "--disable-dependency-tracking"}, opts.GetExtraConfigureFlag()...),
		}
	} else {
		steps = [][]string{
			// TODO: set --disable-silent-rules if found in configure help output
			append([]string{"${ZI_SOURCEDIR}/configure", "--prefix=${ZI_PREFIX}", "--sysconfdir=/etc", "--disable-dependency-tracking"}, opts.GetExtraConfigureFlag()...),
		}
	}
	steps = append(steps, [][]string{
		// TODO: the problem with V=1 is that it typically doesn’t apply to recursive make invocations (e.g. mesa)
		append([]string{"make", "-j8", "V=1"}, opts.GetExtraMakeFlag()...),
		// e.g. help2man doesn’t pick up the environment variable
		append([]string{"make", "install",
			"DESTDIR=${ZI_DESTDIR}",
			"PREFIX=${ZI_PREFIX}", // e.g. for iputils
		}, opts.GetExtraMakeFlag()...),
	}...)
	return stepsToProto(steps), env, nil
}
