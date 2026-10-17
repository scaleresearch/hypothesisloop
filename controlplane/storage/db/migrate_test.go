package db

import (
	"context"
	"testing"
)

func TestNewPoolRequiresAnExplicitMaxConns(t *testing.T) {
	if _, err := NewPool(context.Background(), Config{DSN: testDSN()}); err == nil {
		t.Fatal("NewPool without MaxConns: got = nil, want an error")
	}
}
