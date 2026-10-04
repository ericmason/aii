package remote

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4 for the S3 service, on the stdlib only.
// Reference: docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html
//
// We always sign the real payload hash (x-amz-content-sha256) — never
// UNSIGNED-PAYLOAD — so the signature covers the body too. Bodies are
// in memory anyway.

const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// signV4 computes and sets the Authorization header on req. The
// x-amz-date, x-amz-content-sha256, and (when a session token is
// present) x-amz-security-token headers are set here as well so they
// are guaranteed to match what was signed. payloadHash is the lowercase
// hex SHA-256 of the request body.
func signV4(req *http.Request, creds credentials, region string, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	shortDate := amzDate[:8]

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.SessionToken)
	}

	// Canonical headers: host plus every x-amz-* header we set, plus
	// range and content-type when present — lowercase names, sorted.
	type kv struct{ k, v string }
	hdrs := []kv{{"host", req.Host}}
	if req.Host == "" {
		hdrs[0].v = req.URL.Host
	}
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") || lower == "range" || lower == "content-type" {
			hdrs = append(hdrs, kv{lower, strings.TrimSpace(vals[0])})
		}
	}
	sort.Slice(hdrs, func(i, j int) bool { return hdrs[i].k < hdrs[j].k })

	var canonHdrs, signedHdrs strings.Builder
	for i, h := range hdrs {
		canonHdrs.WriteString(h.k + ":" + h.v + "\n")
		if i > 0 {
			signedHdrs.WriteByte(';')
		}
		signedHdrs.WriteString(h.k)
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonHdrs.String(),
		signedHdrs.String(),
		payloadHash,
	}, "\n")

	scope := shortDate + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), []byte(shortDate))
	key = hmacSHA256(key, []byte(region))
	key = hmacSHA256(key, []byte("s3"))
	key = hmacSHA256(key, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+creds.AccessKeyID+"/"+scope+
			", SignedHeaders="+signedHdrs.String()+
			", Signature="+signature)
}

// canonicalURI is the URI-encoded path with each segment escaped once
// (S3 does not double-encode, unlike other AWS services).
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	// Re-encode from the decoded path so escaping is exactly AWS's
	// set: everything but unreserved characters and '/'.
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return awsEncodeQuery(u.Query())
}

// awsEncodeQuery renders query parameters the way SigV4 canonicalizes
// them, so a caller can build RawQuery with it and get a wire query
// that matches the signed canonical query byte for byte.
// url.Values.Encode is not interchangeable: it writes a space as '+',
// which canonicalQuery reads back as a space and re-escapes to %20,
// so any key or prefix containing a space fails the signature check.
func awsEncodeQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(awsEscape(k) + "=" + awsEscape(v))
		}
	}
	return b.String()
}

// awsEscape percent-encodes per RFC 3986 with AWS's unreserved set:
// A-Z a-z 0-9 - . _ ~. Space becomes %20, not '+'.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}
