// Package objectstore is the only place the durable-data object store is queried from. It speaks
// S3's ListObjectsV2 over net/http with a hand-rolled SigV4 signer rather than pulling in an SDK:
// the control plane never moves a byte of job data — it only lists and measures prefixes — and a
// vendored AWS SDK is a large dependency for two request shapes.
package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Object is one stored object under a prefix.
type Object struct {
	Key          string    `json:"key"`
	SizeBytes    int64     `json:"size_bytes"`
	LastModified time.Time `json:"last_modified"`
}

// Client addresses one bucket on one S3-compatible endpoint.
type Client struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	HTTP            *http.Client
}

// New returns a Client for the given endpoint/bucket. Every field is required: a half-configured
// store has no usable behaviour, so it fails at construction rather than at the first listing.
func New(endpoint, region, bucket, accessKeyID, secretAccessKey string) (*Client, error) {
	if endpoint == "" || region == "" || bucket == "" || accessKeyID == "" || secretAccessKey == "" {
		return nil, fmt.Errorf("objectstore: endpoint, region, bucket, access_key_id and secret_access_key are all required")
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("objectstore: parse endpoint %q: %w", endpoint, err)
	}
	return &Client{
		Endpoint: strings.TrimSuffix(endpoint, "/"), Region: region, Bucket: bucket,
		AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey,
		HTTP: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// URI renders an s3:// address for a prefix in this client's bucket — what a job is handed and
// what its own S3 client resolves.
func (c *Client) URI(prefix string) string {
	return "s3://" + c.Bucket + "/" + prefix
}

// List returns every object under prefix, following continuation tokens to completion. A prefix
// nothing was ever written to lists as an empty slice, never an error: "this job saved nothing"
// is an ordinary answer, not a failure.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	out := []Object{}
	token := ""
	for {
		page, next, err := c.listPage(ctx, prefix, token)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

// TotalBytes sums every object under prefix. Same live read as List — there is no stored counter
// to drift from the bytes themselves.
func (c *Client) TotalBytes(ctx context.Context, prefix string) (int64, error) {
	objects, err := c.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, o := range objects {
		total += o.SizeBytes
	}
	return total, nil
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

func (c *Client) listPage(ctx context.Context, prefix, token string) ([]Object, string, error) {
	query := map[string]string{"list-type": "2", "prefix": prefix, "max-keys": "1000"}
	if token != "" {
		query["continuation-token"] = token
	}
	req, err := c.signedRequest(ctx, http.MethodGet, "s3", "/"+c.Bucket, query, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("objectstore: list %q: %w", prefix, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("objectstore: list %q: read body: %w", prefix, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("objectstore: list %q: status %d: %s", prefix, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result listBucketResult
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, "", fmt.Errorf("objectstore: list %q: parse response: %w", prefix, err)
	}
	objects := make([]Object, 0, len(result.Contents))
	for _, item := range result.Contents {
		objects = append(objects, Object{Key: item.Key, SizeBytes: item.Size, LastModified: item.LastModified})
	}
	next := ""
	if result.IsTruncated {
		next = result.NextContinuationToken
	}
	return objects, next, nil
}

const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signedRequest signs one request with SigV4. service selects the signing scope: "s3" for bucket
// operations, "sts" for AssumeRole — the two share an endpoint but not a signing scope, and
// signing an STS call as s3 fails with a signature mismatch that says nothing about why.
func (c *Client) signedRequest(ctx context.Context, method, service, path string, query map[string]string, body []byte) (*http.Request, error) {
	canonicalQuery := encodeCanonicalQuery(query)
	url := c.Endpoint + path
	if canonicalQuery != "" {
		url += "?" + canonicalQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("objectstore: build request: %w", err)
	}
	payloadHash := emptyPayloadSHA256
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := ""
	if len(body) > 0 {
		payloadHash = sha256Hex(string(body))
		req.Header.Set("Content-Type", formContentType)
		signedHeaders = "content-type;host;x-amz-content-sha256;x-amz-date"
		canonicalHeaders = "content-type:" + formContentType + "\n"
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	canonicalHeaders += "host:" + req.URL.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"

	canonicalRequest := strings.Join([]string{
		method, path, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	scope := dateStamp + "/" + c.Region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), dateStamp)
	key = hmacSHA256(key, c.Region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKeyID, scope, signedHeaders, signature))
	return req, nil
}

// encodeCanonicalQuery renders the query the way SigV4 requires: keys sorted, every character
// outside the unreserved set percent-encoded — including "/", which url.Values.Encode leaves
// alone and which appears in every prefix we list.
func encodeCanonicalQuery(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEscape(k)+"="+uriEscape(query[k]))
	}
	return strings.Join(parts, "&")
}

func uriEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
