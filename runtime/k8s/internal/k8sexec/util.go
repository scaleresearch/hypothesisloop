package k8sexec

import (
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/boundedname"
)

// dnsLabelLength bounds a Job name (Kubernetes copies it into the job-name label), a Service name,
// a label value and a pod hostname alike.
const dnsLabelLength = 63

// jobName names the experiment's headless Service, and its Job when that Job is the only one.
func jobName(experimentID string) string {
	return groupJobName(experimentID, "", 0)
}

// groupJobName names the Job compiled from one group of a heterogeneous experiment (see
// domain.JobSpec.Groups). An empty group is an ungrouped job, named by its experiment alone.
//
// ranks is how many pods of an Indexed Job the name is for, 0 for a Job that is not Indexed.
// Kubernetes makes the hostname of each such pod from the Job's name, a dash and the pod's index,
// and refuses a Job whose last hostname is longer than a DNS label, so the name leaves that room.
func groupJobName(experimentID, group string, ranks int) string {
	readable, identity, limit := "exp-"+experimentID, experimentID, dnsLabelLength
	if group != "" {
		readable, identity = readable+"-"+group, identity+"/"+group
	}
	if ranks > 0 {
		limit -= len(fmt.Sprintf("-%d", ranks-1))
	}
	return boundedname.Name(readable, identity, limit)
}

func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return boundedname.Name(strings.ToLower(strings.Trim(b.String(), "-.")), s, dnsLabelLength)
}

// client-go's own default (QPS 5, Burst 10) is sized for a single controller managing a handful
// of objects, not for a control plane that lists pods per job on every poll across however many
// jobs are in flight at once. Unconfigured, that default is what a busy cluster hits first: every
// caller of this client shares the same conservative budget, so load from one job's polling starves
// every other request behind it, and the failure shows up as "rate: Wait(n=1) would exceed context
// deadline" wherever the next caller happened to be — a job stuck reporting SUBMITTED, a Service
// repair that never lands, a log tail that never resolves — none of which name the actual cause.
// This process talks to nothing but its own cluster's API server, so the limit here IS the ceiling
// on how fast this whole component can act; there is no other consumer to protect from it.
const (
	k8sClientQPS   = 200
	k8sClientBurst = 400
)

// k8sCallTimeout bounds every call to the API server, a log stream read whole included.
const k8sCallTimeout = 30 * time.Second

func buildRestConfig() (*rest.Config, error) {
	cfg, err := loadRestConfig()
	if err != nil {
		return nil, err
	}
	cfg.QPS = k8sClientQPS
	cfg.Burst = k8sClientBurst
	cfg.Timeout = k8sCallTimeout
	return cfg, nil
}

// loadRestConfig reads the kubeconfig file KUBECONFIG names when it is set, and the pod's own
// service account otherwise.
func loadRestConfig() (*rest.Config, error) {
	if path := os.Getenv("KUBECONFIG"); path != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s (KUBECONFIG): %w", path, err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("not running in a pod and KUBECONFIG is not set: %w", err)
	}
	return cfg, nil
}
