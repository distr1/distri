package build

import (
	"strconv"

	"github.com/distr1/distri/pb"
	"golang.org/x/xerrors"
)

var configureTarget = map[string]string{
	"amd64": "x86_64-pc-linux-gnu",
	"i686":  "i686-pc-linux-gnu",
	"arm64": "aarch64-linux-gnu",
}

func (b *Ctx) buildc(opts *pb.Build, builder *pb.CBuilder, env []string) (newSteps []*pb.BuildStep, newEnv []string, _ error) {
	// e.g. ncurses needs DESTDIR in the configure step, too, so just export it for all steps.
	env = append(env, b.substitute("DESTDIR=${DISTRI_DESTDIR}"))

	target, ok := configureTarget[b.Arch]
	if !ok {
		return nil, nil, xerrors.Errorf("cbuilder: No target host set for architecture %s", b.Arch)
	}

	if builder.GetAutoreconf() && (!opts.GetWritableSourcedir() || !opts.GetInTreeBuild()) {
		return nil, nil, xerrors.Errorf("cbuilder: autoreconf requires enabling writable_sourcedir and in_tree_build")
	}

	var steps [][]string
	if opts.GetWritableSourcedir() {
		if builder.GetAutoreconf() {
			steps = append(steps, [][]string{
				[]string{"mkdir", "-p", "m4"},
				[]string{"/bin/sh", "-c", "command -v intltoolize && intltoolize --force --copy --automake || true"},
				[]string{"/bin/sh", "-c", "ACLOCAL_PATH=/ro/share/aclocal autoreconf --force --install"},
			}...)
		}
	}
	steps = append(steps, [][]string{
		// TODO: set --disable-silent-rules if found in configure help output
		// TODO: set --enable-debug=[yes/info/profile/no] to info if found in configure help
		append([]string{
			"${DISTRI_SOURCEDIR}/configure",
			"--host=" + target,
			"--prefix=${DISTRI_PREFIX}",
			"--sysconfdir=/etc",
			"--disable-dependency-tracking",
		}, builder.GetExtraConfigureFlag()...),
	}...)

	steps = append(steps, [][]string{
		// TODO: the problem with V=1 is that it typically doesn’t apply to recursive make invocations (e.g. mesa)
		append([]string{"make", "-j" + strconv.Itoa(b.Jobs), "V=1"}, builder.GetExtraMakeFlag()...),
		// e.g. help2man doesn’t pick up the environment variable
		append([]string{"make", "install",
			"DESTDIR=${DISTRI_DESTDIR}",
			"PREFIX=${DISTRI_PREFIX}", // e.g. for iputils
		}, builder.GetExtraMakeFlag()...),
	}...)
	return stepsToProto(steps), env, nil
}
