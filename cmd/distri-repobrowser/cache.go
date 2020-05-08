package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
)

type cachedMetadata struct {
	fetched time.Time
	meta    *pb.MirrorMeta
	etag    string
}

// metadataCache is a latency and availability cache because the
// meta.binaryproto files rarely change but are needed frequently.
type metadataCache struct {
	mu     sync.RWMutex
	cached map[string]*cachedMetadata

	updateMu sync.Mutex
	updates  map[string]bool
}

func (mc *metadataCache) update(ctx context.Context, u, etag string) error {
	log.Printf("fetching %v", u)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Add("User-Agent", "https://distr1.org/ repobrowser")
	if etag != "" {
		req.Header.Add("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		mc.mu.Lock()
		defer mc.mu.Unlock()
		cached, ok := mc.cached[u]
		if !ok {
			return fmt.Errorf("%s: got %v for uncached content?!", u, resp.Status)
		}
		cached.fetched = time.Now()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: unexpected HTTP status: got %v, want OK", u, resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var meta pb.MirrorMeta
	if err := proto.Unmarshal(b, &meta); err != nil {
		return err
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.cached[u] = &cachedMetadata{
		fetched: time.Now(),
		meta:    &meta,
		etag:    resp.Header.Get("ETag"),
	}
	return nil
}

func (mc *metadataCache) startUpdate(u, etag string) {
	mc.updateMu.Lock()
	_, ok := mc.updates[u]
	if !ok {
		mc.updates[u] = true // grab update lock
	}
	mc.updateMu.Unlock()
	if ok {
		// another goroutine is already doing the update
		return
	}

	if err := mc.update(context.Background(), u, etag); err != nil {
		log.Printf("cache update of %v failed: %v", u, err)
	}

	// release update lock:
	mc.updateMu.Lock()
	defer mc.updateMu.Unlock()
	delete(mc.updates, u)
}

func (mc *metadataCache) Get(ctx context.Context, u string) (*pb.MirrorMeta, error) {
	const expiration = 5 * time.Minute

	mc.mu.RLock()
	cached, ok := mc.cached[u]
	mc.mu.RUnlock()
	if ok {
		if time.Since(cached.fetched) < expiration {
			return cached.meta, nil
		}
		// start a (non-blocking) update in the background:
		mc.startUpdate(u, cached.etag)
		// answer immediately with the old data:
		return cached.meta, nil
	}
	if err := mc.update(ctx, u, ""); err != nil {
		return nil, err
	}
	mc.mu.RLock()
	cached, ok = mc.cached[u]
	mc.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("BUG: key not found in mc.cached after successful update")
	}
	return cached.meta, nil
}
