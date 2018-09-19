package main

import (
	"io"
	"log"
	"os"
	"os/exec"

	"github.com/stapelberg/zi/pb"
)

func (b *buildctx) buildperl(opts *pb.PerlBuilder, env []string, buildLog io.Writer) error {
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

	for idx, step := range steps {
		cmd := exec.Command(b.substitute(step[0]), b.substituteStrings(step[1:])...)
		if b.Hermetic {
			cmd.Env = env
		}
		log.Printf("build step %d of %d: %v", idx+1, len(steps), cmd.Args)
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
