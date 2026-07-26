package store

import (
	"bytes"
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
	"path"
	"sort"
	"strings"
	"time"
)

type Options struct {
	Endpoint, Bucket, Prefix, Region   string
	AccessKey, SecretKey, SessionToken string
	PathStyle, Insecure                bool
}

type Client struct {
	endpoint                    *url.URL
	bucket, prefix, region      string
	accessKey, secretKey, token string
	pathStyle                   bool
	http                        *http.Client
}

func New(o Options) (*Client, error) {
	if o.Endpoint == "" || o.Bucket == "" {
		return nil, errors.New("S3 endpoint and bucket are required")
	}
	if o.AccessKey == "" || o.SecretKey == "" {
		return nil, errors.New("S3 credentials are required (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)")
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", o.Endpoint)
	}
	if u.Scheme != "https" && !(o.Insecure && u.Scheme == "http") {
		return nil, errors.New("S3 endpoint must use HTTPS (use --insecure to explicitly allow HTTP)")
	}
	if o.Region == "" {
		o.Region = "us-east-1"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 32
	transport.ForceAttemptHTTP2 = true
	return &Client{
		endpoint: u, bucket: o.Bucket, prefix: strings.Trim(o.Prefix, "/"),
		region: o.Region, accessKey: o.AccessKey, secretKey: o.SecretKey,
		token: o.SessionToken, pathStyle: o.PathStyle,
		http: &http.Client{Transport: transport, Timeout: 0},
	}, nil
}

func (c *Client) Key(parts ...string) string {
	all := append([]string{}, parts...)
	if c.prefix != "" {
		all = append([]string{c.prefix}, all...)
	}
	return path.Join(all...)
}

func (c *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	req, err := c.request(ctx, http.MethodPut, key, nil, bytes.NewReader(body), shaHex(body))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := c.request(ctx, http.MethodGet, key, nil, nil, emptyHash)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	req, err := c.request(ctx, http.MethodHead, key, nil, nil, emptyHash)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, &RequestError{err}
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, &RequestError{fmt.Errorf("S3 HEAD %s: %s", key, resp.Status)}
	}
	return true, nil
}

type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {c.Key(prefix)}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := c.request(ctx, http.MethodGet, "", q, nil, emptyHash)
		if err != nil {
			return nil, err
		}
		resp, err := c.do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, v := range page.Contents {
			out = append(out, Object{v.Key, v.Size, v.LastModified})
		}
		if !page.IsTruncated {
			break
		}
		token = page.Next
	}
	return out, nil
}

func (c *Client) request(ctx context.Context, method, key string, query url.Values, body io.Reader, payloadHash string) (*http.Request, error) {
	u := *c.endpoint
	if c.pathStyle {
		u.Path = joinURLPath(u.Path, c.bucket, key)
	} else {
		u.Host = c.bucket + "." + u.Host
		u.Path = joinURLPath(u.Path, key)
	}
	u.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if c.token != "" {
		req.Header.Set("X-Amz-Security-Token", c.token)
	}
	c.sign(req, now, payloadHash)
	return req, nil
}

// RequestError は S3 エンドポイントとの通信・応答の失敗を表す。
// 呼び出し側が終了コードの分類に使う。
type RequestError struct{ Err error }

func (e *RequestError) Error() string { return e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

// ErrNotFound は 404 応答を表す。RequestError に wrap されて返る。
var ErrNotFound = errors.New("object not found")

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &RequestError{err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &RequestError{fmt.Errorf("S3 %s %s: %w", req.Method, req.URL.Path, ErrNotFound)}
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, &RequestError{fmt.Errorf("S3 %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))}
}

// Delete はオブジェクトを削除する。存在しないキーへの削除は成功扱い
// （S3 の DeleteObject と同じ冪等性）。
func (c *Client) Delete(ctx context.Context, key string) error {
	req, err := c.request(ctx, http.MethodDelete, key, nil, nil, emptyHash)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) sign(req *http.Request, now time.Time, payloadHash string) {
	headers := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           req.Header.Get("X-Amz-Date"),
	}
	if c.token != "" {
		headers["x-amz-security-token"] = c.token
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonicalHeaders strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&canonicalHeaders, "%s:%s\n", k, strings.TrimSpace(headers[k]))
	}
	signedHeaders := strings.Join(keys, ";")
	canonical := strings.Join([]string{
		req.Method, canonicalURI(req.URL), req.URL.RawQuery,
		canonicalHeaders.String(), signedHeaders, payloadHash,
	}, "\n")
	date := now.Format("20060102")
	scope := date + "/" + c.region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + req.Header.Get("X-Amz-Date") + "\n" + scope + "\n" + shaHex([]byte(canonical))
	kDate := hmacSHA([]byte("AWS4"+c.secretKey), date)
	kRegion := hmacSHA(kDate, c.region)
	kService := hmacSHA(kRegion, "s3")
	signature := hex.EncodeToString(hmacSHA(hmacSHA(kService, "aws4_request"), toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func canonicalURI(u *url.URL) string {
	segments := strings.Split(u.EscapedPath(), "/")
	for i, s := range segments {
		if decoded, err := url.PathUnescape(s); err == nil {
			segments[i] = url.PathEscape(decoded)
		}
	}
	v := strings.Join(segments, "/")
	if v == "" {
		return "/"
	}
	return v
}

func canonicalQuery(q url.Values) string {
	if q == nil {
		return ""
	}
	return q.Encode()
}

func joinURLPath(parts ...string) string {
	var segments []string
	for _, p := range parts {
		for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
			if seg != "" {
				segments = append(segments, seg)
			}
		}
	}
	return "/" + strings.Join(segments, "/")
}

func hmacSHA(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
