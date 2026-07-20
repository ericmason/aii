package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxObjectSize caps what we're willing to download into memory. The
// engine also checks listed sizes before fetching; this is the
// belt-and-braces limit for objects fetched without a prior listing.
const maxObjectSize = 1 << 30 // 1 GiB

// S3 is a Remote backed by any S3-compatible object store: AWS S3,
// Cloudflare R2, Backblaze B2, MinIO. Requests are signed with SigV4
// implemented on the stdlib — no AWS SDK.
type S3 struct {
	bucket    string
	keyPrefix string // repo prefix inside the bucket, "" or "p/" form
	endpoint  string // "" = AWS regional endpoint
	region    string
	pathStyle bool
	creds     credentials
	client    *http.Client

	now func() time.Time // injected in tests

	warnedNoCond bool // one-shot warning when If-None-Match unsupported
}

type S3Config struct {
	Bucket    string
	Prefix    string // key prefix inside the bucket ("team/aii")
	Endpoint  string // override for R2/B2/MinIO ("https://minio.local:9000")
	Region    string // default us-east-1 ("auto" for R2)
	PathStyle bool

	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("s3: credentials required (AII_SYNC_S3_ACCESS_KEY_ID / AWS_ACCESS_KEY_ID env, or config)")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	// A custom endpoint (MinIO, R2) usually means no wildcard TLS for
	// bucket subdomains — default to path-style there.
	pathStyle := cfg.PathStyle || cfg.Endpoint != ""
	return &S3{
		bucket:    cfg.Bucket,
		keyPrefix: prefix,
		endpoint:  strings.TrimSuffix(cfg.Endpoint, "/"),
		region:    region,
		pathStyle: pathStyle,
		creds:     credentials{cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken},
		client: &http.Client{
			Transport: &http.Transport{ResponseHeaderTimeout: time.Minute},
		},
		now: time.Now,
	}, nil
}

func (s *S3) String() string {
	loc := "s3://" + s.bucket
	if s.keyPrefix != "" {
		loc += "/" + strings.TrimSuffix(s.keyPrefix, "/")
	}
	if s.endpoint != "" {
		loc += " (" + s.endpoint + ")"
	}
	return loc
}

// baseURL returns scheme://host and the path prefix for the bucket.
func (s *S3) baseURL() (host, pathPrefix string) {
	if s.endpoint != "" {
		u, _ := url.Parse(s.endpoint)
		if s.pathStyle {
			return s.endpoint, "/" + s.bucket
		}
		return u.Scheme + "://" + s.bucket + "." + u.Host, ""
	}
	if s.pathStyle {
		return "https://s3." + s.region + ".amazonaws.com", "/" + s.bucket
	}
	return "https://" + s.bucket + ".s3." + s.region + ".amazonaws.com", ""
}

// apiError is an S3 XML error response.
type apiError struct {
	Status  int
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("s3: HTTP %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Code == "RequestTimeTooSkewed" {
		msg += " (your system clock appears to be wrong — SigV4 requires it within 15 minutes of the server)"
	}
	return msg
}

// do performs one signed request with bounded retries on transport
// errors and 5xx/429 responses. Returns the response body.
func (s *S3) do(ctx context.Context, method, key, query string, body []byte, extraHdr map[string]string) ([]byte, *http.Response, error) {
	host, pathPrefix := s.baseURL()
	fullKey := s.keyPrefix + key
	rawURL := host + pathPrefix + "/" + escapePath(fullKey)
	if key == "" { // bucket-level request (List)
		rawURL = host + pathPrefix + "/"
	}
	if query != "" {
		rawURL += "?" + query
	}

	payloadHash := emptyPayloadSHA256
	if len(body) > 0 {
		h := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(h[:])
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(200*(1<<attempt))*time.Millisecond + time.Duration(rand.Intn(200))*time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		for k, v := range extraHdr {
			req.Header.Set(k, v)
		}
		signV4(req, s.creds, s.region, payloadHash, s.now())

		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectSize+1))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if len(respBody) > maxObjectSize {
			return nil, resp, fmt.Errorf("s3: object %s exceeds %d byte limit", key, maxObjectSize)
		}
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = parseAPIError(resp.StatusCode, respBody)
			continue
		}
		if resp.StatusCode >= 400 {
			return respBody, resp, parseAPIError(resp.StatusCode, respBody)
		}
		return respBody, resp, nil
	}
	return nil, nil, fmt.Errorf("s3: giving up after 3 attempts: %w", lastErr)
}

func parseAPIError(status int, body []byte) *apiError {
	e := &apiError{Status: status}
	_ = xml.Unmarshal(body, e)
	return e
}

// escapePath escapes an object key for the URL path, one segment at a
// time, keeping '/' separators.
func escapePath(key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = awsEscape(seg)
	}
	return strings.Join(segs, "/")
}

type listBucketResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", s.keyPrefix+prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		body, _, err := s.do(ctx, http.MethodGet, "", q.Encode(), nil, nil)
		if err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}
		var res listBucketResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("list: parse response: %w", err)
		}
		for _, c := range res.Contents {
			key := strings.TrimPrefix(c.Key, s.keyPrefix)
			out = append(out, Object{Key: key, Size: c.Size, ModTime: c.LastModified})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	body, _, err := s.do(ctx, http.MethodGet, key, "", nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == 404 || ae.Code == "NoSuchKey") {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return body, nil
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	_, _, err := s.do(ctx, http.MethodPut, key, "", data, nil)
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (s *S3) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	_, _, err := s.do(ctx, http.MethodPut, key, "", data, map[string]string{"If-None-Match": "*"})
	var ae *apiError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ae) && (ae.Status == 412 || ae.Code == "PreconditionFailed"):
		return ErrExists
	case errors.As(err, &ae) && (ae.Status == 501 || ae.Code == "NotImplemented"):
		// Provider doesn't support conditional writes. Degrade to
		// check-then-put: not atomic, but bundle names are
		// content-derived so a lost race writes identical bytes.
		if !s.warnedNoCond {
			s.warnedNoCond = true
			fmt.Fprintln(os.Stderr, "aii: warning: this S3 provider does not support conditional writes (If-None-Match); falling back to check-then-put")
		}
		if _, gerr := s.Get(ctx, key); gerr == nil {
			return ErrExists
		} else if !errors.Is(gerr, ErrNotExist) {
			return gerr
		}
		return s.Put(ctx, key, data)
	default:
		return fmt.Errorf("put-if-absent %s: %w", key, err)
	}
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, _, err := s.do(ctx, http.MethodDelete, key, "", nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == 404 || ae.Code == "NoSuchKey") {
		return nil // absent = success
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}
