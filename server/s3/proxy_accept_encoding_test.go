package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/gofakes3"
)

// TestS3HeadThroughAcceptEncodingRewritingProxy covers the production failure
// where a reverse proxy (Cloudflare on HEAD requests) rewrites or strips the
// signed Accept-Encoding header between the client and the server: rclone's
// post-upload HeadObject then failed with SignatureDoesNotMatch (403) and the
// whole upload was retried to death. The patched gofakes3 verifies the
// request by retrying common Accept-Encoding values.
func TestS3HeadThroughAcceptEncodingRewritingProxy(t *testing.T) {
	_, localRoot, _ := setupMultipartBackend(t)
	if err := os.WriteFile(filepath.Join(localRoot, "obj.jpg"), []byte("photo-bytes"), 0o644); err != nil {
		t.Fatalf("write object: %v", err)
	}

	const (
		ak     = "test-ak"
		sk     = "test-sk-0123456789"
		region = "us-east-1"
	)
	faker := gofakes3.New(newBackend(), gofakes3.WithV4Auth(map[string]string{ak: sk}))

	// Cloudflare stand-in. The "cf" case replicates what Cloudflare actually
	// does to a HEAD on the wire: reissue it to the origin as GET (cache
	// semantics) and normalize Accept-Encoding to "gzip, br". The "stripped"
	// case covers proxies that just drop the header.
	for _, tc := range []struct {
		name   string
		mutate func(r *http.Request)
	}{
		{"cf-head-to-get", func(r *http.Request) {
			r.Method = http.MethodGet
			r.Header.Set("Accept-Encoding", "gzip, br")
		}},
		{"stripped", func(r *http.Request) { r.Header.Del("Accept-Encoding") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					tc.mutate(r)
				}
				faker.Server().ServeHTTP(w, r)
			})
			ts := httptest.NewServer(proxy)
			defer ts.Close()

			for _, path := range []string{"/mp/obj.jpg", "/mp/missing.jpg"} {
				status := doSignedHead(t, ts.URL+path, ak, sk, region)
				switch path {
				case "/mp/obj.jpg":
					if status != http.StatusOK {
						t.Errorf("HEAD existing: got %d, want 200", status)
					}
				default:
					if status != http.StatusNotFound {
						t.Errorf("HEAD missing: got %d, want 404", status)
					}
				}
			}
		})
	}
}

// doSignedHead issues a HEAD request signed with SigV4 exactly the way
// rclone's aws-sdk-go-v2 client does: Accept-Encoding: identity is sent and
// included in SignedHeaders. Returns the response status code.
func doSignedHead(t *testing.T, url, ak, sk, region string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256)

	// Signed headers: host + accept-encoding + x-amz-* (sorted).
	signed := []string{"accept-encoding", "host", "x-amz-content-sha256", "x-amz-date"}
	var canonicalHeaders strings.Builder
	for _, h := range signed {
		v := ""
		switch h {
		case "host":
			v = req.URL.Host
		case "accept-encoding":
			v = "identity"
		case "x-amz-content-sha256":
			v = req.Header.Get("X-Amz-Content-Sha256")
		case "x-amz-date":
			v = amzDate
		}
		canonicalHeaders.WriteString(h + ":" + v + "\n")
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		"",
		canonicalHeaders.String(),
		strings.Join(signed, ";"),
		emptyPayloadSHA256,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", now.Format("20060102"), region)
	crSum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crSum[:]),
	}, "\n")

	dateKey := hmacSHA256([]byte("AWS4"+sk), now.Format("20060102"))
	regionKey := hmacSHA256(dateKey, region)
	serviceKey := hmacSHA256(regionKey, "s3")
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		ak, scope, strings.Join(signed, ";"), signature))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// TestS3HeadUnmodifiedAcceptEncodingStillVerifies keeps the untouched path
// honest: no proxy rewriting, plain signed HEAD must behave as before.
func TestS3HeadUnmodifiedAcceptEncodingStillVerifies(t *testing.T) {
	_, localRoot, _ := setupMultipartBackend(t)
	if err := os.WriteFile(filepath.Join(localRoot, "obj.jpg"), []byte("photo-bytes"), 0o644); err != nil {
		t.Fatalf("write object: %v", err)
	}

	const (
		ak     = "test-ak"
		sk     = "test-sk-0123456789"
		region = "us-east-1"
	)
	faker := gofakes3.New(newBackend(), gofakes3.WithV4Auth(map[string]string{ak: sk}))
	ts := httptest.NewServer(faker.Server())
	defer ts.Close()

	if status := doSignedHead(t, ts.URL+"/mp/obj.jpg", ak, sk, region); status != http.StatusOK {
		t.Errorf("got %d, want 200", status)
	}
}
