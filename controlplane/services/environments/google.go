package environments

import (
	"context"
	"fmt"

	"golang.org/x/oauth2/google"
)

// GoogleCredential pushes to Artifact Registry as the Application Default Credentials of the
// process: a service account's on Google's infrastructure, or the file GOOGLE_APPLICATION_CREDENTIALS
// names.
func GoogleCredential(ctx context.Context) (RegistryCredential, error) {
	source, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("environments: Google application default credentials: %w", err)
	}
	return func(context.Context) (string, string, error) {
		token, err := source.Token()
		if err != nil {
			return "", "", fmt.Errorf("environments: Google access token: %w", err)
		}
		return "oauth2accesstoken", token.AccessToken, nil
	}, nil
}
