package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// SandboxConfig is the provider account sandboxed agents and coordinators run on. Left out, the
// deployment hosts nothing: every request that would host an agent is refused.
type SandboxConfig struct {
	// Name is part of every instance name, hl-agent-<name>-<the platform's name for it>: the
	// control plane lists, creates and destroys the instances that start so and no others, which
	// keeps two deployments on one provider account apart. Changing it, the provider or its
	// account leaves the machines rented before where the platform no longer looks.
	Name string `yaml:"name"`
	// ReconcileIntervalSeconds paces the passes over the provider: each costs it one list call,
	// one log read per running or exited agent, and a call for each instance that has to change.
	ReconcileIntervalSeconds int `yaml:"reconcile_interval_seconds"`
	// CreatePauseMilliseconds is waited between two rentals of one pass. 0 is no pause, and the
	// key must be present either way.
	CreatePauseMilliseconds *int `yaml:"create_pause_milliseconds"`
	// WarmPoolSize is how many warm instances are kept ready to start agents in. Left out, it is
	// 2 on a provider that can warm instances and none on another.
	WarmPoolSize *int                  `yaml:"warm_pool_size"`
	Provider     ProviderConfig        `yaml:"provider"`
	Instance     SandboxInstanceConfig `yaml:"instance"`
}

// ProviderConfig names a vendor and holds its own section, which only its adapter reads.
type ProviderConfig struct {
	Name     string    `yaml:"name"`
	Settings yaml.Node `yaml:"settings"`
}

// SandboxInstanceConfig is what every agent instance runs and how large it is rented, the
// quantities written as a job writes its own ("8", "16Gi").
type SandboxInstanceConfig struct {
	// Command is the whole argv, in place of the image's entrypoint.
	Command []string `yaml:"command"`
	CPU     string   `yaml:"cpu"`
	Memory  string   `yaml:"memory"`
	Storage string   `yaml:"storage"`

	CPUMillis   int64 `yaml:"-"`
	MemoryBytes int64 `yaml:"-"`
	DiskBytes   int64 `yaml:"-"`
}

// CreatePause is SandboxConfig.CreatePauseMilliseconds.
func (s SandboxConfig) CreatePause() time.Duration {
	return time.Duration(*s.CreatePauseMilliseconds) * time.Millisecond
}

// Interval is SandboxConfig.ReconcileIntervalSeconds.
func (s SandboxConfig) Interval() time.Duration {
	return time.Duration(s.ReconcileIntervalSeconds) * time.Second
}

// CloudComputeConfig is one provider account jobs are rented machines on, scheduled as one
// cluster of that name. The cluster is registered as any other is; the control plane runs its
// runtime itself.
type CloudComputeConfig struct {
	ClusterName string `yaml:"cluster_name"`
	// JobImage is what every instance boots. It carries hl, which each job's command runs under.
	JobImage string `yaml:"job_image"`
	// Namespace is part of every instance name, hl-job-<namespace>-...: the cluster lists, counts
	// and destroys the instances of its namespace only. Do not change it while instances exist.
	Namespace string `yaml:"namespace"`
	// The loop intervals. Each pass costs calls to the vendor's API, so these are set against
	// its rate limit.
	ReconcileIntervalSeconds int `yaml:"reconcile_interval_seconds"`
	StatusIntervalSeconds    int `yaml:"status_interval_seconds"`
	// How long a job may wait for a machine to rent, and a rented one to start, before the job
	// is given up here and the machine destroyed.
	OfferWaitSeconds int                   `yaml:"offer_wait_seconds"`
	StartupSeconds   int                   `yaml:"startup_seconds"`
	Provider         ComputeProviderConfig `yaml:"provider"`
	Shapes           []ShapeConfig         `yaml:"shapes"`
}

// ComputeProviderConfig is a ProviderConfig with the account it rents on.
type ComputeProviderConfig struct {
	Name string `yaml:"name"`
	// AccountID is the account's id at the provider. With the name it is the cluster's id.
	AccountID string    `yaml:"account_id"`
	Settings  yaml.Node `yaml:"settings"`
}

// ShapeConfig is one size of machine the account rents for jobs, each of its max_instances
// reported as a node that holds one job.
type ShapeConfig struct {
	Name            string `yaml:"name"`
	AcceleratorType string `yaml:"accelerator_type"`
	Accelerators    int    `yaml:"accelerators"`
	CPU             string `yaml:"cpu"`
	Memory          string `yaml:"memory"`
	Storage         string `yaml:"storage"`
	MaxInstances    int    `yaml:"max_instances"`

	CPUMillis    int64 `yaml:"-"`
	MemoryBytes  int64 `yaml:"-"`
	StorageBytes int64 `yaml:"-"`
}

// quantities parses cpu, memory and storage.
func quantities(cpu, memory, storage string) (cpuMillis, memoryBytes, storageBytes int64, err error) {
	parsed := make([]resource.Quantity, 3)
	for i, q := range []struct{ key, value string }{{"cpu", cpu}, {"memory", memory}, {"storage", storage}} {
		if parsed[i], err = resource.ParseQuantity(q.value); err != nil {
			return 0, 0, 0, fmt.Errorf("%s %q: %w", q.key, q.value, err)
		}
	}
	return parsed[0].MilliValue(), parsed[1].Value(), parsed[2].Value(), nil
}

// buildRented checks the sandbox and cloud compute sections. Whether the vendor can rent what
// they ask for is the provider's to say when it is built at startup.
func (c *Settings) buildRented() error {
	if s := c.Sandbox; s != nil {
		if s.Provider.Name == "" {
			return fmt.Errorf("sandbox provider name is required")
		}
		if s.ReconcileIntervalSeconds <= 0 {
			return fmt.Errorf("sandbox reconcile_interval_seconds is required and positive")
		}
		if s.CreatePauseMilliseconds == nil || *s.CreatePauseMilliseconds < 0 {
			return fmt.Errorf("sandbox create_pause_milliseconds is required: the pause between two rentals of one pass, 0 for none")
		}
		if s.WarmPoolSize != nil && *s.WarmPoolSize < 0 {
			return fmt.Errorf("sandbox warm_pool_size must not be negative")
		}
		i := &s.Instance
		if len(i.Command) == 0 {
			return fmt.Errorf("sandbox instance command is required")
		}
		var err error
		if i.CPUMillis, i.MemoryBytes, i.DiskBytes, err = quantities(i.CPU, i.Memory, i.Storage); err != nil {
			return fmt.Errorf("sandbox instance: %w", err)
		}
		// One pass asks for at most max_new_instances_per_answer machines, created one after
		// another with the pause between them and reported one interval later. A machine not
		// reported within the boot limit spends a restart, and a running one's progress reaches
		// the platform once a pass, so both limits cover two passes.
		pass := time.Duration(c.Agents.MaxNewInstancesPerAnswer)*s.CreatePause() + s.Interval()
		for _, limit := range []struct {
			key     string
			seconds int
		}{{"boot_limit_seconds", c.Agents.BootLimitSeconds}, {"progress_limit_seconds", c.Agents.ProgressLimitSeconds}} {
			if 2*pass > time.Duration(limit.seconds)*time.Second {
				return fmt.Errorf("sandbox: a pass of agents.max_new_instances_per_answer %d x create_pause %s + reconcile_interval %s = %s, and twice that must be at most agents.%s %ds",
					c.Agents.MaxNewInstancesPerAnswer, s.CreatePause(), s.Interval(), pass, limit.key, limit.seconds)
			}
		}
	}
	clusters, accounts := map[string]bool{}, map[string]string{}
	for _, cc := range c.CloudCompute {
		if cc.ClusterName == "" || clusters[cc.ClusterName] {
			return fmt.Errorf("cloud_compute: cluster_name %q is empty or listed twice", cc.ClusterName)
		}
		clusters[cc.ClusterName] = true
		// A provider's key has one name in the environment, so a provider has one account.
		if other, taken := accounts[cc.Provider.Name]; taken {
			return fmt.Errorf("cloud_compute %s: provider %s is %s's already, and a provider has one account", cc.ClusterName, cc.Provider.Name, other)
		}
		accounts[cc.Provider.Name] = cc.ClusterName
		if cc.Provider.Name == "" || cc.Provider.AccountID == "" || cc.JobImage == "" {
			return fmt.Errorf("cloud_compute %s: job_image, provider name and provider account_id are required", cc.ClusterName)
		}
		if cc.OfferWaitSeconds <= 0 || cc.StartupSeconds <= 0 || cc.ReconcileIntervalSeconds <= 0 || cc.StatusIntervalSeconds <= 0 {
			return fmt.Errorf("cloud_compute %s: offer_wait_seconds, startup_seconds, reconcile_interval_seconds and status_interval_seconds must be positive", cc.ClusterName)
		}
		for i := range cc.Shapes {
			shape := &cc.Shapes[i]
			var err error
			if shape.CPUMillis, shape.MemoryBytes, shape.StorageBytes, err = quantities(shape.CPU, shape.Memory, shape.Storage); err != nil {
				return fmt.Errorf("cloud_compute %s shape %q: %w", cc.ClusterName, shape.Name, err)
			}
		}
	}
	return nil
}
