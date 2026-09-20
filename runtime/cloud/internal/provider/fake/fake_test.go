package fake

import (
	"context"
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// A vendor does not keep names unique, so the fake does not either: what is built on the
// contract has to be shown two instances of one name.
func TestTheSameNameCanBeRentedTwice(t *testing.T) {
	p := New()
	ctx := context.Background()
	for range 2 {
		if _, err := p.Create(ctx, provider.Spec{Name: "hl-job-a"}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := p.List(ctx, "hl-job-")
	if err != nil || len(listed) != 2 || listed[0].ID == listed[1].ID {
		t.Fatalf("listed %+v, %v", listed, err)
	}
	if ids := p.IDs("hl-job-a"); !reflect.DeepEqual(ids, []string{"fake-0001", "fake-0002"}) {
		t.Fatalf("ids = %v", ids)
	}
	if _, err := p.Create(ctx, provider.Spec{Name: "Not_A_Name"}); err == nil {
		t.Fatal("a name outside the contract's rule was rented")
	}
}
