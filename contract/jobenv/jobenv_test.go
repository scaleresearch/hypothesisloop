package jobenv

import "testing"

func TestTheSocketInterfacesArePlatformEnvAndTheRestOfNCCLIsNot(t *testing.T) {
	for _, name := range []string{NCCLSocketInterface, GlooSocketInterface, TorchMasterPort, MasterPort} {
		if !Reserved(name) {
			t.Errorf("%s is not reserved", name)
		}
	}
	for _, name := range []string{"NCCL_DEBUG", "NCCL_IB_DISABLE", "GLOO_DEVICE_TRANSPORT"} {
		if Reserved(name) {
			t.Errorf("%s is reserved, and the platform never sets it", name)
		}
	}
}
