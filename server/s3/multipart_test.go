package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"

	"github.com/OpenListTeam/gofakes3"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func init() {
	dataDir, err := os.MkdirTemp("", "openlist-s3-mp-*")
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig(dataDir)
	if err := os.MkdirAll(conf.Conf.TempDir, 0o755); err != nil {
		panic("mkdir temp dir: " + err.Error())
	}
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database: " + err.Error())
	}
	db.Init(dB)
}

// s3ErrorCode extracts the gofakes3 ErrorCode from an error returned by the
// MultipartBackend methods.
func s3ErrorCode(err error) gofakes3.ErrorCode {
	if err == nil {
		return gofakes3.ErrNone
	}
	var s3err interface{ ErrorCode() gofakes3.ErrorCode }
	if errors.As(err, &s3err) {
		return s3err.ErrorCode()
	}
	return gofakes3.ErrNone
}

// setupMultipartBackend prepares a Local storage mounted at /mpbucket and an
// s3Backend with an "mp" bucket pointing at it. It returns the backend, the
// local root directory on disk, and the storage id.
func setupMultipartBackend(t *testing.T) (*s3Backend, string, uint) {
	t.Helper()
	ctx := context.Background()

	// Unique mount path and bucket per test: the in-memory sqlite is shared
	// across tests in this package, so a fixed mount path would clash.
	mount := "/" + sanitizeTestName(t.Name())
	bucket := "mp"

	localRoot, err := os.MkdirTemp("", "openlist-s3-local-*")
	if err != nil {
		t.Fatalf("mkdir local root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(localRoot) })
	addition, err := json.Marshal(struct {
		RootFolderPath string `json:"root_folder_path"`
		Thumbnail      bool   `json:"thumbnail"`
	}{
		RootFolderPath: localRoot,
		Thumbnail:      false,
	})
	if err != nil {
		t.Fatalf("marshal local storage addition: %v", err)
	}

	sid, err := op.CreateStorage(ctx, model.Storage{
		Driver:    "Local",
		MountPath: mount,
		Addition:  string(addition),
	})
	if err != nil {
		t.Fatalf("create local storage: %+v", err)
	}
	t.Cleanup(func() {
		// Re-enable first (deleting a disabled storage errors), then remove
		// the row so a second -count iteration can recreate the same mount.
		_ = op.EnableStorage(context.Background(), sid)
		_ = op.DeleteStorageById(context.Background(), sid)
	})

	if err := op.SaveSettingItem(&model.SettingItem{
		Key:   conf.S3Buckets,
		Value: `[{"name":"` + bucket + `","path":"` + mount + `"}]`,
	}); err != nil {
		t.Fatalf("save s3 buckets setting: %+v", err)
	}

	return newBackend().(*s3Backend), localRoot, sid
}

// waitFor polls cond every 20ms until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func sanitizeTestName(name string) string {
	r := strings.NewReplacer("/", "_", " ", "_")
	return r.Replace(name)
}

// mustCreateUpload starts a multipart upload and fails the test on error.
func mustCreateUpload(t *testing.T, b *s3Backend, bucket, object string, meta map[string]string) gofakes3.UploadID {
	t.Helper()
	id, err := b.CreateMultipartUpload(context.Background(), bucket, object, meta)
	if err != nil {
		t.Fatalf("CreateMultipartUpload %s/%s: %+v", bucket, object, err)
	}
	return id
}

// mustUploadPart uploads one part and fails the test on error, returning the etag.
func mustUploadPart(t *testing.T, b *s3Backend, bucket, object string, uploadID gofakes3.UploadID, n int, body string) string {
	t.Helper()
	etag, err := b.UploadPart(context.Background(), bucket, object, uploadID, n, int64(len(body)), strings.NewReader(body))
	if err != nil {
		t.Fatalf("UploadPart %d: %+v", n, err)
	}
	return etag
}

// waitUploadGone blocks until the upload is removed from backend bookkeeping.
func waitUploadGone(t *testing.T, b *s3Backend, uploadID gofakes3.UploadID) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		_, ok := b.uploads.Load(uploadID)
		return !ok
	})
}

func TestMultipartUploadEndToEnd(t *testing.T) {
	ctx := context.Background()
	b, localRoot, _ := setupMultipartBackend(t)

	meta := map[string]string{"Content-Type": "text/plain"}
	uploadID := mustCreateUpload(t, b, "mp", "dir/hello.txt", meta)
	if uploadID == "" {
		t.Fatal("empty upload id")
	}

	etag1 := mustUploadPart(t, b, "mp", "dir/hello.txt", uploadID, 1, "Hello, ")
	etag2 := mustUploadPart(t, b, "mp", "dir/hello.txt", uploadID, 2, "multipart ")
	etag3 := mustUploadPart(t, b, "mp", "dir/hello.txt", uploadID, 3, "world!")

	// Re-uploading the same part number overwrites it and returns a fresh etag.
	if e := mustUploadPart(t, b, "mp", "dir/hello.txt", uploadID, 2, "multipart "); e != etag2 {
		t.Fatalf("re-upload part 2 etag = %q, want %q", e, etag2)
	}

	// Short read (body smaller than declared Content-Length) must fail.
	_, shortErr := b.UploadPart(ctx, "mp", "dir/hello.txt", uploadID, 4, 10, strings.NewReader("abc"))
	shortErr = s3ErrorCode(shortErr)
	if shortErr != gofakes3.ErrIncompleteBody {
		t.Fatalf("short read error = %v, want IncompleteBody", shortErr)
	}

	// Unknown upload id.
	_, uerr := b.UploadPart(ctx, "mp", "dir/hello.txt", "does-not-exist", 1, 1, strings.NewReader("x"))
	if code := s3ErrorCode(uerr); code != gofakes3.ErrNoSuchUpload {
		t.Fatalf("unknown upload error = %v, want NoSuchUpload", code)
	}
	// Out-of-range part number.
	_, perr := b.UploadPart(ctx, "mp", "dir/hello.txt", uploadID, 0, 1, strings.NewReader("x"))
	if code := s3ErrorCode(perr); code != gofakes3.ErrInvalidPart {
		t.Fatalf("part 0 error = %v, want InvalidPart", code)
	}

	// Parts out of order.
	_, _, err := b.CompleteMultipartUpload(ctx, "mp", "dir/hello.txt", uploadID, &gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{
			{PartNumber: 2, ETag: etag2},
			{PartNumber: 1, ETag: etag1},
		},
	})
	if code := s3ErrorCode(err); code != gofakes3.ErrInvalidPartOrder {
		t.Fatalf("out-of-order complete error = %v, want InvalidPartOrder", code)
	}

	// Wrong etag.
	_, _, err = b.CompleteMultipartUpload(ctx, "mp", "dir/hello.txt", uploadID, &gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{
			{PartNumber: 1, ETag: etag1},
			{PartNumber: 2, ETag: `"deadbeef"`},
			{PartNumber: 3, ETag: etag3},
		},
	})
	if code := s3ErrorCode(err); code != gofakes3.ErrInvalidPart {
		t.Fatalf("wrong etag complete error = %v, want InvalidPart", code)
	}

	// Missing part number.
	_, _, err = b.CompleteMultipartUpload(ctx, "mp", "dir/hello.txt", uploadID, &gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{
			{PartNumber: 1, ETag: etag1},
			{PartNumber: 99, ETag: etag3},
		},
	})
	if code := s3ErrorCode(err); code != gofakes3.ErrInvalidPart {
		t.Fatalf("missing part complete error = %v, want InvalidPart", code)
	}

	// The failed completes must leave the upload available for retry.
	if _, ok := b.uploads.Load(uploadID); !ok {
		t.Fatal("upload was removed after a failed complete")
	}

	// Successful complete: returns immediately with the final etag; the
	// object is assembled by the background finalizer (eventual consistency).
	_, etag, err := b.CompleteMultipartUpload(ctx, "mp", "dir/hello.txt", uploadID, &gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{
			{PartNumber: 1, ETag: etag1},
			{PartNumber: 2, ETag: etag2},
			{PartNumber: 3, ETag: etag3},
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %+v", err)
	}
	if !strings.HasSuffix(etag, `-3"`) || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("complete etag = %q, want a quoted \"<hex>-3\" multipart etag", etag)
	}

	// The object must appear on disk with the concatenated content once the
	// finalization finishes.
	objPath := filepath.Join(localRoot, "dir", "hello.txt")
	waitFor(t, 10*time.Second, func() bool {
		got, err := os.ReadFile(objPath)
		return err == nil && bytes.Equal(got, []byte("Hello, multipart world!"))
	})

	// Bookkeeping and temp files are cleaned up asynchronously on success.
	waitUploadGone(t, b, uploadID)
}

func TestMultipartAbort(t *testing.T) {
	ctx := context.Background()
	b, _, _ := setupMultipartBackend(t)

	uploadID := mustCreateUpload(t, b, "mp", "abort.txt", nil)
	mustUploadPart(t, b, "mp", "abort.txt", uploadID, 1, "abc")

	state, _ := b.uploads.Load(uploadID)
	dir := state.(*multipartState).dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("temp dir missing before abort: %+v", err)
	}

	if err := b.AbortMultipartUpload(ctx, "mp", "abort.txt", uploadID); err != nil {
		t.Fatalf("AbortMultipartUpload: %+v", err)
	}
	if _, ok := b.uploads.Load(uploadID); ok {
		t.Fatal("upload still tracked after abort")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir still exists after abort (err=%v)", err)
	}

	// Aborting an unknown upload must be idempotent.
	if err := b.AbortMultipartUpload(ctx, "mp", "abort.txt", "nope"); err != nil {
		t.Fatalf("abort unknown upload returned error: %+v", err)
	}
}

func TestMultipartReapExpired(t *testing.T) {
	b, _, _ := setupMultipartBackend(t)

	// An active upload (fresh lastActivity) must be kept.
	freshID := mustCreateUpload(t, b, "mp", "fresh.txt", nil)

	// An abandoned upload (stale lastActivity) must be reaped.
	staleID := mustCreateUpload(t, b, "mp", "stale.txt", nil)
	mustUploadPart(t, b, "mp", "stale.txt", staleID, 1, "abc")
	val, _ := b.uploads.Load(staleID)
	st := val.(*multipartState)
	staleDir := st.dir
	if _, err := os.Stat(staleDir); err != nil {
		t.Fatalf("stale temp dir missing: %+v", err)
	}

	// Force the stale upload's lastActivity well into the past.
	ttl := 30 * time.Minute
	now := time.Now()
	st.mu.Lock()
	st.lastActivity = now.Add(-2 * ttl)
	st.mu.Unlock()

	b.reapExpired(now, ttl)

	if _, ok := b.uploads.Load(staleID); ok {
		t.Fatal("stale upload still tracked after reap")
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("stale temp dir still exists after reap (err=%v)", err)
	}
	if _, ok := b.uploads.Load(freshID); !ok {
		t.Fatal("fresh upload was reaped, should have been kept")
	}
}

func TestMultipartCleanupStaleDirs(t *testing.T) {
	b, _, _ := setupMultipartBackend(t)

	tempDir := conf.Conf.TempDir
	staleDir, err := os.MkdirTemp(tempDir, multipartDirPrefix+"*")
	if err != nil {
		t.Fatalf("mkdir stale dir: %v", err)
	}
	freshDir, err := os.MkdirTemp(tempDir, multipartDirPrefix+"*")
	if err != nil {
		t.Fatalf("mkdir fresh dir: %v", err)
	}
	// Age the stale dir beyond the TTL; leave the fresh dir young.
	ttl := 30 * time.Minute
	now := time.Now()
	past := now.Add(-2 * ttl)
	if err := os.Chtimes(staleDir, past, past); err != nil {
		t.Fatalf("chtimes stale dir: %v", err)
	}

	b.cleanupStaleDirs(now, ttl)

	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("stale dir should have been removed (err=%v)", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Fatalf("fresh dir should have been kept (err=%v)", err)
	}
	_ = os.RemoveAll(freshDir)
}

// TestMultipartCompleteIsAsync locks down the async-completion contract:
// CompleteMultipartUpload must answer immediately (before the parts are
// streamed to storage), deduplicate repeated Complete calls, and honor an
// Abort that arrives while the finalization is still queued.
func TestMultipartCompleteIsAsync(t *testing.T) {
	ctx := context.Background()
	b, localRoot, _ := setupMultipartBackend(t)

	uploadID := mustCreateUpload(t, b, "mp", "async.txt", nil)
	e1 := mustUploadPart(t, b, "mp", "async.txt", uploadID, 1, "abc")
	e2 := mustUploadPart(t, b, "mp", "async.txt", uploadID, 2, "def")
	parts := &gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{
			{PartNumber: 1, ETag: e1},
			{PartNumber: 2, ETag: e2},
		},
	}

	// Occupy both finalize slots so the finalization cannot start yet.
	finalizeSlots <- struct{}{}
	finalizeSlots <- struct{}{}

	_, etag, err := b.CompleteMultipartUpload(ctx, "mp", "async.txt", uploadID, parts)
	if err != nil {
		t.Fatalf("CompleteMultipartUpload blocked or failed: %+v", err)
	}
	if !strings.HasSuffix(etag, `-2"`) {
		t.Fatalf("etag = %q, want quoted \"<hex>-2\"", etag)
	}

	// The object must not exist yet: completion returned before finalization.
	if _, err := os.Stat(filepath.Join(localRoot, "async.txt")); !os.IsNotExist(err) {
		t.Fatalf("object exists before finalization ran (err=%v)", err)
	}

	// A repeated Complete while finalizing is deduplicated with the same etag.
	_, etag2, err := b.CompleteMultipartUpload(ctx, "mp", "async.txt", uploadID, parts)
	if err != nil {
		t.Fatalf("deduplicated CompleteMultipartUpload: %+v", err)
	}
	if etag2 != etag {
		t.Fatalf("dedup etag = %q, want %q", etag2, etag)
	}

	// Abort while the finalization is queued must be recorded, not rejected.
	if err := b.AbortMultipartUpload(ctx, "mp", "async.txt", uploadID); err != nil {
		t.Fatalf("AbortMultipartUpload while completing: %+v", err)
	}

	// Release the slots: the queued finalization observes the abort and drops
	// the upload without writing the object.
	<-finalizeSlots
	<-finalizeSlots

	waitUploadGone(t, b, uploadID)
	if _, err := os.Stat(filepath.Join(localRoot, "async.txt")); !os.IsNotExist(err) {
		t.Fatalf("object written despite abort before finalization (err=%v)", err)
	}
}

// TestMultipartCompleteRetryAfterFailure verifies the retry contract when the
// background finalization fails (here: the backing storage is disabled): the
// upload must return to the retryable state, and a later Complete with the
// same parts must succeed once the storage is back.
func TestMultipartCompleteRetryAfterFailure(t *testing.T) {
	ctx := context.Background()
	b, localRoot, sid := setupMultipartBackend(t)

	uploadID := mustCreateUpload(t, b, "mp", "retry.txt", nil)
	e1 := mustUploadPart(t, b, "mp", "retry.txt", uploadID, 1, "abc")
	parts := &gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{{PartNumber: 1, ETag: e1}},
	}

	// Break the backing storage so the background finalization fails.
	if err := op.DisableStorage(ctx, sid); err != nil {
		t.Fatalf("DisableStorage: %+v", err)
	}

	if _, _, err := b.CompleteMultipartUpload(ctx, "mp", "retry.txt", uploadID, parts); err != nil {
		t.Fatalf("CompleteMultipartUpload should be accepted even if storage is down: %+v", err)
	}

	// The failed finalization must put the upload back into retryable state.
	waitFor(t, 10*time.Second, func() bool {
		val, ok := b.uploads.Load(uploadID)
		if !ok {
			return false
		}
		st := val.(*multipartState)
		st.mu.Lock()
		defer st.mu.Unlock()
		return !st.completing
	})

	// Repair the storage and retry with the same parts.
	if err := op.EnableStorage(ctx, sid); err != nil {
		t.Fatalf("EnableStorage: %+v", err)
	}
	_, etag, err := b.CompleteMultipartUpload(ctx, "mp", "retry.txt", uploadID, parts)
	if err != nil {
		t.Fatalf("retry CompleteMultipartUpload: %+v", err)
	}
	if !strings.HasSuffix(etag, `-1"`) {
		t.Fatalf("etag = %q, want quoted \"<hex>-1\"", etag)
	}

	waitFor(t, 10*time.Second, func() bool {
		got, err := os.ReadFile(filepath.Join(localRoot, "retry.txt"))
		return err == nil && string(got) == "abc"
	})
	waitUploadGone(t, b, uploadID)
}

// TestMultipartHeadPendingDuringFinalize verifies that HEAD serves the
// committed size while the finalization is still queued/running, so clients
// verifying immediately after CompleteMultipartUpload (rclone) do not see a
// missing or zero-sized object.
func TestMultipartHeadPendingDuringFinalize(t *testing.T) {
	ctx := context.Background()
	b, _, _ := setupMultipartBackend(t)

	uploadID := mustCreateUpload(t, b, "mp", "pending.txt", nil)
	e1 := mustUploadPart(t, b, "mp", "pending.txt", uploadID, 1, "abc")
	e2 := mustUploadPart(t, b, "mp", "pending.txt", uploadID, 2, "defgh")

	// Missing before completion.
	if _, err := b.HeadObject(ctx, "mp", "pending.txt"); s3ErrorCode(err) != gofakes3.ErrNoSuchKey {
		t.Fatalf("HEAD before complete error = %v, want NoSuchKey", s3ErrorCode(err))
	}

	// Block the finalize slots so completion is accepted but not executed.
	finalizeSlots <- struct{}{}
	finalizeSlots <- struct{}{}

	parts := &gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{
			{PartNumber: 1, ETag: e1},
			{PartNumber: 2, ETag: e2},
		},
	}
	if _, _, err := b.CompleteMultipartUpload(ctx, "mp", "pending.txt", uploadID, parts); err != nil {
		t.Fatalf("CompleteMultipartUpload: %+v", err)
	}

	// HEAD during the finalize window returns the committed size (3+5=8).
	obj, err := b.HeadObject(ctx, "mp", "pending.txt")
	if err != nil {
		t.Fatalf("HEAD during finalize: %+v", err)
	}
	if obj.Size != 8 {
		t.Fatalf("pending HEAD size = %d, want 8", obj.Size)
	}

	<-finalizeSlots
	<-finalizeSlots
	waitUploadGone(t, b, uploadID)

	// After finalization the real object serves the same size.
	obj, err = b.HeadObject(ctx, "mp", "pending.txt")
	if err != nil {
		t.Fatalf("HEAD after finalize: %+v", err)
	}
	if obj.Size != 8 {
		t.Fatalf("final HEAD size = %d, want 8", obj.Size)
	}
}

// TestMultipartReapSkipsCompleting verifies that the reaper never drops an
// upload whose finalization is in progress, even if its lastActivity is stale.
func TestMultipartReapSkipsCompleting(t *testing.T) {
	b, _, _ := setupMultipartBackend(t)

	uploadID := mustCreateUpload(t, b, "mp", "busy.txt", nil)

	ttl := 30 * time.Minute
	now := time.Now()

	// Simulate an accepted completion that is still finalizing and looks stale.
	val, _ := b.uploads.Load(uploadID)
	st := val.(*multipartState)
	st.mu.Lock()
	st.completing = true
	st.lastActivity = now.Add(-2 * ttl)
	st.mu.Unlock()

	b.reapExpired(now, ttl)
	if _, ok := b.uploads.Load(uploadID); !ok {
		t.Fatal("reaper dropped an upload while finalization was in progress")
	}

	// Once the finalization is done, staleness applies again.
	st.mu.Lock()
	st.completing = false
	st.mu.Unlock()
	b.reapExpired(now, ttl)
	if _, ok := b.uploads.Load(uploadID); ok {
		t.Fatal("stale upload still tracked after finalization ended")
	}
}

// --- Single-PUT async pipeline (PutObject -> spool -> background finalize) ---

// TestSinglePutHeadReportsCommittedSizeImmediately verifies the contract
// uploading clients depend on: PutObject returns right away (the body is
// spooled), and a HEAD issued in the same instant reports the committed size
// instead of NoSuchKey.
func TestSinglePutHeadReportsCommittedSizeImmediately(t *testing.T) {
	ctx := context.Background()
	b, localRoot, _ := setupMultipartBackend(t)

	// Block both finalize slots so the background finalize cannot run yet.
	finalizeSlots <- struct{}{}
	finalizeSlots <- struct{}{}

	body := "single-put-payload-0123456789"
	if _, err := b.PutObject(ctx, "mp", "instant/hello.txt", map[string]string{"Content-Type": "text/plain"}, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutObject: %+v", err)
	}

	obj, err := b.HeadObject(ctx, "mp", "instant/hello.txt")
	if err != nil {
		t.Fatalf("HEAD right after PutObject: %+v", err)
	}
	if obj.Size != int64(len(body)) {
		t.Fatalf("HEAD size = %d, want %d", obj.Size, len(body))
	}

	// Release the finalize slots; the object must become durable.
	<-finalizeSlots
	<-finalizeSlots

	waitFor(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(localRoot, "instant", "hello.txt"))
		return err == nil && string(data) == body
	})
}

// TestSinglePutShortBodyFailsSync ensures a truncated request is rejected
// synchronously (ErrIncompleteBody) and leaves nothing behind.
func TestSinglePutShortBodyFailsSync(t *testing.T) {
	ctx := context.Background()
	b, _, _ := setupMultipartBackend(t)

	if _, err := b.PutObject(ctx, "mp", "short/x.txt", nil, strings.NewReader("abc"), 10); s3ErrorCode(err) != gofakes3.ErrIncompleteBody {
		t.Fatalf("short body error = %v, want ErrIncompleteBody", err)
	}

	empty := true
	b.uploads.Range(func(_, _ any) bool { empty = false; return false })
	if !empty {
		t.Fatal("short body left an upload state behind")
	}
}

// TestSinglePutFinalizedHeadCache proves the finalized-metadata cache serves
// HEADs after the background write completes, without touching storage.
func TestSinglePutFinalizedHeadCache(t *testing.T) {
	ctx := context.Background()
	b, localRoot, _ := setupMultipartBackend(t)

	body := "cached-head-payload"
	if _, err := b.PutObject(ctx, "mp", "cached/a.txt", nil, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutObject: %+v", err)
	}

	// Wait until the finalize pipeline published the hot metadata.
	waitFor(t, 10*time.Second, func() bool {
		_, ok := b.recentFinalized.Load("mp/cached/a.txt")
		return ok
	})

	// Remove the backing file: the recent metadata must still answer the HEAD.
	if err := os.Remove(filepath.Join(localRoot, "cached", "a.txt")); err != nil {
		t.Fatalf("remove backing file: %v", err)
	}
	obj, err := b.HeadObject(ctx, "mp", "cached/a.txt")
	if err != nil {
		t.Fatalf("HEAD after finalize with storage removed: %+v", err)
	}
	if obj.Size != int64(len(body)) {
		t.Fatalf("HEAD size = %d, want %d", obj.Size, len(body))
	}
}

// TestSinglePutDeleteInvalidatesFinalizedCache keeps DELETE authoritative:
// after deleting an object its hot finalize metadata must not resurrect it.
func TestSinglePutDeleteInvalidatesFinalizedCache(t *testing.T) {
	ctx := context.Background()
	b, _, _ := setupMultipartBackend(t)

	body := "delete-me"
	if _, err := b.PutObject(ctx, "mp", "todelete/a.txt", nil, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutObject: %+v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_, ok := b.recentFinalized.Load("mp/todelete/a.txt")
		return ok
	})

	if _, err := b.DeleteObject(ctx, "mp", "todelete/a.txt"); err != nil {
		t.Fatalf("DeleteObject: %+v", err)
	}
	if _, ok := b.recentFinalized.Load("mp/todelete/a.txt"); ok {
		t.Fatal("delete left finalized metadata in the cache")
	}
}

// TestSinglePutDirMarkerStaysSynchronous checks that trailing-slash objects
// (directory markers) still create the folder synchronously.
func TestSinglePutDirMarkerStaysSynchronous(t *testing.T) {
	ctx := context.Background()
	b, localRoot, _ := setupMultipartBackend(t)

	if _, err := b.PutObject(ctx, "mp", "marker/", nil, strings.NewReader(""), 0); err != nil {
		t.Fatalf("PutObject dir marker: %+v", err)
	}
	if st, err := os.Stat(filepath.Join(localRoot, "marker")); err != nil || !st.IsDir() {
		t.Fatalf("marker dir missing after sync put: %v", err)
	}
}

// TestSinglePutFinalizeRetriesItself disables the backing storage so the
// background finalize fails; the single-PUT pipeline must retry itself and
// succeed once the storage returns, with no client re-issuing anything.
func TestSinglePutFinalizeRetriesItself(t *testing.T) {
	ctx := context.Background()
	b, localRoot, sid := setupMultipartBackend(t)

	if err := op.DisableStorage(ctx, sid); err != nil {
		t.Fatalf("DisableStorage: %+v", err)
	}
	body := "retry-me"
	if _, err := b.PutObject(ctx, "mp", "retry/x.txt", nil, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutObject: %+v", err)
	}

	// First finalize fails fast (disabled storage), the retry backoff then
	// waits; repair before the retry budget is spent.
	if err := op.EnableStorage(ctx, sid); err != nil {
		t.Fatalf("EnableStorage: %+v", err)
	}

	waitFor(t, 30*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(localRoot, "retry", "x.txt"))
		return err == nil && string(data) == body
	})

	// The state must be fully reclaimed once the retry succeeds.
	waitFor(t, 10*time.Second, func() bool {
		empty := true
		b.uploads.Range(func(_, _ any) bool { empty = false; return false })
		return empty
	})
}

// TestSinglePutDeleteDuringFinalizeWins keeps a delete issued inside the
// finalize window authoritative: the in-flight background write must not
// resurrect the object afterwards.
func TestSinglePutDeleteDuringFinalizeWins(t *testing.T) {
	ctx := context.Background()
	b, _, _ := setupMultipartBackend(t)

	// Block the finalize slots so the window is observable.
	finalizeSlots <- struct{}{}
	finalizeSlots <- struct{}{}

	body := "doomed"
	if _, err := b.PutObject(ctx, "mp", "doomed/a.txt", nil, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutObject: %+v", err)
	}
	if _, err := b.DeleteObject(ctx, "mp", "doomed/a.txt"); err != nil {
		t.Fatalf("DeleteObject: %+v", err)
	}

	<-finalizeSlots
	<-finalizeSlots

	// After the finalize finishes it must honor the delete: no hot metadata,
	// no state left, and (storage still enabled) HEAD falls through to 404.
	waitFor(t, 10*time.Second, func() bool {
		_, hotOk := b.recentFinalized.Load("mp/doomed/a.txt")
		empty := true
		b.uploads.Range(func(_, _ any) bool { empty = false; return false })
		return !hotOk && empty
	})
	if _, err := b.HeadObject(ctx, "mp", "doomed/a.txt"); s3ErrorCode(err) != gofakes3.ErrNoSuchKey {
		t.Fatalf("HEAD after delete-while-finalizing = %v, want NoSuchKey", err)
	}
}
