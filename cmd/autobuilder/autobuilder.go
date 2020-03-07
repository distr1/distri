package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/distr1/distri"
	"github.com/google/go-github/v27/github"
	"github.com/google/renameio"
	"golang.org/x/oauth2"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
)

var (
	accessToken = flag.String("github_access_token",
		"",
		"oauth2 GitHub access token")
)

type buildctx struct {
	Commit  string
	Workdir string
	DryRun  bool
	Rebuild string
}

func (b *buildctx) serialize() (string, error) {
	enc, err := json.Marshal(b)
	if err != nil {
		return "", err
	}

	tmp, err := ioutil.TempFile("", "distri")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := tmp.Write(enc); err != nil {
		return "", err
	}

	return tmp.Name(), tmp.Close()
}

type step struct {
	stamp string
	argv  []string
}

var steps = []step{
	// smoke tests: verify package building works with this version of distri
	{"smoke-quick", []string{"distri", "build", "-pkg=srcfs"}},
	{"smoke-c", []string{"distri", "build", "-pkg=make"}},
	// TODO(later):	{"bootstrap", []string{"distri", "build", "-bootstrap"}},
	{"batch", []string{"distri", "batch", "-dry_run"}},
	{"mirror-pkg", []string{"sh", "-c", "cd build/distri/pkg && distri mirror"}},
	{"mirror-debug", []string{"sh", "-c", "cd build/distri/debug && distri mirror"}},
	{"mirror-src", []string{"sh", "-c", "cd build/distri/src && distri mirror"}},
	{"cp-destdir", []string{"sh", "-c", "cp --link -r -f -a build/distri/* $DESTDIR/"}},
	{"image", []string{"sh", "-c", "mkdir -p $DESTDIR/img && make image DISKIMG=$DESTDIR/img/distri-disk.img"}},
	{"image-serial", []string{"sh", "-c", "mkdir -p $DESTDIR/img && make image serial=1 DISKIMG=$DESTDIR/img/distri-qemu-serial.img"}},
	{"image-gce", []string{"sh", "-c", "mkdir -p $DESTDIR/img && make gceimage GCEDISKIMG=$DESTDIR/img/distri-gce.tar.gz"}},
	// TODO(later): hook this up with credentials to push to the docker hub
	{"docker", []string{"sh", "-c", "make dockertar | tar tf -"}},
	{"docs", []string{"sh", "-c", "make docs DOCSDIR=$DESTDIR/docs"}},
}

func (b *buildctx) run(ctx context.Context) error {
	// store stamps for each step of a build
	for _, step := range steps {
		stampFile := filepath.Join(b.Workdir, "stamp."+step.stamp)
		if b.Rebuild == b.Commit {
			// keep going
		} else {
			log.Printf("checking stampFile=%q", stampFile)
			if _, err := os.Stat(stampFile); err == nil {
				fmt.Printf("[%s] already built, skipping\n", step.stamp)
				continue // already built
			}
		}
		fmt.Printf("[%s] %v\n", step.stamp, strings.Join(step.argv, " "))
		if b.DryRun {
			continue
		}
		build := exec.CommandContext(ctx, step.argv[0], step.argv[1:]...)
		build.Dir = filepath.Join(b.Workdir, "distri")
		build.Stdout = os.Stdout
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			return xerrors.Errorf("%v: %w", build.Args, err)
		}
		if err := ioutil.WriteFile(stampFile, nil, 0644); err != nil {
			return err
		}
	}
	return nil
}

func runJob(ctx context.Context, job string) error {
	var b buildctx
	c, err := ioutil.ReadFile(job)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(c, &b); err != nil {
		return xerrors.Errorf("unmarshaling %q: %v", string(c), err)
	}
	return b.run(ctx)
}

type autobuilder struct {
	repo    string
	branch  string
	srvDir  string
	dryRun  bool
	rebuild string

	status struct {
		sync.Mutex
		commits     []*github.RepositoryCommit
		lastUpdated time.Time
	}

	runMu sync.Mutex
}

func stamp(dir, stampName string) bool {
	_, err := os.Stat(filepath.Join(dir, "stamp."+stampName))
	return err == nil
}

type logWriter struct{ underlying *log.Logger }

func (lw logWriter) Write(p []byte) (n int, err error) {
	lw.underlying.Output(4, string(p))
	return len(p), nil
}

func (a *autobuilder) runCommit(ctx context.Context, commit string) error {
	log := log.New(&logWriter{
		log.New(log.Writer(), "", log.LstdFlags|log.Lshortfile),
	}, fmt.Sprintf("[commit %s] ", commit), 0)
	if a.rebuild == commit {
		// keep going
	} else {
		if _, err := os.Stat(filepath.Join(a.srvDir, "distri", commit)); err == nil {
			log.Printf("already built, skipping")
			return nil // already built
		}
	}

	log.Printf("building")

	workdir := filepath.Join(a.srvDir, "work", commit)
	if err := os.MkdirAll(workdir, 0755); err != nil {
		return err
	}

	if a.rebuild == commit || !stamp(workdir, "clone") {
		log.Printf("clone")
		distri := filepath.Join(workdir, "distri")
		if err := os.RemoveAll(distri); err != nil {
			return err
		}

		// TODO(later): use a cache
		clone := exec.Command("sh", "-c", fmt.Sprintf("git clone --depth=%d %s && cd distri && git reset --hard %s", 10+10 /* TODO: pending */, a.repo, commit))
		clone.Dir = workdir
		clone.Stdout = os.Stdout
		clone.Stderr = os.Stderr
		if err := clone.Run(); err != nil {
			return xerrors.Errorf("%v: %w", clone.Args, err)
		}

		if err := ioutil.WriteFile(filepath.Join(workdir, "stamp.clone"), nil, 0644); err != nil {
			return err
		}
	} else {
		log.Printf("already cloned")
	}

	if !a.dryRun /* TODO(later): #no-cache build tag not set */ {
		target, err := os.Readlink(filepath.Join(a.srvDir, "distri", a.branch))
		if err != nil {
			return err
		}
		// TODO: link upstream tarballs, too!
		// need to actually go through all pkgs/, parse, inspect sources to get the basenames

		// TODO(later): maybe implement this in Go to avoid process overhead
		dest := filepath.Join(workdir, "distri", "build", "distri")
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		for _, subdir := range []string{"pkg", "debug", "src"} {
			src := filepath.Join(a.srvDir, "distri", target, subdir)
			if _, err := os.Stat(src); err != nil && os.IsNotExist(err) {
				log.Printf("not copying subdir %s, does not exist in src %s", subdir, src)
				continue // skip
			}

			if _, err := os.Stat(filepath.Join(dest, subdir)); err == nil {
				log.Printf("not copying subdir %s, already exists in dest %s", subdir, dest)
				continue // skip, already exists
			}

			cp := exec.CommandContext(ctx, "cp",
				"--link",
				"-r",
				"-a",
				src,
				dest)
			cp.Stdout = os.Stdout
			cp.Stderr = os.Stderr
			log.Println(strings.Join(cp.Args, " "))
			if err := cp.Run(); err != nil {
				return xerrors.Errorf("%v: %w", cp.Args, err)
			}
		}
	}

	b := &buildctx{
		Commit:  commit,
		Workdir: workdir,
		DryRun:  a.dryRun,
		Rebuild: a.rebuild,
	}
	serialized, err := b.serialize()
	if err != nil {
		return err
	}
	defer os.Remove(serialized)

	if err := os.MkdirAll(filepath.Join(workdir, "dest"), 0755); err != nil {
		return err
	}

	install := exec.CommandContext(ctx, "make", "install")
	install.Dir = filepath.Join(workdir, "distri")
	install.Env = []string{
		"HOME=" + os.Getenv("HOME"), // TODO(later): make hermetic
		"GOPATH=" + filepath.Join(workdir, "go"),
		"GOPROXY=https://proxy.golang.org",
		"GOFLAGS=-modcacherw",
		"PATH=" + filepath.Join(workdir, "go", "bin") + ":" + os.Getenv("PATH"),
	}
	install.Stdout = os.Stdout
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		return xerrors.Errorf("%v: %w", install.Args, err)
	}

	cmd := exec.CommandContext(ctx, os.Args[0], "-job="+serialized)
	cmd.Dir = filepath.Join(workdir, "distri")
	// TODO(later): clean the environment
	cmd.Env = append(os.Environ(),
		"DISTRIROOT="+cmd.Dir,
		"DESTDIR="+filepath.Join(workdir, "dest"),
		"PATH="+workdir+"/go/bin:"+os.Getenv("PATH"))
	logDir := filepath.Join(a.srvDir, "buildlogs", commit, fmt.Sprintf("%d", time.Now().Unix()))
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}
	// TODO(later): add a combined log (stdout+stderr)
	stdout, err := os.Create(filepath.Join(logDir, "stdout.log"))
	if err != nil {
		return err
	}
	defer stdout.Close()
	cmd.Stdout = io.MultiWriter(os.Stdout, stdout)
	errlog := filepath.Join(logDir, "stderr.log")
	stderr, err := os.Create(errlog)
	if err != nil {
		return err
	}
	defer stderr.Close()
	cmd.Stderr = io.MultiWriter(os.Stderr, stderr)
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v (log %s): %w", cmd.Args, errlog, err)
	}

	oldpath := filepath.Join(workdir, "dest")
	newpath := filepath.Join(a.srvDir, "distri", commit)
	if err := os.Rename(oldpath, newpath); err != nil {
		if os.IsExist(err) {
			if err := os.RemoveAll(newpath); err != nil {
				log.Println(err)
				return err
			}
			if err := os.Rename(oldpath, newpath); err != nil {
				return err
			}
			err = nil
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *autobuilder) run(ctx context.Context) error {
	log.Printf("waiting for lock")
	a.runMu.Lock()
	defer a.runMu.Unlock()

	log.Printf("lock acquired, querying git commits")

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: *accessToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	owner, repo := func() (string, string) {
		parts := strings.Split(strings.TrimPrefix(a.repo, "https://github.com/"), "/")
		return parts[0], parts[1]
	}()
	repoCommits, _, err := client.Repositories.ListCommits(ctx, owner, repo, &github.CommitsListOptions{
		ListOptions: github.ListOptions{
			PerPage: 10, // TODO(later): flag
		},
	})
	if err != nil {
		return err
	}

	a.status.Lock()
	a.status.commits = repoCommits
	a.status.lastUpdated = time.Now()
	a.status.Unlock()

	// commits[0] is the most recent commit, i.e. we process commits
	// last-in-first-out (LIFO). This is what we want: when multiple commits are
	// pushed, the most recent one should be made available first, but the older
	// ones will still be built (so that bisection remains precise).
	for idx, c := range repoCommits {
		commit := c.GetSHA()
		if err := a.runCommit(ctx, commit); err != nil {
			log.Printf("runCommit(%s): %v", commit, err)
			break // TODO: continue: change after debugging
		} else {
			log.Printf("commit %s built", commit)
		}
		if idx == 0 && !a.dryRun {
			if err := renameio.Symlink(commit, filepath.Join(a.srvDir, "distri", a.branch)); err != nil {
				log.Printf("updating symlink: %v", err)
			}
		}
		break // TODO: remove after debugging
	}

	// TODO(later): implement garbage collection: limit disk quota, delete older
	// builds to stay within quota. first -n=10 builds are always retained.

	// TODO: `distri pack` speichert die korrekte URL an den relevanten stellen

	// FYI: manuell einschreiten:
	// - systemctl stop distri-autobuilder
	// - mirror in nötige form bringen
	// - systemctl restart distri-autobuilder

	return nil
}

var statusTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"firstLine": func(message string) string {
		if idx := strings.IndexByte(message, '\n'); idx > -1 {
			return message[:idx]
		}
		return message
	},
	"formatTimestamp": func(t time.Time) string {
		return t.Format(time.RFC3339)
	},
	"formatBytes": func(b uint64) string {
		switch {
		case b > 1024*1024*1024:
			return fmt.Sprintf("%.2f GiB", float64(b)/1024/1024/1024)

		case b > 1024*1024:
			return fmt.Sprintf("%.2f MiB", float64(b)/1024/1024)

		case b > 1024:
			return fmt.Sprintf("%.2f KiB", float64(b)/1024)

		default:
			return fmt.Sprintf("%d bytes", b)
		}
	},
}).Parse(`<!DOCTYPE html>
<head>
<meta charset="utf-8">
<title>distri autobuilder status</title>
<style type="text/css">
td {
  padding: 0.5em;
}
td.action {
  text-align: center;
}
</style>
</head>
<body>
<h1>last commits</h1>
<table width="100%" cellpadding=0 cellspacing=0>
{{ range $idx, $statusCommit := .Commits }}
{{ $commit := $statusCommit.RepoCommit }}
<tr>
<td>
<a href="{{ $commit.HTMLURL }}">{{ firstLine $commit.Commit.Message }}</a><br>
{{ $commit.Commit.Author.Name }} committed {{ $commit.Commit.Author.Date }}
</td>
<td class="action">
<a href="{{ $.Repo }}/tree/{{ $commit.SHA }}">
browse<br>
source
</a>
</td>
<td class="action">
{{ if ne $statusCommit.LatestBuildLog "" }}
<a href="{{ $statusCommit.LatestBuildLog }}">build log</a><br>
{{ else }}
(no build yet)<br>
{{ end }}
(<a href="/logs/{{ $commit.SHA }}">older logs</a>)
</td>
<td class="action">
<a href="/distri/{{ $commit.SHA }}">output</a>
<!-- label with branch chips -->
</td>
</tr>
{{ end }}
</table>
<h1>system status</h1>
<p>
tracking repository <code>{{ .Repo }}</code>, branch <code>{{ .Branch }}</code><br>
commits last updated {{ formatTimestamp .CommitsLastUpdated }}<br>
free disk space {{ formatBytes .DiskSpace }}<br>
</p>
</body>
</html>`))

func (a *autobuilder) serveStatusPage(w http.ResponseWriter, r *http.Request) {
	if err := func() error {
		a.status.Lock()
		defer a.status.Unlock()

		type statusCommit struct {
			LatestBuildLog string
			RepoCommit     *github.RepositoryCommit
		}
		statusCommits := make([]statusCommit, len(a.status.commits))
		for idx, commit := range a.status.commits {
			var latestBuildLog string
			logDir := filepath.Join(a.srvDir, "buildlogs", commit.GetSHA())
			if fis, err := ioutil.ReadDir(logDir); err == nil && len(fis) > 0 {
				latestBuildLog = path.Join("/logs", commit.GetSHA(), fis[len(fis)-1].Name())
			}
			statusCommits[idx] = statusCommit{
				LatestBuildLog: latestBuildLog,
				RepoCommit:     commit,
			}
		}

		var fs unix.Statfs_t
		if err := unix.Statfs(a.srvDir, &fs); err != nil {
			log.Println(err)
		}

		var buf bytes.Buffer
		if err := statusTmpl.Execute(&buf, struct {
			Commits            []statusCommit
			CommitsLastUpdated time.Time
			Repo               string
			Branch             string
			DiskSpace          uint64
		}{
			Commits:            statusCommits,
			CommitsLastUpdated: a.status.lastUpdated,
			Repo:               a.repo,
			Branch:             a.branch,
			DiskSpace:          fs.Bavail * uint64(fs.Bsize),
		}); err != nil {
			return err
		}
		io.Copy(w, &buf)
		return nil
	}(); err != nil {
		log.Printf("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func main() {
	var (
		repo     = flag.String("repo", "https://github.com/distr1/distri", "distri git repository to build")
		branch   = flag.String("branch", "master", "which branch of -repo to build")
		srvDir   = flag.String("srv_dir", "/srv/repo.distr1.org", "TODO")
		dryRun   = flag.Bool("dry_run", false, "print build commands instead of running them")
		once     = flag.Bool("once", false, "do one iteration instead of polling/listening for webhooks")
		job      = flag.String("job", "", "TODO")
		interval = flag.Duration("interval", 15*time.Minute, "how frequently to check for new tags (independent of any webhook notifications)")
		rebuild  = flag.String("rebuild", "", "If non-empty, a commit id to rebuild, i.e. ignore stamp files for")
	)
	flag.Parse()
	ctx, canc := distri.InterruptibleContext()
	defer canc()

	if *job != "" {
		if err := runJob(ctx, *job); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}
	a := &autobuilder{
		repo:    *repo,
		branch:  *branch,
		srvDir:  *srvDir,
		dryRun:  *dryRun,
		rebuild: *rebuild,
	}
	http.Handle("/logs/",
		http.StripPrefix("/logs/",
			http.FileServer(http.Dir(filepath.Join(*srvDir, "buildlogs")))))
	http.Handle("/distri/",
		http.StripPrefix("/distri/",
			http.FileServer(http.Dir(filepath.Join(*srvDir, "distri")))))
	http.HandleFunc("/status", a.serveStatusPage)
	go http.ListenAndServe(":3718", nil)
	if *once {
		if err := a.run(ctx); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	for {
		if err := a.run(ctx); err != nil {
			log.Fatalf("%+v", err)
		}
		select {
		// TODO: webhook support for triggering a run
		case <-hup:
		case <-time.After(*interval):
		}
	}
}
