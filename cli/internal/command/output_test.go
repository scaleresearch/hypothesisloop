package command

import (
	"testing"
)

func TestCompactJSONKeepsKeyOrder(t *testing.T) {
	got, err := CompactJSON([]byte(`{"z": 1, "a": {"y": "", "b": [], "c": null, "d": "<x>"}, "m": 0.10}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"a":{"b":[],"d":"<x>"},"m":0.10}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
