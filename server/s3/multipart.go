// Credits: https://pkg.go.dev/github.com/rclone/rclone@v1.65.2/cmd/serve/s3
// Package s3 implements a fake s3 server for openlist
package s3

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/gofakes3"
	log "github.com/sirupsen/logrus"
)

// Compile-time assertions that s3Backend implements both the base Backend and
// the optional MultipartBackend interface from gofakes3.
var (
	_ gofakes3.Backend          = (*s3Backend)(nil)
	_ gofakes3.MultipartBackend = (*s3Backend)(nil)
)

// multipartPart records a single uploaded part on disk.
type multipartPart struct {
	path    string
	size    int64
	md5hex  string // unquoted lowercase hex
	updated time.Time
}

// multipartState tracks one in-progress multipart upload.
//
// Concurrency: gofakes3 does not serialize operations for the same uploadID,
// so concurrent UploadPart/Complete/Abort calls may overlap. The parts map is
// protected by mu. Each part is written to its own file inside dir, so
// concurrent UploadPart calls for different part numbers are safe without
// additional locking. lastActivity is updated under mu on create and on each
// part upload so the reaper can make a consistent expiry decision.
type multipartState struct {
	bucket       string
	object       string
	meta         map[string]string
	dir          string
	created      time.Time
	lastActivity time.Time // updated under mu on create and each part upload

	// Async completion bookkeeping (all guarded by mu):
	// completing is true from the moment CompleteMultipartUpload is accepted
	// until finalizeMultipart finishes. completedEtag caches the etag handed
	// back to clients so repeated Complete calls are deduplicated. abortPending
	// records an Abort that arrived while a finalization was in flight or
	// queued behind the finalize semaphore.
	completing    bool
	completedEtag string
	abortPending  bool

	mu    sync.Mutex
	parts map[int]*multipartPart
}

// CreateMultipartUpload begins a new multipart upload. Parts are streamed to
// local temp files so that large uploads do not need to be buffered in memory.
//
// It implements gofakes3.MultipartBackend.
func (b *s3Backend) CreateMultipartUpload(ctx context.Context, bucket, object string, meta map[string]string) (gofakes3.UploadID, error) {
	if _, err := getBucketByName(bucket); err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp(multipartTempDir(), "s3-multipart-*")
	if err != nil {
		return "", fmt.Errorf("create multipart upload dir: %w", err)
	}

	uploadID := gofakes3.UploadID(strings.ReplaceAll(uuid.NewString(), "-", ""))
	now := time.Now()
	state := &multipartState{
		bucket:       bucket,
		object:       object,
		meta:         meta,
		dir:          dir,
		created:      now,
		lastActivity: now,
		parts:        map[int]*multipartPart{},
	}

	b.uploads.Store(uploadID, state)
	log.Debugf("s3 multipart: created upload %s for %s/%s", uploadID, bucket, object)
	return uploadID, nil
}

// UploadPart writes a single part to disk and returns its (quoted) MD5 etag.
//
// It implements gofakes3.MultipartBackend. The body must contain exactly
// contentLength bytes; a short read is reported as ErrIncompleteBody so a
// truncated client request is never silently stored.
func (b *s3Backend) UploadPart(ctx context.Context, bucket, object string, uploadID gofakes3.UploadID, partNumber int, contentLength int64, body io.Reader) (string, error) {
	if partNumber <= 0 || partNumber > gofakes3.MaxUploadPartNumber {
		return "", gofakes3.ErrInvalidPart
	}

	state, ok := b.loadUpload(uploadID)
	if !ok {
		return "", gofakes3.ErrNoSuchUpload
	}

	partPath := filepath.Join(state.dir, fmt.Sprintf("part-%05d", partNumber))
	f, err := os.Create(partPath)
	if err != nil {
		return "", fmt.Errorf("create part file: %w", err)
	}
	// Remove a half-written file on any failure path.
	partFailed := true
	defer func() {
		if partFailed {
			_ = f.Close()
			_ = os.Remove(partPath)
		}
	}()

	hash := md5.New()
	// io.TeeReader feeds the hasher while the part is streamed to disk, so the
	// etag costs no extra pass over the data.
	n, err := utils.CopyWithBuffer(io.MultiWriter(f, hash), io.LimitReader(body, contentLength))
	if err != nil {
		return "", fmt.Errorf("write part %d: %w", partNumber, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close part %d: %w", partNumber, err)
	}
	if n != contentLength {
		// The client under-delivered (truncated/aborted request). gofakes3 does
		// not validate this for streaming backends, so we must.
		return "", gofakes3.ErrIncompleteBody
	}

	md5hex := hex.EncodeToString(hash.Sum(nil))
	etag := fmt.Sprintf("%q", md5hex)

	now := time.Now()
	state.mu.Lock()
	if state.completing {
		// The part set is frozen once a Complete has been accepted: a late
		// UploadPart could truncate a part file the finalizer is reading.
		state.mu.Unlock()
		return "", gofakes3.ErrNoSuchUpload
	}
	if old := state.parts[partNumber]; old != nil && old.path != partPath {
		_ = os.Remove(old.path)
	}
	state.parts[partNumber] = &multipartPart{
		path:    partPath,
		size:    n,
		md5hex:  md5hex,
		updated: now,
	}
	state.lastActivity = now
	state.mu.Unlock()

	partFailed = false
	log.Debugf("s3 multipart: stored part %d for %s (%d bytes)", partNumber, uploadID, n)
	return etag, nil
}

// CompleteMultipartUpload validates the uploaded parts and accepts the
// completion, returning the final etag immediately. The actual assembly —
// concatenating the part files and streaming them into storage — happens in
// finalizeMultipart on a background goroutine.
//
// Why async: putStream for a multi-GB object can run for minutes inside the
// HTTP request. Reverse proxies in front of the gateway (notably Cloudflare
// Tunnel) cut origin responses after ~100s and return 524, which makes the
// client re-upload the entire object. Answering fast and finalizing detached
// avoids that; the trade-off is eventual consistency — the object may not be
// visible for the duration of the finalization.
//
// It implements gofakes3.MultipartBackend. Part ordering and etags are
// validated against the parts actually received. Repeated Complete calls
// while a finalization is pending return the cached etag; after a failed
// finalization the upload stays retryable (per the gofakes3 contract).
func (b *s3Backend) CompleteMultipartUpload(ctx context.Context, bucket, object string, uploadID gofakes3.UploadID, input *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
	state, ok := b.loadUpload(uploadID)
	if !ok {
		return "", "", gofakes3.ErrNoSuchUpload
	}

	if input == nil || len(input.Parts) == 0 {
		return "", "", gofakes3.ErrorMessagef(gofakes3.ErrMalformedXML, "complete multipart upload has no parts")
	}
	// S3 requires the parts in a CompleteMultipartUpload request to be listed
	// in ascending part-number order.
	for i := 1; i < len(input.Parts); i++ {
		if input.Parts[i].PartNumber <= input.Parts[i-1].PartNumber {
			return "", "", gofakes3.ErrInvalidPartOrder
		}
	}

	// Validate every requested part exists with a matching etag, and collect
	// them in the order requested by the client (which is sorted ascending).
	state.mu.Lock()
	if state.abortPending {
		// An abort landed after a failed finalization reset this upload
		// (see AbortMultipartUpload): nothing will finalize it, so clean it
		// up instead of leaving it stuck until the reaper.
		state.mu.Unlock()
		b.removeUpload(uploadID)
		return "", "", gofakes3.ErrNoSuchUpload
	}
	if state.completing {
		// A finalization is already running (or queued) for this upload:
		// deduplicate instead of streaming the parts twice.
		etag := state.completedEtag
		state.mu.Unlock()
		return "", etag, nil
	}
	ordered := make([]*multipartPart, 0, len(input.Parts))
	var concat []byte
	var total int64
	for _, p := range input.Parts {
		stored := state.parts[p.PartNumber]
		if stored == nil {
			state.mu.Unlock()
			return "", "", gofakes3.ErrorMessagef(gofakes3.ErrInvalidPart, "unexpected part number %d in complete request", p.PartNumber)
		}
		if strings.Trim(p.ETag, "\"") != stored.md5hex {
			state.mu.Unlock()
			return "", "", gofakes3.ErrorMessagef(gofakes3.ErrInvalidPart, "unexpected part etag for number %d in complete request", p.PartNumber)
		}
		ordered = append(ordered, stored)
		total += stored.size
		// S3 multipart etag = hex(md5(concat(part_md5_digests)))-N
		concat = append(concat, stored.md5Bytes()...)
	}
	// From here the upload is committed to finalization: parts are frozen
	// (UploadPart rejects completing uploads), the reaper skips it, and part
	// files stay on disk until finalizeMultipart succeeds. The etag is
	// published together with completing so a racing duplicate Complete can
	// never observe an empty cached etag.
	sum := md5.Sum(concat)
	etag := fmt.Sprintf("%q", fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:]), len(ordered)))
	state.completing = true
	state.completedEtag = etag
	state.lastActivity = time.Now()
	state.mu.Unlock()

	go b.finalizeMultipart(uploadID, state, ordered, total)

	log.Debugf("s3 multipart: accepted complete for %s -> %s/%s (%d bytes), finalizing in background", uploadID, bucket, object, total)
	return "", etag, nil
}

// finalizeSlots bounds how many multipart finalizations stream into storage at
// once. Each finalization re-reads every part file and can run for minutes on
// multi-GB objects, so they are capped to keep disk I/O and driver load sane.
var finalizeSlots = make(chan struct{}, 2)

// finalizeMultipart assembles the part files in ascending part-number order
// and streams the result into storage via the shared putStream path.
//
// It runs detached from the CompleteMultipartUpload request: that request's
// context is canceled as soon as the response is written (or the connection
// drops), so a background context is used deliberately — the transfer must
// survive the client/proxy hanging up.
func (b *s3Backend) finalizeMultipart(uploadID gofakes3.UploadID, state *multipartState, ordered []*multipartPart, total int64) {
	finalizeSlots <- struct{}{}
	defer func() { <-finalizeSlots }()

	if state.isAbortPending() {
		// Abort arrived while this finalization was queued behind the
		// semaphore: drop the upload without touching storage.
		b.removeUpload(uploadID)
		log.Infof("s3 multipart: dropped upload %s (%s/%s), aborted before finalization", uploadID, state.bucket, state.object)
		return
	}

	// The part set is frozen (completing), the reaper skips this upload and
	// an abort while completing only records abortPending — nothing can
	// delete the part files out from under us, so no lock is needed here.
	readers := make([]io.Reader, 0, len(ordered))
	closers := make([]io.Closer, 0, len(ordered))
	for _, part := range ordered {
		f, err := os.Open(part.path)
		if err != nil {
			for _, c := range closers {
				_ = c.Close()
			}
			b.finalizeFailed(uploadID, state, fmt.Errorf("open part %s: %w", part.path, err))
			return
		}
		readers = append(readers, f)
		closers = append(closers, f)
	}

	combined := utils.NewReadCloser(io.MultiReader(readers...), func() error {
		var firstErr error
		for _, c := range closers {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	})

	err := b.putStream(context.Background(), state.bucket, state.object, state.meta, combined, total)
	_ = combined.Close()

	if err != nil {
		b.finalizeFailed(uploadID, state, err)
		return
	}

	b.removeUpload(uploadID)
	log.Debugf("s3 multipart: completed upload %s -> %s/%s (%d bytes)", uploadID, state.bucket, state.object, total)
}

// finalizeFailed resets the completion state so the client may send another
// CompleteMultipartUpload to retry. Parts and their files stay in place; only
// a reaped upload would drop them. A pending abort wins over retryability and
// removes the upload outright.
func (b *s3Backend) finalizeFailed(uploadID gofakes3.UploadID, state *multipartState, err error) {
	state.mu.Lock()
	state.completing = false
	state.completedEtag = ""
	state.lastActivity = time.Now()
	aborted := state.abortPending
	state.mu.Unlock()

	if aborted {
		b.removeUpload(uploadID)
		log.Infof("s3 multipart: dropped upload %s (%s/%s) after failed finalization + abort", uploadID, state.bucket, state.object)
		return
	}
	log.Errorf("s3 multipart: finalize %s (%s/%s) failed, retryable via CompleteMultipartUpload: %v", uploadID, state.bucket, state.object, err)
}

func (s *multipartState) isAbortPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abortPending
}

// pendingHeadObject returns a synthetic object for a bucket/object pair whose
// multipart completion has been accepted and is still being finalized in the
// background (completing == true, completedEtag published). The data is not
// readable yet, but HEAD must report the committed size so uploading clients
// (rclone verifies size immediately after upload) do not misread the
// finalize window as a corrupted/missing transfer.
func (b *s3Backend) pendingHeadObject(bucket, object string) *gofakes3.Object {
	var state *multipartState
	b.uploads.Range(func(key, val any) bool {
		st := val.(*multipartState)
		st.mu.Lock()
		match := st.completing && st.completedEtag != "" && st.bucket == bucket && st.object == object
		st.mu.Unlock()
		if match {
			state = st
			return false
		}
		return true
	})
	if state == nil {
		return nil
	}

	state.mu.Lock()
	var size int64
	for _, p := range state.parts {
		size += p.size
	}
	meta := map[string]string{
		"Last-Modified": time.Now().UTC().Format(timeFormat),
	}
	if ct := state.meta["Content-Type"]; ct != "" {
		meta["Content-Type"] = ct
	}
	state.mu.Unlock()

	return &gofakes3.Object{
		Name:     object,
		Metadata: meta,
		Size:     size,
		Contents: noOpReadCloser{},
	}
}

// AbortMultipartUpload discards an in-progress upload and its parts.
//
// It implements gofakes3.MultipartBackend and is idempotent: aborting an
// unknown upload succeeds so retries do not fail. Aborting an upload whose
// finalization already started records the request; the finalizer then drops
// the upload (best-effort — a stream already in progress is allowed to run to
// completion or failure first).
func (b *s3Backend) AbortMultipartUpload(ctx context.Context, bucket, object string, uploadID gofakes3.UploadID) error {
	state, ok := b.loadUpload(uploadID)
	if !ok {
		return nil
	}

	state.mu.Lock()
	if state.completing {
		// Finalization in progress (or queued): record the abort and let the
		// finalizer drop the upload. Single critical section — reading
		// completing and setting abortPending in two sections would race a
		// finalizeFailed that resets completing in between, leaving an
		// abortPending flag nothing will ever act on.
		state.abortPending = true
		state.mu.Unlock()
		log.Infof("s3 multipart: abort requested for %s while finalizing, will drop after finalization", uploadID)
		return nil
	}
	state.mu.Unlock()

	b.removeUpload(uploadID)
	return nil
}

// multipartTempDir returns the configured temp dir, falling back to the
// system default when unset.
func multipartTempDir() string {
	if dir := conf.Conf.TempDir; dir != "" {
		return dir
	}
	return os.TempDir()
}

// loadUpload returns the tracked state for uploadID, if any.
func (b *s3Backend) loadUpload(uploadID gofakes3.UploadID) (*multipartState, bool) {
	val, ok := b.uploads.Load(uploadID)
	if !ok {
		return nil, false
	}
	return val.(*multipartState), true
}

// removeUpload deletes the upload's temp directory and drops its bookkeeping.
// Missing uploads are ignored to keep abort/complete idempotent.
func (b *s3Backend) removeUpload(uploadID gofakes3.UploadID) {
	val, ok := b.uploads.LoadAndDelete(uploadID)
	if !ok {
		return
	}
	state := val.(*multipartState)
	if state.dir != "" {
		if err := os.RemoveAll(state.dir); err != nil {
			log.Warnf("s3 multipart: failed to clean up %s: %v", state.dir, err)
		}
	}
}

// md5Bytes returns the raw 16-byte MD5 digest of the part.
func (p *multipartPart) md5Bytes() []byte {
	b, _ := hex.DecodeString(p.md5hex)
	return b
}

// Defaults for reaping abandoned multipart uploads. A client that never sends
// CompleteMultipartUpload or AbortMultipartUpload would otherwise leave part
// files on disk forever; the reaper drops uploads inactive for longer than the
// TTL.
const (
	defaultMultipartTTL = 24 * time.Hour
	multipartDirPrefix  = "s3-multipart-"
)

// multipartTTL returns the configured max idle time for an upload before the
// reaper reclaims it. It parses conf.Conf.S3.MultipartTTL as a Go duration
// (e.g. "24h", "30m"); an empty or invalid value falls back to the default.
func multipartTTL() time.Duration {
	if v := conf.Conf.S3.MultipartTTL; v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultMultipartTTL
}

// reapInterval derives the reaper tick interval from the TTL: a quarter of the
// TTL, clamped to [10s, 1h].
func reapInterval(ttl time.Duration) time.Duration {
	d := ttl / 4
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// startReaper removes leftover part directories from a previous process crash
// and then launches a background goroutine that periodically reclaims uploads
// inactive for longer than the TTL. The goroutine runs for the lifetime of the
// process; NewServer is called once at startup, so there is one reaper per
// backend instance.
func (b *s3Backend) startReaper() {
	ttl := multipartTTL()
	b.cleanupStaleDirs(time.Now(), ttl)
	interval := reapInterval(ttl)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for now := range ticker.C {
			b.reapExpired(now, multipartTTL())
		}
	}()
}

// reapExpired removes uploads whose lastActivity is older than ttl. It is safe
// to call concurrently with UploadPart/Complete/Abort: each candidate is
// re-checked under its own lock and removed atomically via removeUpload.
// Uploads with an accepted-but-unfinished completion are never reaped — their
// parts are being streamed by finalizeMultipart and must survive until it
// succeeds or fails.
func (b *s3Backend) reapExpired(now time.Time, ttl time.Duration) {
	b.uploads.Range(func(key, val any) bool {
		state := val.(*multipartState)
		state.mu.Lock()
		expired := now.Sub(state.lastActivity) > ttl && !state.completing
		state.mu.Unlock()
		if !expired {
			return true
		}
		b.removeUpload(key.(gofakes3.UploadID))
		log.Infof("s3 multipart: reaped abandoned upload %s (%s/%s)", key, state.bucket, state.object)
		return true
	})
}

// cleanupStaleDirs removes s3-multipart-* directories under TempDir that are
// older than ttl. This reclaims part files left behind by a previous process
// crash; dirs younger than ttl are left alone so a concurrently-starting
// sibling backend instance is never disturbed.
func (b *s3Backend) cleanupStaleDirs(now time.Time, ttl time.Duration) {
	tempDir := multipartTempDir()
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return
	}
	cutoff := now.Add(-ttl)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), multipartDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(tempDir, e.Name())); err != nil {
			log.Warnf("s3 multipart: failed to clean up stale dir %s: %v", e.Name(), err)
		} else {
			log.Infof("s3 multipart: removed stale multipart dir %s", e.Name())
		}
	}
}
