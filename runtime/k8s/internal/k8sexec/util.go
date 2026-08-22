package k8sexec

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func jobName(experimentID string) string {
	return boundedName("exp-"+experimentID, experimentID)
}

// groupJobName names the Job compiled from one group of a heterogeneous experiment (see
// domain.JobSpec.Groups). An empty group is an ungrouped job and keeps the plain jobName, so
// nothing about an existing job's identity moves.
func groupJobName(experimentID, group string) string {
	if group == "" {
		return jobName(experimentID)
	}
	return boundedName("exp-"+experimentID+"-"+group, experimentID+"/"+group)
}

// boundedName keeps name inside Kubernetes' 63-character limit. identity is what makes a
// truncated name unique: two identities sharing a long prefix would collide on a plain
// truncation, and the name is the reconciliation identity — one workload would silently adopt
// another's Job. Suffixing a short hash of the full identity keeps truncated names unique and
// deterministic.
func boundedName(name, identity string) string {
	if len(name) <= 63 {
		return name
	}
	h := sha256.Sum256([]byte(identity))
	suffix := fmt.Sprintf("-%x", h[:4]) // 9 chars: '-' + 8 hex
	return name[:63-len(suffix)] + suffix
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
	v := strings.ToLower(strings.Trim(b.String(), "-."))
	if len(v) > 63 {
		// Trim again after truncating: a cut landing on a '-'/'.' would otherwise leave an
		// invalid trailing char (k8s label values must start and end alphanumeric).
		v = strings.Trim(v[:63], "-.")
	}
	return v
}

func buildRestConfig(kubeconfigPath, context string) (*rest.Config, error) {
	if kubeconfigPath == "" && context == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	} else if home := homedir.HomeDir(); home != "" {
		loadingRules.Precedence = append(loadingRules.Precedence, filepath.Join(home, ".kube", "config"))
	}
	overrides := &clientcmd.ConfigOverrides{}
	if context != "" {
		overrides.CurrentContext = context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

func traceparentFromID(id string) string {
	h := sha256.Sum256([]byte(id))
	return fmt.Sprintf("00-%x-%x-01", h[:16], h[16:24])
}
