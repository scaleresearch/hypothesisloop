package k8sexec

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

const privateImage = "europe-west1-docker.pkg.dev/p/hl-environments/environment@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func pullTestClient(t *testing.T, objects ...runtime.Object) *JobWorkloadClient {
	listKinds := map[schema.GroupVersionResource]string{resourceClaimTemplateGVR: "ResourceClaimTemplateList"}
	return &JobWorkloadClient{
		apiURL: "http://api",
		kube:   chartKube(t, objects...),
		dyn:    chartDynamic(t, listKinds),
	}
}

func pullTestExperiment(id, image string, pull *clusterapi.RegistryLogin) *clusterapi.DesiredJob {
	return &clusterapi.DesiredJob{
		Image: image, ImagePull: pull, Launch: "exec \"$@\"",
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git", ID: id, AgentID: "agent",
		EstimatedDurationHours: 1, CapacityTier: clusterapi.CapacityGuaranteed,
		Job: clusterapi.JobSpec{Command: []string{"python", "train.py"}, CPU: "1", Memory: "1Gi", Storage: "1Gi",
			MaxRetries: intPtr(1), TerminationGracePeriodSeconds: int64Ptr(5)},
	}
}

func pullLogin(password string) *clusterapi.RegistryLogin {
	return &clusterapi.RegistryLogin{Registry: "europe-west1-docker.pkg.dev", Username: "oauth2accesstoken", Password: password}
}

// secretLogin is the login a pull Secret holds for registry.
func secretLogin(t *testing.T, secret *corev1.Secret, registry string) (username, password, auth string) {
	t.Helper()
	var config struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &config); err != nil {
		t.Fatalf("pull secret %s is no docker config: %v", secret.Name, err)
	}
	entry, ok := config.Auths[registry]
	if !ok || len(config.Auths) != 1 {
		t.Fatalf("pull secret %s logs in to %v, want %s alone", secret.Name, config.Auths, registry)
	}
	return entry.Username, entry.Password, entry.Auth
}

// A job whose image needs a login pulls it with the login desired state carries, kept
// in a Secret of its own from before its pods exist until its workload is deleted.
func TestAJobWithAPullLoginPullsWithASecretThatGoesWithIt(t *testing.T) {
	ctx := context.Background()
	c := pullTestClient(t)
	exp := pullTestExperiment("exp-env", privateImage, pullLogin("pull-token"))
	if err := c.CreateWorkload(ctx, exp); err != nil {
		t.Fatal(err)
	}

	secret, err := c.kube.CoreV1().Secrets(HypothesisLoopNamespace).Get(ctx, pullSecretName(exp.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no pull secret: %v", err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson || secret.Labels[workloadkeys.ManagedBy] != workloadkeys.ManagedByValue || secret.Labels[workloadkeys.ExperimentID] != exp.ID {
		t.Fatalf("pull secret = type %s, labels %v", secret.Type, secret.Labels)
	}
	if username, password, auth := secretLogin(t, secret, "europe-west1-docker.pkg.dev"); username != "oauth2accesstoken" || password != "pull-token" || auth != "b2F1dGgyYWNjZXNzdG9rZW46cHVsbC10b2tlbg==" {
		t.Fatalf("pull secret holds %s / %s / %s, want the desired state's login", username, password, auth)
	}
	jobs, err := c.managedJobsFor(ctx, exp.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v, %v", jobs, err)
	}
	if got := jobs[0].Spec.Template.Spec.ImagePullSecrets; len(got) != 1 || got[0].Name != secret.Name {
		t.Fatalf("the job's pods pull with %v, want %s", got, secret.Name)
	}
	matches, err := c.WorkloadMatchesDesired(ctx, exp)
	if err != nil || !matches {
		t.Fatalf("the created workload matches desired state: %v, %v", matches, err)
	}

	if err := c.DeleteWorkload(ctx, exp.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.kube.CoreV1().Secrets(HypothesisLoopNamespace).Get(ctx, pullSecretName(exp.ID), metav1.GetOptions{}); err == nil {
		t.Fatal("the pull secret outlived its job")
	}
}

// A pull secret left by an earlier generation is written over with the login handed out now, and
// one whose job is gone is an orphan the reconcile pass deletes.
func TestALeftoverPullSecretIsReplacedOrReportedAsAnOrphan(t *testing.T) {
	ctx := context.Background()
	leftover := func(id string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: pullSecretName(id), Namespace: HypothesisLoopNamespace,
				Labels: map[string]string{workloadkeys.ManagedBy: workloadkeys.ManagedByValue, workloadkeys.ExperimentID: id}},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"europe-west1-docker.pkg.dev":{"password":"expired"}}}`)},
		}
	}
	c := pullTestClient(t, leftover("exp-env"), leftover("exp-gone"))

	auxiliary, err := c.ListManagedAuxiliaryWorkloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(slices.Sorted(slices.Values(auxiliary)), ",") != "exp-env,exp-gone" {
		t.Fatalf("auxiliary = %v, want both experiments the secrets name", auxiliary)
	}

	if err := c.CreateWorkload(ctx, pullTestExperiment("exp-env", privateImage, pullLogin("fresh-token"))); err != nil {
		t.Fatal(err)
	}
	secret, err := c.kube.CoreV1().Secrets(HypothesisLoopNamespace).Get(ctx, pullSecretName("exp-env"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, password, _ := secretLogin(t, secret, "europe-west1-docker.pkg.dev"); password != "fresh-token" {
		t.Fatalf("the pull secret holds %q, want the login handed out now", password)
	}
}

// A job whose desired state carries no pull login pulls with nothing.
func TestAJobWithNoPullLoginHasNoPullSecret(t *testing.T) {
	ctx := context.Background()
	c := pullTestClient(t)
	plain := pullTestExperiment("exp-plain", "registry.test/public@sha256:aa", nil)
	if err := c.CreateWorkload(ctx, plain); err != nil {
		t.Fatal(err)
	}
	jobs, err := c.managedJobsFor(ctx, plain.ID)
	if err != nil || len(jobs) != 1 || len(jobs[0].Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Fatalf("a job with no login = %v, %v, want one job that names no pull secret", jobs, err)
	}
	secrets, err := c.kube.CoreV1().Secrets(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 0 {
		t.Fatalf("secrets = %v, %v, want none", secrets, err)
	}
}

// Each pass rewrites the pull Secret of a live job with the current login when it holds another,
// and leaves it alone when it holds that one already.
func TestALiveJobsPullSecretFollowsTheCurrentLogin(t *testing.T) {
	ctx := context.Background()
	c := pullTestClient(t)
	exp := pullTestExperiment("exp-env", privateImage, pullLogin("first-token"))
	if err := c.CreateWorkload(ctx, exp); err != nil {
		t.Fatal(err)
	}
	kube := c.kube.(*fake.Clientset)
	writes := func() int {
		n := 0
		for _, action := range kube.Actions() {
			if action.GetResource().Resource == "secrets" && (action.GetVerb() == "create" || action.GetVerb() == "update") {
				n++
			}
		}
		return n
	}

	if err := c.RefreshPull(ctx, exp); err != nil {
		t.Fatal(err)
	}
	if got := writes(); got != 1 {
		t.Fatalf("%d secret writes after a refresh with the same login, want only the create", got)
	}

	exp.ImagePull = pullLogin("second-token")
	if err := c.RefreshPull(ctx, exp); err != nil {
		t.Fatal(err)
	}
	if got := writes(); got != 2 {
		t.Fatalf("%d secret writes after a refresh with a new login, want 2", got)
	}
	secret, err := c.kube.CoreV1().Secrets(HypothesisLoopNamespace).Get(ctx, pullSecretName(exp.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, password, _ := secretLogin(t, secret, "europe-west1-docker.pkg.dev"); password != "second-token" {
		t.Fatal("the pull secret does not hold the current login")
	}
}
