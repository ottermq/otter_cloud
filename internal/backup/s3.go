package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// S3 is a Target in a bucket of an S3-compatible service, commercial or
// self-hosted (architecture.md §10). It speaks the four calls it needs,
// path-style and signed with AWS Signature Version 4, and nothing else.
// Each object is one PUT, so it holds up to 5 GiB.
type S3 struct {
	// Endpoint is the service's scheme and host, e.g. https://s3.example.com.
	Endpoint *url.URL
	Bucket   string
	// Prefix is put before every key: empty, or ending in a slash.
	Prefix    string
	Region    string
	AccessKey string
	SecretKey string
	Client    *http.Client // http.DefaultClient when nil
	now       func() time.Time
}

// emptySHA256 is the SHA-256 of no bytes, the payload hash of a request
// without a body.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (t *S3) Put(ctx context.Context, key string, b Blob) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	if _, err := b.Body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := t.request(ctx, http.MethodPut, t.Prefix+key, nil, io.NopCloser(b.Body))
	if err != nil {
		return err
	}
	req.ContentLength = b.Size
	// The signed payload hash is the blob's: the service refuses a body
	// that doesn't match it.
	resp, err := t.do(req, b.SHA256)
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return resp.Body.Close()
}

func (t *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := CheckKey(key); err != nil {
		return nil, err
	}
	req, err := t.request(ctx, http.MethodGet, t.Prefix+key, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.do(req, emptySHA256)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return resp.Body, nil
}

func (t *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, err
	}
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {t.Prefix + prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := t.request(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		resp, err := t.do(req, emptySHA256)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		var page struct {
			Contents []struct {
				Key  string
				Size int64
			}
			IsTruncated           bool
			NextContinuationToken string
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, c := range page.Contents {
			key, ok := strings.CutPrefix(c.Key, t.Prefix)
			if ok && CheckKey(key) == nil {
				out = append(out, Object{Key: key, Size: c.Size})
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	slices.SortFunc(out, func(a, b Object) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func (t *S3) Delete(ctx context.Context, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	req, err := t.request(ctx, http.MethodDelete, t.Prefix+key, nil, nil)
	if err != nil {
		return err
	}
	resp, err := t.do(req, emptySHA256)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return resp.Body.Close()
}

// request is a request for the object at key in the bucket, or for the
// bucket itself when key is empty.
func (t *S3) request(ctx context.Context, method, key string, q url.Values, body io.ReadCloser) (*http.Request, error) {
	u := *t.Endpoint
	u.Path = "/" + t.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body == nil {
		req.Body = nil
	}
	return req, nil
}

// do signs and sends req. Any status but 2xx is an error, ErrNotFound for
// 404, with the service's error code.
func (t *S3) do(req *http.Request, payloadSHA256 string) (*http.Response, error) {
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	t.sign(req, payloadSHA256, now())
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	var e struct{ Code, Message string }
	_ = xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w (%s)", ErrNotFound, e.Code)
	}
	return nil, fmt.Errorf("%s: %s %s", resp.Status, e.Code, e.Message)
}

// sign adds AWS Signature Version 4 to req: the date, the payload hash, and
// the Authorization header over every header req carries and its host.
func (t *S3) sign(req *http.Request, payloadSHA256 string, now time.Time) {
	stamp := now.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadSHA256)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	slices.Sort(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method, uriEscape(req.URL.Path, false), canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signed, payloadSHA256,
	}, "\n")
	scope := day + "/" + t.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+t.SecretKey), day)
	for _, part := range []string{t.Region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+t.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	parts := make([]string, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, uriEscape(k, true)+"="+uriEscape(v, true))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, "&")
}

// uriEscape is SigV4's URI encoding: every byte but the unreserved ones
// percent-encoded, and the slash too when encodeSlash.
func uriEscape(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
