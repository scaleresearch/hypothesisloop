package registrycred

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// tokens mints a token of each account it is asked for, numbered, that lives lifetime from the
// clock's now.
type tokens struct {
	clock    *time.Time
	lifetime time.Duration
	minted   []string
	err      error
}

func (f *tokens) mint(_ context.Context, account string) (*oauth2.Token, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.minted = append(f.minted, account)
	return &oauth2.Token{AccessToken: fmt.Sprintf("%s-%d", account, len(f.minted)), Expiry: f.clock.Add(f.lifetime)}, nil
}

func newTestSource(t *testing.T, f *tokens) *Source {
	t.Helper()
	s, err := newSource("europe-docker.pkg.dev/project/environments/environment", "pusher@p.iam.gserviceaccount.com", "puller@p.iam.gserviceaccount.com",
		f.mint, func() time.Time { return *f.clock })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestALoginIsTheTokenOfItsServiceAccountForTheRegistryHost(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := &tokens{clock: &now, lifetime: time.Hour}
	s := newTestSource(t, f)
	push, err := s.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pull, err := s.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if push.Registry != "europe-docker.pkg.dev" || push.Username != "oauth2accesstoken" || push.Password != "pusher@p.iam.gserviceaccount.com-1" || !push.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("push = %+v, want the pusher's token for the registry host", push)
	}
	if pull.Registry != "europe-docker.pkg.dev" || pull.Password != "puller@p.iam.gserviceaccount.com-2" {
		t.Errorf("pull = %+v, want the puller's token for the registry host", pull)
	}
}

func TestEveryLoginHandedOutLivesAtLeastTheMinimumAndIsMintedOnlyWhenItWouldNot(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := &tokens{clock: &now, lifetime: time.Hour}
	s := newTestSource(t, f)
	var passwords []string
	for range 8 {
		login, err := s.Push(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if left := login.ExpiresAt.Sub(now); left < MinLifetime {
			t.Fatalf("at %s a login was handed out with %s left, want at least %s", now, left, MinLifetime)
		}
		passwords = append(passwords, login.Password)
		now = now.Add(10 * time.Minute)
	}
	// Minted at 0, reused at 10, 20 and 30 (35 left), minted again at 40 when 20 would be left.
	want := []string{"-1", "-1", "-1", "-1", "-2", "-2", "-2", "-2"}
	for i, password := range passwords {
		if !strings.HasSuffix(password, want[i]) {
			t.Fatalf("passwords = %v, want the token reused while it lives %s or more and a new one then", passwords, MinLifetime)
		}
	}
}

func TestATokenThatLivesTooShortOrIsNotMintedIsNoLogin(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	short := newTestSource(t, &tokens{clock: &now, lifetime: 10 * time.Minute})
	if _, err := short.Pull(context.Background()); err == nil {
		t.Error("a token that lives 10 minutes was handed out")
	}
	refused := newTestSource(t, &tokens{clock: &now, lifetime: time.Hour, err: errors.New("permission denied")})
	if _, err := refused.Push(context.Background()); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("got = %v, want the minting error", err)
	}
}

func TestASourceNeedsTheRepositoryAndBothAccounts(t *testing.T) {
	f := &tokens{lifetime: time.Hour}
	for _, c := range [][3]string{
		{"no-host-path", "a@p.iam.gserviceaccount.com", "b@p.iam.gserviceaccount.com"},
		{"host/path", "", "b@p.iam.gserviceaccount.com"},
		{"host/path", "a@p.iam.gserviceaccount.com", ""},
	} {
		if _, err := newSource(c[0], c[1], c[2], f.mint, time.Now); err == nil {
			t.Errorf("newSource%q was accepted", c)
		}
	}
}
