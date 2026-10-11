// Package objectstore is the only place the durable-data object store is queried from. It speaks
// S3's ListObjectsV2 over net/http with a hand-rolled SigV4 signer rather than pulling in an SDK:
// the control plane never moves a byte of job data — it lists and measures prefixes, creates one
// key per job and signs read URLs — and a vendored SDK is a large dependency for those. S3 has no
// call that creates a key, so that part is the provider's (KeyIssuer), in a sub-package of its own.
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
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore/minio"
)

const unreachableAfter = 5 * time.Second

// Object is one stored object under a prefix.
type Object struct {
	Key          string    `json:"key"`
	URI          string    `json:"uri" doc:"s3:// address of the object, as a job's own S3 client resolves it."`
	URL          string    `json:"url,omitempty" doc:"A plain GET of it reads the object, with no key, for 12 hours from the listing (curl -o FILE URL): how a prior run's data is read, its URI being outside every data key. List again for a fresh one."`
	SizeBytes    int64     `json:"size_bytes"`
	LastModified time.Time `json:"last_modified"`
}

// Client addresses one bucket on one S3-compatible store.
type Client struct {
	// Endpoint is the address handed to jobs and agents. No request of this client goes there:
	// those go to the address New was given.
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	HTTP            *http.Client
	address         string
	keys            KeyIssuer
}

// New returns a Client that reaches the store at address and hands endpoint out to jobs and
// agents, which may reach the store by another address than this process does, and creates keys
// the way provider's store does. Every field is required: a half-configured store has no usable
// behaviour, so it fails at construction rather than at the first listing.
func New(provider, address, endpoint, region, bucket, accessKeyID, secretAccessKey string) (*Client, error) {
	if address == "" || endpoint == "" || region == "" || bucket == "" || accessKeyID == "" || secretAccessKey == "" {
		return nil, fmt.Errorf("objectstore: address, endpoint, region, bucket, access_key_id and secret_access_key are all required")
	}
	for _, stated := range []string{address, endpoint} {
		if _, err := url.Parse(stated); err != nil {
			return nil, fmt.Errorf("objectstore: parse %q: %w", stated, err)
		}
	}
	// A store that is down is known within seconds, well inside the time a submit may take.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: unreachableAfter}).DialContext
	transport.ResponseHeaderTimeout = unreachableAfter
	c := &Client{
		Endpoint: strings.TrimSuffix(endpoint, "/"), Region: region, Bucket: bucket,
		AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey,
		HTTP:    &http.Client{Timeout: 20 * time.Second, Transport: transport},
		address: strings.TrimSuffix(address, "/"),
	}
	switch provider {
	case config.DataStoreProviderMinIO:
		c.keys = minio.New(c, secretAccessKey)
	default:
		return nil, fmt.Errorf("objectstore: provider %q is not one this build knows (%s)", provider, config.DataStoreProviderMinIO)
	}
	return c, nil
}

// Unavailable is a call the store did not answer: it could not be reached, did not answer in
// time, or answered a 5xx. A handler answers it 503 with its reason and never a bare 500.
type Unavailable struct{ Err error }

func (e *Unavailable) Error() string { return "objectstore: unavailable: " + e.Err.Error() }
func (e *Unavailable) Unwrap() error { return e.Err }

// Unavailable is the reason and the sentence a caller is answered with.
func (e *Unavailable) Unavailable() (reason, message string) {
	return "object_store_unavailable", "the platform's object store is not answering, so nothing that measures or keys durable data can be answered: try again shortly"
}

// Do sends req, made by SignedRequest. A store that is not answering is an *Unavailable.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Unavailable{Err: err}
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &Unavailable{Err: fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))}
	}
	return resp, nil
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
	req, err := c.SignedRequest(ctx, http.MethodGet, "/"+c.Bucket, query, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.Do(req)
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
		objects = append(objects, Object{Key: item.Key, URI: c.URI(item.Key), SizeBytes: item.Size, LastModified: item.LastModified})
	}
	next := ""
	if result.IsTruncated {
		next = result.NextContinuationToken
	}
	return objects, next, nil
}

const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// SignedRequest is one request to path on the store as the control plane reaches it, signed with
// SigV4 in the s3 scope as the configured key. A provider's admin API shares the scope.
func (c *Client) SignedRequest(ctx context.Context, method, path string, query map[string]string, body []byte) (*http.Request, error) {
	canonicalQuery := encodeCanonicalQuery(query)
	url := c.address + path
	if canonicalQuery != "" {
		url += "?" + canonicalQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("objectstore: build request: %w", err)
	}
	payloadHash := emptyPayloadSHA256
	if len(body) > 0 {
		payloadHash = sha256Hex(string(body))
	}
	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	canonicalHeaders := "host:" + req.URL.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"

	canonicalRequest := strings.Join([]string{
		method, path, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	scope := dateStamp + "/" + c.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(c.signingKey(dateStamp), stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKeyID, scope, signedHeaders, signature))
	return req, nil
}

func (c *Client) signingKey(dateStamp string) []byte {
	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), dateStamp)
	key = hmacSHA256(key, c.Region)
	key = hmacSHA256(key, "s3")
	return hmacSHA256(key, "aws4_request")
}

// ReadURLLifetime is how long a URL ListReadable hands out opens its object.
const ReadURLLifetime = 12 * time.Hour

// ListReadable is List with each object's URL: a plain GET of it, from anywhere that reaches
// Endpoint, reads that one object until ReadURLLifetime has passed. It is how a run reads a prior
// run's data: no data key names another run, so whoever the API lets list a prefix reads it this
// way, decided at each request and never written into a key.
func (c *Client) ListReadable(ctx context.Context, prefix string) ([]Object, error) {
	objects, err := c.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("objectstore: parse endpoint %q: %w", c.Endpoint, err)
	}
	now := time.Now()
	for i := range objects {
		path := endpoint.Path + "/" + c.Bucket + "/" + escapePath(objects[i].Key)
		objects[i].URL = endpoint.Scheme + "://" + endpoint.Host + path + "?" + c.presignedQuery(endpoint.Host, path, now, ReadURLLifetime)
	}
	return objects, nil
}

// presignedQuery is the SigV4 query that signs a GET of path on host from now for lifetime.
func (c *Client) presignedQuery(host, path string, now time.Time, lifetime time.Duration) string {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + c.Region + "/s3/aws4_request"
	query := encodeCanonicalQuery(map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    c.AccessKeyID + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(lifetime.Seconds())),
		"X-Amz-SignedHeaders": "host",
	})
	canonicalRequest := strings.Join([]string{
		http.MethodGet, path, query, "host:" + host + "\n", "host", "UNSIGNED-PAYLOAD",
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest),
	}, "\n")
	return query + "&X-Amz-Signature=" + hex.EncodeToString(hmacSHA256(c.signingKey(dateStamp), stringToSign))
}

// escapePath escapes each segment of an object key as SigV4 and a URL both read it.
func escapePath(key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = uriEscape(s)
	}
	return strings.Join(segments, "/")
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
