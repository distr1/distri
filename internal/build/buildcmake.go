package build

import (
	"strconv"

	"github.com/distr1/distri/pb"
)

func (b *Ctx) buildcmake(opts *pb.CMakeBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	dir := "${DISTRI_SOURCEDIR}"
	configure := append([]string{
		"/bin/cmake",
		dir,
		"-DCMAKE_INSTALL_PREFIX:PATH=${DISTRI_PREFIX}",
		"-DCMAKE_VERBOSE_MAKEFILE:BOOL=ON",
		"-G", "Ninja",
	}, opts.GetExtraCmakeFlag()...)
	// TODO: apply this unconditionally (rebuild all packages)
	if b.Arch != "amd64" {
		// From https://gitlab.kitware.com/cmake/community/-/wikis/doc/cmake/CrossCompiling:
		// If this compiler is a gcc-cross compiler with a prefixed name
		// (e.g. "arm-elf-gcc") CMake will detect this and automatically find
		// the corresponding C++ compiler (i.e. "arm-elf-c++").
		configure = append(configure, "-DCMAKE_C_COMPILER="+configureTarget[b.Arch]+"-gcc")
	}
	var steps [][]string
	steps = append(steps, [][]string{
		configure,
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
