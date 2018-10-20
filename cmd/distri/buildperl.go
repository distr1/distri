package main

import (
	"github.com/stapelberg/zi/pb"
)

func (b *buildctx) buildperl(opts *pb.PerlBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	var steps [][]string
	// TODO: Perl distributions generally don’t seem to build out-of-source? If they start doing so, stop copying
	steps = [][]string{
		[]string{"cp", "-T", "-ar", "${ZI_SOURCEDIR}/", "."},
	}

	steps = append(steps, [][]string{
		append([]string{"perl", "Makefile.PL", "INSTALL_BASE=${ZI_PREFIX}", "PREREQ_FATAL=true"}, opts.GetExtraMakefileFlag()...),
		// TODO: the problem with V=1 is that it typically doesn’t apply to recursive make invocations (e.g. mesa)
		[]string{"make", "-j8", "V=1"},
		[]string{"make", "install", "DESTDIR=${ZI_DESTDIR}"},
	}...)

	return stepsToProto(steps), env, nil
}
