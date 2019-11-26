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
	rebuild = flag.String("rebuild",
		"",
		"If non-empty, a commit id to rebuild, i.e. ignore stamp files for")
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
	{"debug", []string{"sh", "-c", "cd pkgs/i3status && distri"}},
	// TODO(later):	{"bootstrap", []string{"distri", "build", "-bootstrap"}},
	{"batch", []string{"distri", "batch", "-dry_run"}}, // TODO: enable actual build
	{"image", []string{"sh", "-c", "mkdir -p $DESTDIR/img && make image DISKIMG=$DESTDIR/img/distri-disk.img"}},
	{"image-serial", []string{"sh", "-c", "mkdir -p $DESTDIR/img && make image serial=1 DISKIMG=$DESTDIR/img/distri-qemu-serial.img"}},
	{"image-gce", []string{"sh", "-c", "mkdir -p $DESTDIR/img && make gceimage GCEDISKIMG=$DESTDIR/img/distri-gce.tar.gz"}},
	// TODO(later): hook this up with credentials to push to the docker hub
	{"docker", []string{"sh", "-c", "make dockertar | tar tf -"}},
	{"docs", []string{"sh", "-c", "make docs DOCSDIR=$DESTDIR/docs"}},

	{"cp-destdir", []string{"sh", "-c", "cp --link -r -f -a build/distri/* $DESTDIR/"}},
}

func (b *buildctx) run() error {
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
		build := exec.Command(step.argv[0], step.argv[1:]...)
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

func runJob(job string) error {
	var b buildctx
	c, err := ioutil.ReadFile(job)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(c, &b); err != nil {
		return xerrors.Errorf("unmarshaling %q: %v", string(c), err)
	}
	return b.run()
}

type autobuilder struct {
	repo   string
	branch string
	srvDir string
	dryRun bool

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

func (a *autobuilder) runCommit(commit string) error {
	if *rebuild == commit {
		// keep going
	} else {
		if _, err := os.Stat(filepath.Join(a.srvDir, "distri", commit)); err == nil {
			log.Printf("[%s] already built, skipping", commit)
			return nil // already built
		}
	}

	log.Printf("[%s] building", commit)

	workdir := filepath.Join(a.srvDir, "work", commit)
	if err := os.MkdirAll(workdir, 0755); err != nil {
		return err
	}

	if *rebuild == commit || !stamp(workdir, "clone") {
		distri := filepath.Join(workdir, "distri")
		if err := os.RemoveAll(distri); err != nil {
			return err
		}

		// TODO: clone from *repo
		// TODO(later): use a cache
		clone := exec.Command("sh", "-c", fmt.Sprintf("git clone --depth=%d file:///home/michael/distri && cd distri && git reset --hard %s", 10+10 /* TODO: pending */, commit))
		clone.Dir = workdir
		clone.Stdout = os.Stdout
		clone.Stderr = os.Stderr
		if err := clone.Run(); err != nil {
			return xerrors.Errorf("%v: %w", clone.Args, err)
		}

		for _, subdir := range []string{"pkg", "debug"} {
			if err := os.MkdirAll(filepath.Join(workdir, "distri", "build", "distri", subdir), 0755); err != nil {
				return err
			}
		}

		if err := ioutil.WriteFile(filepath.Join(workdir, "stamp.clone"), nil, 0644); err != nil {
			return err
		}
	}

	if true /* TODO(later): #no-cache build tag not set */ {
		target, err := os.Readlink(filepath.Join(a.srvDir, "distri", a.branch))
		if err != nil {
			return err
		}
		// TODO: link upstream tarballs, too!
		// need to actually go through all pkgs/, parse, inspect sources to get the basenames

		// TODO(later): maybe implement this in Go to avoid process overhead
		for _, subdir := range []string{"pkg", "debug"} {
			cp := exec.Command("cp",
				"--link",
				"-r",
				"-a",
				filepath.Join(a.srvDir, "distri", target, subdir),
				filepath.Join(workdir, "distri", "build", "distri"))
			cp.Stdout = os.Stdout
			cp.Stderr = os.Stderr
			if err := cp.Run(); err != nil {
				return xerrors.Errorf("%v: %w", cp.Args, err)
			}
		}
	}

	b := &buildctx{
		Commit:  commit,
		Workdir: workdir,
		DryRun:  a.dryRun,
		Rebuild: *rebuild,
	}
	serialized, err := b.serialize()
	if err != nil {
		return err
	}
	defer os.Remove(serialized)

	if err := os.MkdirAll(filepath.Join(workdir, "dest"), 0755); err != nil {
		return err
	}

	// TODO(later): call “make install” instead
	gotool := exec.Command("go", "install", "github.com/distr1/distri/cmd/distri")
	gotool.Dir = filepath.Join(workdir, "distri")
	gotool.Env = []string{
		"HOME=" + os.Getenv("HOME"), // TODO(later): make hermetic
		"GOPATH=" + filepath.Join(workdir, "go"),
		"GOPROXY=https://proxy.golang.org",
		"PATH=" + os.Getenv("PATH"),
	}
	gotool.Stdout = os.Stdout
	gotool.Stderr = os.Stderr
	if err := gotool.Run(); err != nil {
		return xerrors.Errorf("%v: %w", gotool.Args, err)
	}
	sudo := exec.Command("sudo", "setcap", "CAP_SYS_ADMIN,CAP_DAC_OVERRIDE=ep CAP_SETFCAP=eip", filepath.Join(workdir, "go", "bin", "distri"))
	sudo.Stdout = os.Stdout
	sudo.Stderr = os.Stderr
	if err := sudo.Run(); err != nil {
		return xerrors.Errorf("%v: %w", sudo.Args, err)
	}

	cmd := exec.Command(os.Args[0], "-job="+serialized)
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
	cmd.Stdout = stdout
	errlog := filepath.Join(logDir, "stderr.log")
	stderr, err := os.Create(errlog)
	if err != nil {
		return err
	}
	defer stderr.Close()
	cmd.Stderr = stderr
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

func (a *autobuilder) run() error {
	log.Printf("waiting for lock")
	a.runMu.Lock()
	defer a.runMu.Unlock()

	log.Printf("lock acquired, querying git commits")

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: *accessToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	repoCommits, _, err := client.Repositories.ListCommits(ctx, "stapelberg", "distri", &github.CommitsListOptions{
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
		if err := a.runCommit(commit); err != nil {
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
		repo     = flag.String("repo", "https://github.com/stapelberg/distri", "distri git repository to build")
		branch   = flag.String("branch", "master", "which branch of -repo to build")
		srvDir   = flag.String("srv_dir", "/srv/repo.distr1.org", "TODO")
		dryRun   = flag.Bool("dry_run", false, "print build commands instead of running them")
		job      = flag.String("job", "", "TODO")
		interval = flag.Duration("interval", 15*time.Minute, "how frequently to check for new tags (independent of any webhook notifications)")
	)
	flag.Parse()
	if *job != "" {
		if err := runJob(*job); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}
	a := &autobuilder{
		repo:   *repo,
		branch: *branch,
		srvDir: *srvDir,
		dryRun: *dryRun,
	}
	http.Handle("/logs/",
		http.StripPrefix("/logs/",
			http.FileServer(http.Dir(filepath.Join(*srvDir, "buildlogs")))))
	http.Handle("/distri/",
		http.StripPrefix("/distri/",
			http.FileServer(http.Dir(filepath.Join(*srvDir, "distri")))))
	http.HandleFunc("/status", a.serveStatusPage)
	go http.ListenAndServe(":3718", nil)
	if false /* once */ {
		if err := a.run(); err != nil {
			log.Fatalf("%+v", err)
		}
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	for {
		if err := a.run(); err != nil {
			log.Fatalf("%+v", err)
		}
		select {
		// TODO: webhook support for triggering a run
		case <-hup:
		case <-time.After(*interval):
		}
	}
}
