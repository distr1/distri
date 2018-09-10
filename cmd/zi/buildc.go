package main

import (
	"io"
	"log"
	"os"
	"os/exec"

	"github.com/stapelberg/zi/pb"
)

func (b *buildctx) buildc(opts *pb.CBuilder, env []string, buildLog io.Writer) error {
	// e.g. ncurses needs DESTDIR in the configure step, too, so just export it for all steps.
	env = append(env, b.substitute("DESTDIR=${ZI_DESTDIR}"))

	//  argv: "cp -T -ar ${ZI_SOURCEDIR}/ ."

	var steps [][]string
	if opts.GetCopyToBuilddir() {
		steps = [][]string{
			[]string{"cp", "-T", "-ar", "${ZI_SOURCEDIR}/", "."},
			append([]string{"./configure", "--prefix=${ZI_PREFIX}"}, opts.GetExtraConfigureFlag()...),
		}
	} else {
		steps = [][]string{
			// TODO: set --disable-silent-rules if found in configure help output
			append([]string{"${ZI_SOURCEDIR}/configure", "--prefix=${ZI_PREFIX}"}, opts.GetExtraConfigureFlag()...),
		}
	}
	steps = append(steps, [][]string{
		[]string{"make", "-j8", "V=1"},
		// e.g. help2man doesn’t pick up the environment variable
		[]string{"make", "install",
			b.substitute("DESTDIR=${ZI_DESTDIR}"),
			b.substitute("PREFIX=${ZI_PREFIX}"), // e.g. for iputils
		},
	}...)

	for idx, step := range steps {
		cmd := exec.Command(b.substitute(step[0]), b.substituteStrings(step[1:])...)
		if b.Hermetic {
			cmd.Env = env
		}
		log.Printf("build step %d of %d: %v", idx, len(steps), cmd.Args)
		cmd.Stdin = os.Stdin // for interactive debugging
		// TODO: logging with io.MultiWriter results in output no longer being colored, e.g. during the systemd build. any workaround?
		cmd.Stdout = io.MultiWriter(os.Stdout, buildLog)
		cmd.Stderr = io.MultiWriter(os.Stderr, buildLog)
		if err := cmd.Run(); err != nil {
			// TODO: ask the user first if they want to debug, and only during interactive builds. detect pty?
			// TODO: ring the bell :)
			log.Printf("build step failed (%v), starting debug shell", err)
			cmd := exec.Command("bash", "-i")
			if b.Hermetic {
				cmd.Env = env
			}
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				log.Printf("debug command failed: %v", err)
			}
			return err
		}
	}

	return nil
}
