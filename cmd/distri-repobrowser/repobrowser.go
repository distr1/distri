package main

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"

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

func logic(listen string) error {
	db, err := sql.Open("postgres", "dbname=distri sslmode=disable")
	if err != nil {
		return err
	}
	// TODO: refresh this data periodically
	getVersions, err := db.Prepare(`SELECT package, upstream_version, last_reachable FROM upstream_status`)
	if err != nil {
		return err
	}
	rows, err := getVersions.Query()
	if err != nil {
		return err
	}
	type upstreamStatus struct {
		Package         string
		UpstreamVersion string
		LastReachable   time.Time
	}
	upstream := make(map[string]upstreamStatus)
	for rows.Next() {
		var s upstreamStatus
		if err := rows.Scan(&s.Package, &s.UpstreamVersion, &s.LastReachable); err != nil {
			return err
		}
		upstream[s.Package] = s
	}
	if err := rows.Err(); err != nil {
		return err
	}

	whitelisted := map[string]bool{
		"repo.distr1.org": true,
	}
	mux := http.NewServeMux()
	mux.Handle("/", errHandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		path := r.URL.Path
		if path == "/" || path == "" {
			// TODO: update whenever there is a new release. flag?
			path = "/repo.distr1.org/distri/jackherer/"
		}
		repoURL, err := url.Parse("https:/" + path)
		if err != nil {
			return err
		}
		if !whitelisted[repoURL.Host] {
			http.Error(w, "forbidden: "+repoURL.Host, http.StatusForbidden)
			return nil
		}
		// TODO: other sections, too
		var meta pb.MirrorMeta
		// TODO: use context for cancelation/timeout
		resp, err := http.Get(repoURL.String() + "pkg/meta.binaryproto")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected HTTP status: got %v, want OK", resp.Status)
		}
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if err := proto.Unmarshal(b, &meta); err != nil {
			return err
		}

		// TODO: cache fetched meta
		// TODO: render package list

		indexData := struct {
			Repo           string
			Packages       []*pb.MirrorMeta_Package
			UpstreamStatus map[string]upstreamStatus
		}{
			Repo:           repoURL.String(),
			Packages:       meta.Package,
			UpstreamStatus: upstream,
		}

		var buf bytes.Buffer
		if err := indexTmpl.Execute(&buf, indexData); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		return nil
	}))
	return http.ListenAndServe(listen, mux)
}

func main() {
	var (
		listen = flag.String("listen", "localhost:8047", "[host]:port to listen on")
	)
	flag.Parse()
	if err := logic(*listen); err != nil {
		log.Fatal(err)
	}
}
