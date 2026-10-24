package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// The environment seeds the credential once per auth.json, a restart keeps its renewals, and of
// the callers that find it due at once one renews it and the others read the renewed pair.
func TestTheChatGPTCredentialIsSeededOnceAndRenewedOnce(t *testing.T) {
	pool := eventsTestDB(t)
	s := NewStore(pool, 3)
	ctx := context.Background()
	if _, err := pool.pool.Exec(ctx, `DELETE FROM chatgpt_credential`); err != nil {
		t.Fatal(err)
	}
	never := func(domain.ChatGPTCredential) bool { return false }
	if _, err := s.FreshChatGPTCredential(ctx, never, nil); err == nil {
		t.Fatal("no credential stored: got one, want an error")
	}
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	seed := domain.ChatGPTCredential{AccessToken: "a-1", RefreshToken: "r-1", AccountID: "acc", ExpiresAt: at}
	if err := s.SeedChatGPTCredential(ctx, seed, "digest-1"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	renewals := 0
	due := func(c domain.ChatGPTCredential) bool { return c.RefreshToken == "r-1" }
	renew := func(_ context.Context, c domain.ChatGPTCredential) (domain.ChatGPTCredential, error) {
		mu.Lock()
		renewals++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return domain.ChatGPTCredential{AccessToken: "a-2", RefreshToken: "r-2", AccountID: c.AccountID, ExpiresAt: at.Add(time.Hour)}, nil
	}
	var wg sync.WaitGroup
	got := make([]domain.ChatGPTCredential, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.FreshChatGPTCredential(ctx, due, renew)
			if err != nil {
				t.Error(err)
			}
			got[i] = c
		}()
	}
	wg.Wait()
	for _, c := range got {
		if c.RefreshToken != "r-2" || c.AccessToken != "a-2" || !c.ExpiresAt.Equal(at.Add(time.Hour)) {
			t.Errorf("a caller read %+v, want the renewed pair", c)
		}
	}
	if renewals != 1 {
		t.Errorf("renewed %d times, want once", renewals)
	}
	if err := s.SeedChatGPTCredential(ctx, seed, "digest-1"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.FreshChatGPTCredential(ctx, never, nil); c.RefreshToken != "r-2" {
		t.Errorf("the same auth.json seeded again: %+v, want the renewal kept", c)
	}
	if _, err := s.FreshChatGPTCredential(ctx, func(domain.ChatGPTCredential) bool { return true },
		func(context.Context, domain.ChatGPTCredential) (domain.ChatGPTCredential, error) {
			return seed, errors.New("refused")
		}); err == nil {
		t.Error("a refused renewal: got a credential, want the error")
	}
	if c, _ := s.FreshChatGPTCredential(ctx, never, nil); c.RefreshToken != "r-2" {
		t.Errorf("after a refused renewal: %+v, want the pair unchanged", c)
	}
	if err := s.SeedChatGPTCredential(ctx, domain.ChatGPTCredential{AccessToken: "a-9", RefreshToken: "r-9", AccountID: "acc", ExpiresAt: at}, "digest-2"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.FreshChatGPTCredential(ctx, never, nil); c.RefreshToken != "r-9" {
		t.Errorf("another auth.json seeded: %+v, want it stored", c)
	}
	if err := s.SeedChatGPTCredential(ctx, seed, "digest-1"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.FreshChatGPTCredential(ctx, never, nil); c.RefreshToken != "r-9" {
		t.Errorf("an older auth.json seeded again, as by a pod still holding it: %+v, want the newer one kept", c)
	}
}

// The token endpoint rotates the refresh token as it answers: a caller that goes away after the
// answer and before the commit still leaves the renewed pair stored.
func TestARenewalTheCallerLeftIsStillStored(t *testing.T) {
	pool := eventsTestDB(t)
	s := NewStore(pool, 3)
	if _, err := pool.pool.Exec(context.Background(), `DELETE FROM chatgpt_credential`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := s.SeedChatGPTCredential(context.Background(), domain.ChatGPTCredential{AccessToken: "a-1", RefreshToken: "r-1", AccountID: "acc", ExpiresAt: at}, "digest-1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got, err := s.FreshChatGPTCredential(ctx, func(c domain.ChatGPTCredential) bool { return c.RefreshToken == "r-1" },
		func(_ context.Context, c domain.ChatGPTCredential) (domain.ChatGPTCredential, error) {
			cancel()
			return domain.ChatGPTCredential{AccessToken: "a-2", RefreshToken: "r-2", AccountID: c.AccountID, ExpiresAt: at.Add(time.Hour)}, nil
		})
	if err != nil || got.RefreshToken != "r-2" {
		t.Fatalf("a caller gone before the commit: got %+v, %v, want the renewed pair", got, err)
	}
	never := func(domain.ChatGPTCredential) bool { return false }
	if c, err := s.FreshChatGPTCredential(context.Background(), never, nil); err != nil || c.RefreshToken != "r-2" || c.AccessToken != "a-2" {
		t.Fatalf("stored: %+v, %v, want the renewed pair", c, err)
	}
}

// An access token the ChatGPT backend refused makes the credential due, and only that one: a pair
// renewed since is left as it is.
func TestARefusedAccessTokenMakesTheCredentialDue(t *testing.T) {
	pool := eventsTestDB(t)
	s := NewStore(pool, 3)
	ctx := context.Background()
	if _, err := pool.pool.Exec(ctx, `DELETE FROM chatgpt_credential`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := s.SeedChatGPTCredential(ctx, domain.ChatGPTCredential{AccessToken: "a-1", RefreshToken: "r-1", AccountID: "acc", ExpiresAt: at}, "digest-1"); err != nil {
		t.Fatal(err)
	}
	expired := func(c domain.ChatGPTCredential) bool { return !c.ExpiresAt.After(time.Now()) }
	read := func() domain.ChatGPTCredential {
		t.Helper()
		c, err := s.FreshChatGPTCredential(ctx, func(domain.ChatGPTCredential) bool { return false }, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := s.ExpireChatGPTCredential(ctx, "a-other"); err != nil {
		t.Fatal(err)
	}
	if expired(read()) {
		t.Fatal("another access token refused: the credential is due, want it left")
	}
	if err := s.ExpireChatGPTCredential(ctx, "a-1"); err != nil {
		t.Fatal(err)
	}
	if !expired(read()) {
		t.Fatal("its access token refused: the credential is not due")
	}
}
