package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/distr1/distri"
	"github.com/distr1/distri/pb"

	// PostgreSQL driver for database/sql:
	_ "github.com/lib/pq"
)

func errHandlerFunc(h func(w http.ResponseWriter, r *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			log.Printf("HTTP serving error: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

func logic(listen, assetsDir string) error {
	db, err := sql.Open("postgres", "dbname=distri sslmode=disable")
	if err != nil {
		return err
	}
	// TODO: refresh this data periodically
	getVersions, err := db.Prepare(`SELECT package, upstream_version, last_reachable, unreachable FROM upstream_status`)
	if err != nil {
		return err
	}
	rows, err := getVersions.Query()
	if err != nil {
		return err
	}
	type upstreamStatus struct {
		SourcePackage   string
		UpstreamVersion string
		LastReachable   time.Time
		Unreachable     bool
	}
	upstream := make(map[string]upstreamStatus)
	for rows.Next() {
		var s upstreamStatus
		if err := rows.Scan(&s.SourcePackage, &s.UpstreamVersion, &s.LastReachable, &s.Unreachable); err != nil {
			return err
		}
		upstream[s.SourcePackage] = s
	}
	if err := rows.Err(); err != nil {
		return err
	}

	whitelisted := map[string]bool{
		"repo.distr1.org":       true,
		"midna.zekjur.net:7080": true,
	}
	mc := &metadataCache{
		cached:  make(map[string]*cachedMetadata),
		updates: make(map[string]bool),
	}
	go func() {
		ctx, canc := context.WithTimeout(context.Background(), 5*time.Second)
		defer canc()
		log.Printf("pre-warming cache")
		const defaultURL = "https://repo.distr1.org/distri/jackherer/pkg/meta.binaryproto"
		if err := mc.update(ctx, defaultURL, ""); err != nil {
			log.Printf("pre-warming %v failed: %v", defaultURL, err)
		}
	}()

	mux := http.NewServeMux()
	for _, fn := range []string{
		"css/",
		"js/",
	} {
		if strings.HasSuffix(fn, "/") {
			mux.Handle("/"+fn, http.StripPrefix("/"+fn, http.FileServer(http.Dir(filepath.Join(assetsDir, fn)))))
		} else {
			mux.Handle("/"+fn, http.FileServer(http.Dir(assetsDir)))
		}
	}

	mux.Handle("/", errHandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		path := r.URL.Path
		// TODO(later): default to https:// and fall back to http:/ with a warning
		scheme := "http:/"
		if path == "/" || path == "" {
			// TODO: update whenever there is a new release. flag?
			scheme, path = "https:/", "/repo.distr1.org/distri/jackherer/"
		}
		repoURL, err := url.Parse(scheme + path)
		if err != nil {
			return err
		}
		if !whitelisted[repoURL.Host] {
			http.Error(w, "forbidden: "+repoURL.Host, http.StatusForbidden)
			return nil
		}
		u := repoURL.String() + "pkg/meta.binaryproto"
		meta, err := mc.Get(r.Context(), u)
		if err != nil {
			return err
		}

		// TODO: plumb SourcePackage into distri mirror for gcc-libs split package

		indexData := struct {
			Repo             string
			Packages         []*pb.MirrorMeta_Package
			UpstreamStatus   map[string]upstreamStatus
			NewUpstreamCount int
			UpToDateCount    int
		}{
			Repo:           repoURL.String(),
			Packages:       meta.Package,
			UpstreamStatus: upstream,
		}

		for _, pkg := range meta.Package {
			pv := distri.ParseVersion(pkg.GetName())
			status := upstream[pv.Pkg]
			if status.SourcePackage != "" && !status.Unreachable && status.UpstreamVersion != pv.Upstream {
				indexData.NewUpstreamCount++
			} else if status.SourcePackage != "" && !status.Unreachable && status.UpstreamVersion == pv.Upstream {
				indexData.UpToDateCount++
			}
		}

		var buf bytes.Buffer
		if err := indexTmpl.Execute(&buf, indexData); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		// Cache for one hour:
		utc := time.Now().UTC()
		cacheSince := utc.Format(http.TimeFormat)
		cacheUntil := utc.Add(1 * time.Hour).Format(http.TimeFormat)
		w.Header().Set("Cache-Control", "max-age=3600, public")
		w.Header().Set("Last-Modified", cacheSince)
		w.Header().Set("Expires", cacheUntil)
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		return nil
	}))
	return http.ListenAndServe(listen, mux)
}

func main() {
	var (
		listen    = flag.String("listen", "localhost:8047", "[host]:port to listen on")
		assetsDir = flag.String("assets", "assets", "directory in which to find assets")
	)
	flag.Parse()
	if err := logic(*listen, *assetsDir); err != nil {
		log.Fatal(err)
	}
}
