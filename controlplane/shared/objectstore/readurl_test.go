package objectstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The signature of AWS's own worked example of a presigned GET ("Authenticating Requests: Using
// Query Parameters"), so a URL the store must verify is not checked only against this signer.
func TestAReadURLIsSignedAsAWSSignsItsExample(t *testing.T) {
	c := &Client{Region: "us-east-1", AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	query := c.presignedQuery("examplebucket.s3.amazonaws.com", "/test.txt", at, 24*time.Hour)
	const want = "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(query, want) {
		t.Fatalf("query = %s, want it signed %s", query, want)
	}
}

// Each listed object carries a URL on the address agents and jobs reach the store by, for that one
// object, its key escaped as the signature reads it.
func TestEachListedObjectCarriesAReadURLOnTheHandedOutEndpoint(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<ListBucketResult><Contents><Key>pe-1/a/job-1/ckpt step 1.pt</Key><Size>3</Size>` +
			`<LastModified>2026-10-09T10:00:00Z</LastModified></Contents></ListBucketResult>`))
	}))
	defer store.Close()
	c, err := New(store.URL, "https://data.example.com", "us-east-1", "hl-data", "AK", "SK")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := c.ListReadable(context.Background(), "pe-1/a/job-1/")
	if err != nil || len(objects) != 1 {
		t.Fatalf("ListReadable = %+v, %v, want one object", objects, err)
	}
	const want = "https://data.example.com/hl-data/pe-1/a/job-1/ckpt%20step%201.pt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AK%2F"
	if got := objects[0].URL; !strings.HasPrefix(got, want) || !strings.Contains(got, "&X-Amz-Expires=43200&") {
		t.Errorf("url = %s, want one under %s valid for 12 hours", got, want)
	}
}
