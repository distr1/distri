package main

import (
	"github.com/distr1/distri/pb"
)

func (b *buildctx) buildperl(opts *pb.PerlBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	var steps [][]string
	// TODO: Perl distributions generally don’t seem to build out-of-source? If they start doing so, stop copying
	steps = [][]string{
		[]string{"cp", "-T", "-ar", "${DISTRI_SOURCEDIR}/", "."},
	}

	steps = append(steps, [][]string{
		append([]string{"perl", "Makefile.PL", "INSTALL_BASE=${DISTRI_PREFIX}", "PREREQ_FATAL=true"}, opts.GetExtraMakefileFlag()...),
		// TODO: the problem with V=1 is that it typically doesn’t apply to recursive make invocations (e.g. mesa)
		[]string{"make", "-j8", "V=1"},
		[]string{"make", "install", "DESTDIR=${DISTRI_DESTDIR}"},
	}...)

	return stepsToProto(steps), env, nil
}
