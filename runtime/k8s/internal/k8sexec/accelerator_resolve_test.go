package k8sexec

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func deviceClassFor(name, driver string) *unstructured.Unstructured {
	class := tenstorrentDeviceClass()
	class.SetName(name)
	class.Object["spec"] = map[string]interface{}{"selectors": []interface{}{map[string]interface{}{
		"cel": map[string]interface{}{"expression": `device.driver == "` + driver + `"`},
	}}}
	return class
}

func TestADRATypeResolvesToTheOneDeviceClassOfItsDriver(t *testing.T) {
	foreign := deviceClassFor("foreign", "x")
	foreign.Object["spec"] = map[string]interface{}{"selectors": []interface{}{map[string]interface{}{
		"cel": map[string]interface{}{"expression": `device.attributes["foreign.example"].model == "x"`},
	}}}
	c := newDRATestClient(t,
		foreign,
		deviceClassFor("other", "other.example.com"),
		tenstorrentDeviceClass(),
		tenstorrentResourceSlice("slice-a", "worker-a", "chip-0"),
	)

	placement, found, err := c.resolveDRAPlacement(context.Background(), "tenstorrent.com/chipArch=blackhole")
	if err != nil || !found || placement.DeviceClassName != "tenstorrent" {
		t.Fatalf("placement = %+v, %v, %v, want the tenstorrent class beside classes of other tenants", placement, found, err)
	}
	if _, found, err := c.resolveDRAPlacement(context.Background(), "tenstorrent.com/chipArch=wormhole"); err != nil || found {
		t.Fatalf("a type no device publishes: found %v, %v", found, err)
	}
}

// A claim names one DeviceClass. Two for one driver resolved to whichever the API listed first.
func TestTwoDeviceClassesForOneDriverAreAnAmbiguityAndNotAChoice(t *testing.T) {
	c := newDRATestClient(t,
		tenstorrentDeviceClass(),
		deviceClassFor("tenstorrent-b", "tenstorrent.com"),
		tenstorrentResourceSlice("slice-a", "worker-a", "chip-0"),
	)

	_, found, err := c.resolveDRAPlacement(context.Background(), "tenstorrent.com/chipArch=blackhole")
	if err == nil || found || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "[tenstorrent tenstorrent-b]") {
		t.Fatalf("found %v, err = %v, want an ambiguity naming both classes", found, err)
	}
}

func TestAResourceSliceThatCannotBeReadFailsTheResolutionAsItFailsTheCapacityRead(t *testing.T) {
	for name, spoil := range map[string]func(spec map[string]interface{}){
		"no driver":          func(spec map[string]interface{}) { delete(spec, "driver") },
		"driver not text":    func(spec map[string]interface{}) { spec["driver"] = int64(7) },
		"no devices":         func(spec map[string]interface{}) { delete(spec, "devices") },
		"devices not a list": func(spec map[string]interface{}) { spec["devices"] = "chip-0" },
		"a malformed device": func(spec map[string]interface{}) { spec["devices"] = []interface{}{"chip-0"} },
	} {
		t.Run(name, func(t *testing.T) {
			broken := tenstorrentResourceSlice("slice-broken", "worker-a", "chip-9")
			broken.Object["spec"].(map[string]interface{})["devices"] = []interface{}{map[string]interface{}{"name": "chip-9"}}
			spoil(broken.Object["spec"].(map[string]interface{}))
			c := newDRATestClient(t, tenstorrentDeviceClass(), broken, tenstorrentResourceSlice("slice-z", "worker-a", "chip-0"))

			_, found, err := c.resolveDRAPlacement(context.Background(), "tenstorrent.com/chipArch=blackhole")
			if err == nil || found || !strings.Contains(err.Error(), "slice-broken") {
				t.Fatalf("found %v, err = %v, want an error naming the slice", found, err)
			}
		})
	}
}
