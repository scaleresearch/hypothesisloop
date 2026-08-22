package objectstore

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const formContentType = "application/x-www-form-urlencoded"

// STSMinDuration and STSMaxDuration are the window AssumeRole accepts. Both ends are the store's,
// not ours: below the minimum the call is rejected outright, and above the maximum a session is
// silently clamped, which would hand a long job credentials that expire before it finishes
// without anything saying so.
const (
	STSMinDuration = 15 * time.Minute
	STSMaxDuration = 12 * time.Hour
)

// SessionCredentials are one job's scoped credentials: usable only for what its session policy
// allows, and only until Expiration.
type SessionCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

type assumeRoleResponse struct {
	XMLName xml.Name `xml:"AssumeRoleResponse"`
	Result  struct {
		Credentials struct {
			AccessKeyID     string    `xml:"AccessKeyId"`
			SecretAccessKey string    `xml:"SecretAccessKey"`
			SessionToken    string    `xml:"SessionToken"`
			Expiration      time.Time `xml:"Expiration"`
		} `xml:"Credentials"`
	} `xml:"AssumeRoleResult"`
}

// AssumeRole mints credentials restricted to policy. The platform stays out of the data path:
// this hands a job a key it can use directly against the store, it does not proxy the bytes.
//
// The store's own root credentials never leave the control plane after this — a job holds only
// the session, so the write-scoping in policy is enforced by the store on every request rather
// than trusted to the job.
func (c *Client) AssumeRole(ctx context.Context, policy Policy, duration time.Duration) (SessionCredentials, error) {
	if duration < STSMinDuration || duration > STSMaxDuration {
		return SessionCredentials{}, fmt.Errorf("objectstore: session duration %s is outside the %s–%s the store accepts", duration, STSMinDuration, STSMaxDuration)
	}
	document, err := policy.JSON()
	if err != nil {
		return SessionCredentials{}, err
	}
	form := url.Values{
		"Action":          []string{"AssumeRole"},
		"Version":         []string{"2011-06-15"},
		"DurationSeconds": []string{strconv.Itoa(int(duration.Seconds()))},
		"Policy":          []string{document},
	}
	req, err := c.signedRequest(ctx, http.MethodPost, "sts", "/", nil, []byte(form.Encode()))
	if err != nil {
		return SessionCredentials{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return SessionCredentials{}, fmt.Errorf("objectstore: assume role: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return SessionCredentials{}, fmt.Errorf("objectstore: assume role: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return SessionCredentials{}, fmt.Errorf("objectstore: assume role: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed assumeRoleResponse
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return SessionCredentials{}, fmt.Errorf("objectstore: assume role: parse response: %w", err)
	}
	creds := SessionCredentials{
		AccessKeyID:     parsed.Result.Credentials.AccessKeyID,
		SecretAccessKey: parsed.Result.Credentials.SecretAccessKey,
		SessionToken:    parsed.Result.Credentials.SessionToken,
		Expiration:      parsed.Result.Credentials.Expiration,
	}
	// An STS response that parses but carries no session token is the shape a store returns when
	// it does not implement AssumeRole at all. Handing that on would mean shipping the root key
	// to jobs under the name of a scoped one.
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" {
		return SessionCredentials{}, fmt.Errorf("objectstore: assume role returned no session credentials — the endpoint does not implement STS AssumeRole")
	}
	return creds, nil
}
