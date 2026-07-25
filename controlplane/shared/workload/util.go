package workload

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
	name := "exp-" + experimentID
	if len(name) > 63 {
		// Two IDs sharing a >59-char prefix would collide on a plain truncation, and the job name
		// is the reconciliation identity — one experiment would silently adopt another's Job.
		// Suffix a short hash of the full ID so truncated names stay unique and deterministic.
		h := sha256.Sum256([]byte(experimentID))
		suffix := fmt.Sprintf("-%x", h[:4]) // 9 chars: '-' + 8 hex
		name = name[:63-len(suffix)] + suffix
	}
	return name
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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
