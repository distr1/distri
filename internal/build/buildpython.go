package build

import (
	"github.com/distr1/distri/pb"
)

func (b *Ctx) buildpython(opts *pb.PythonBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	steps := [][]string{
		[]string{"cp", "-T", "-ar", "${DISTRI_SOURCEDIR}/", "."},
		[]string{"python3", "setup.py", "install", "--prefix=${DISTRI_PREFIX}", "--root=${DISTRI_DESTDIR}"},
	}
	return stepsToProto(steps), env, nil
}
