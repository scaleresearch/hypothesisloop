package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrAuthorizationInvalid reports a code GitHub does not exchange for a person's token: made up,
// used before, run out, or issued to another App.
var ErrAuthorizationInvalid = errors.New("github did not take the authorization code")

// UserInstallations is the ids of the App's installations the person may reach who authorized
// the App with code, the one GitHub hands back when the App asks for authorization during an
// install. The person's token it is exchanged for is used for that alone and dropped.
func (c *Client) UserInstallations(ctx context.Context, clientSecret, code string) ([]int64, error) {
	token, err := c.exchange(ctx, clientSecret, code)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for page := 1; ; page++ {
		var out struct {
			TotalCount    int `json:"total_count"`
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		path := fmt.Sprintf("/user/installations?per_page=%d&page=%d", pageSize, page)
		switch err := c.call(ctx, http.MethodGet, path, token, nil, &out); {
		case errors.Is(err, errUnauthorized):
			return nil, ErrAuthorizationInvalid
		case err != nil:
			return nil, err
		}
		for _, one := range out.Installations {
			ids = append(ids, one.ID)
		}
		if len(out.Installations) < pageSize || len(ids) >= out.TotalCount {
			return ids, nil
		}
	}
}

// exchange trades code for the person's token at GitHub's OAuth endpoint, which answers a code it
// refuses with 200 and an error.
func (c *Client) exchange(ctx context.Context, clientSecret, code string) (string, error) {
	form := url.Values{"client_id": {c.clientID}, "client_secret": {clientSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("github: build POST /login/oauth/access_token")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w (github: POST /login/oauth/access_token did not complete: %w)", errFailed, transportError(err))
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil || json.Unmarshal(data, &out) != nil {
		return "", fmt.Errorf("github: POST /login/oauth/access_token answered %d with a body that is not the JSON expected", resp.StatusCode)
	}
	switch {
	case resp.StatusCode >= 500:
		return "", fmt.Errorf("%w (github: POST /login/oauth/access_token answered %d)", errFailed, resp.StatusCode)
	case out.Error == "bad_verification_code":
		return "", ErrAuthorizationInvalid
	case out.Error == "incorrect_client_credentials":
		return "", ErrAppCredentials
	case resp.StatusCode != http.StatusOK || out.Error != "" || out.AccessToken == "":
		return "", fmt.Errorf("github: POST /login/oauth/access_token answered %d without a token: %s", resp.StatusCode, bounded(out.Error))
	}
	return out.AccessToken, nil
}
