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
		name = name[:63]
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
		v = v[:63]
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
