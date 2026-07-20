package remote

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Golden vectors from the AWS SigV4 documentation ("Authenticating
// Requests: Using the Authorization Header", examplebucket examples).
// If these pass, the canonicalization and signing chain match AWS
// exactly.
func TestSigV4GoldenVectors(t *testing.T) {
	creds := credentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	t.Run("get-object", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
		req.Header.Set("Range", "bytes=0-9")
		signV4(req, creds, "us-east-1", emptyPayloadSHA256, when)
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
			"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
			"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization mismatch:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("get-lifecycle", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?lifecycle", nil)
		signV4(req, creds, "us-east-1", emptyPayloadSHA256, when)
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
			"Signature=fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization mismatch:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("list-objects", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
		signV4(req, creds, "us-east-1", emptyPayloadSHA256, when)
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
			"Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization mismatch:\n got %s\nwant %s", got, want)
		}
	})
}

// fakeS3 is a minimal in-memory S3 endpoint: path-style objects,
// paginated ListObjectsV2, NoSuchKey XML errors, conditional PUT.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	pageSize int
	fail500  int  // fail the next N requests with a 500
	no501    bool // if true, conditional PUT returns 501
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail500 > 0 {
		f.fail500--
		w.WriteHeader(500)
		fmt.Fprint(w, `<Error><Code>InternalError</Code><Message>boom</Message></Error>`)
		return
	}
	if r.Header.Get("Authorization") == "" || r.Header.Get("x-amz-date") == "" {
		w.WriteHeader(403)
		fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>unsigned</Message></Error>`)
		return
	}

	// Path-style: /<bucket>/<key...>
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	key := ""
	if len(parts) == 2 {
		key, _ = url.PathUnescape(parts[1])
	}

	switch {
	case r.Method == http.MethodGet && key == "": // ListObjectsV2
		prefix := r.URL.Query().Get("prefix")
		start := 0
		if tok := r.URL.Query().Get("continuation-token"); tok != "" {
			start, _ = strconv.Atoi(tok)
		}
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		end := start + f.pageSize
		truncated := end < len(keys)
		if end > len(keys) {
			end = len(keys)
		}
		type contents struct {
			Key          string    `xml:"Key"`
			Size         int64     `xml:"Size"`
			LastModified time.Time `xml:"LastModified"`
		}
		res := struct {
			XMLName               xml.Name   `xml:"ListBucketResult"`
			IsTruncated           bool       `xml:"IsTruncated"`
			NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
			Contents              []contents `xml:"Contents"`
		}{IsTruncated: truncated}
		if truncated {
			res.NextContinuationToken = strconv.Itoa(end)
		}
		for _, k := range keys[start:end] {
			res.Contents = append(res.Contents, contents{k, int64(len(f.objects[k])), time.Unix(1700000000, 0).UTC()})
		}
		xml.NewEncoder(w).Encode(res)

	case r.Method == http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`)
			return
		}
		w.Write(b)

	case r.Method == http.MethodPut:
		if r.Header.Get("If-None-Match") == "*" {
			if f.no501 {
				w.WriteHeader(501)
				fmt.Fprint(w, `<Error><Code>NotImplemented</Code><Message>conditional writes unsupported</Message></Error>`)
				return
			}
			if _, ok := f.objects[key]; ok {
				w.WriteHeader(412)
				fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
				return
			}
		}
		body := make([]byte, 0)
		if r.Body != nil {
			buf := new(strings.Builder)
			_, _ = copyN(buf, r)
			body = []byte(buf.String())
		}
		f.objects[key] = body
		w.WriteHeader(200)

	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(204)

	default:
		w.WriteHeader(400)
	}
}

func copyN(dst *strings.Builder, r *http.Request) (int64, error) {
	buf := make([]byte, 32*1024)
	var n int64
	for {
		m, err := r.Body.Read(buf)
		dst.Write(buf[:m])
		n += int64(m)
		if err != nil {
			return n, nil
		}
	}
}

func newFakeS3(t *testing.T, f *fakeS3) *S3 {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := NewS3(S3Config{
		Bucket:          "testbucket",
		Prefix:          "repo",
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		AccessKeyID:     "test",
		SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3Roundtrip(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string][]byte{}, pageSize: 100}
	s := newFakeS3(t, f)

	if _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get missing = %v, want ErrNotExist", err)
	}
	if err := s.Put(ctx, "bundles/aa/1-1-x.age", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	// Key prefix is applied inside the bucket.
	if _, ok := f.objects["repo/bundles/aa/1-1-x.age"]; !ok {
		t.Fatalf("object not stored under prefix; stored keys: %v", mapKeys(f.objects))
	}
	got, err := s.Get(ctx, "bundles/aa/1-1-x.age")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Fatalf("Get = %q", got)
	}
	if err := s.Delete(ctx, "bundles/aa/1-1-x.age"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "bundles/aa/1-1-x.age"); err != nil {
		t.Fatalf("delete absent = %v, want nil", err)
	}
}

func TestS3PutIfAbsent(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string][]byte{}, pageSize: 100}
	s := newFakeS3(t, f)

	if err := s.PutIfAbsent(ctx, "keys/master.age", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIfAbsent(ctx, "keys/master.age", []byte("second")); !errors.Is(err, ErrExists) {
		t.Fatalf("second PutIfAbsent = %v, want ErrExists", err)
	}
	if string(f.objects["repo/keys/master.age"]) != "first" {
		t.Fatal("loser overwrote winner")
	}
}

func TestS3PutIfAbsentFallback501(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string][]byte{}, pageSize: 100, no501: true}
	s := newFakeS3(t, f)

	if err := s.PutIfAbsent(ctx, "keys/master.age", []byte("first")); err != nil {
		t.Fatalf("fallback put = %v", err)
	}
	if err := s.PutIfAbsent(ctx, "keys/master.age", []byte("second")); !errors.Is(err, ErrExists) {
		t.Fatalf("fallback second PutIfAbsent = %v, want ErrExists", err)
	}
}

func TestS3ListPagination(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string][]byte{}, pageSize: 2}
	s := newFakeS3(t, f)

	keys := []string{"bundles/aa/1.age", "bundles/bb/2.age", "bundles/cc/3.age", "bundles/dd/4.age", "bundles/ee/5.age", "other/x"}
	for _, k := range keys {
		if err := s.Put(ctx, k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, "bundles/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("List = %d objects, want 5: %+v", len(got), got)
	}
	for _, o := range got {
		if !strings.HasPrefix(o.Key, "bundles/") {
			t.Errorf("listed key %q not stripped of bucket prefix or out of scope", o.Key)
		}
		if o.Size <= 0 {
			t.Errorf("key %q has size %d", o.Key, o.Size)
		}
	}
}

func TestS3RetryOn500(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string][]byte{}, pageSize: 100, fail500: 2}
	s := newFakeS3(t, f)

	if err := s.Put(ctx, "x", []byte("v")); err != nil {
		t.Fatalf("Put with 2 transient 500s = %v, want success on 3rd attempt", err)
	}
	f.fail500 = 3
	if err := s.Put(ctx, "y", []byte("v")); err == nil {
		t.Fatal("Put with 3 persistent 500s succeeded, want failure")
	}
}

func mapKeys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
