package k8sexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/boundedname"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// pullSecretName names the Secret the pods of an experiment that runs in an environment pull its
// image with: one for every group of the experiment.
func pullSecretName(experimentID string) string {
	return boundedname.Name("exp-"+experimentID+"-pull", experimentID+"/pull", dnsLabelLength)
}

// ensurePullSecret writes pull as the experiment's pull Secret when it holds anything else, so
// its pods pull with the login the control plane handed out last.
func (c *JobWorkloadClient) ensurePullSecret(ctx context.Context, exp *clusterapi.DesiredJob, pull *clusterapi.RegistryLogin) error {
	if pull == nil {
		return fmt.Errorf("workload: experiment %s runs in an environment and the reconcile answer has no registry_pull", exp.ID)
	}
	config, err := json.Marshal(map[string]any{"auths": map[string]any{pull.Registry: map[string]string{
		"username": pull.Username,
		"password": pull.Password,
		"auth":     base64.StdEncoding.EncodeToString([]byte(pull.Username + ":" + pull.Password)),
	}}})
	if err != nil {
		return fmt.Errorf("workload: encode the pull secret of %s: %w", exp.ID, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pullSecretName(exp.ID),
			Namespace: HypothesisLoopNamespace,
			Labels: map[string]string{
				workloadkeys.ManagedBy:    workloadkeys.ManagedByValue,
				workloadkeys.ExperimentID: exp.ID,
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: config},
	}
	secrets := c.kube.CoreV1().Secrets(HypothesisLoopNamespace)
	current, err := secrets.Get(ctx, secret.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
	case err != nil:
	case current.Type == secret.Type && maps.Equal(current.Labels, secret.Labels) &&
		bytes.Equal(current.Data[corev1.DockerConfigJsonKey], config) && len(current.Data) == 1:
		return nil
	default:
		secret.ResourceVersion = current.ResourceVersion
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("workload: write the pull secret of %s: %w", exp.ID, err)
	}
	return nil
}

// RefreshPull rewrites the pull Secret of exp's live workload with the current login: a pod that
// is rescheduled after the login it was created with expired pulls with this one.
func (c *JobWorkloadClient) RefreshPull(ctx context.Context, exp *clusterapi.DesiredJob, pull *clusterapi.RegistryLogin) error {
	return c.ensurePullSecret(ctx, exp, pull)
}

// deletePullSecret removes the experiment's pull Secret. Most experiments have none.
func (c *JobWorkloadClient) deletePullSecret(ctx context.Context, experimentID string) error {
	err := c.kube.CoreV1().Secrets(HypothesisLoopNamespace).Delete(ctx, pullSecretName(experimentID), metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// addManagedPullSecretIDs adds the experiment of every pull Secret to ids.
func (c *JobWorkloadClient) addManagedPullSecretIDs(ctx context.Context, ids map[string]struct{}) error {
	secrets, err := c.kube.CoreV1().Secrets(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ManagedBy, workloadkeys.ManagedByValue),
	})
	if err != nil {
		return err
	}
	for _, secret := range secrets.Items {
		id := secret.Labels[workloadkeys.ExperimentID]
		if id == "" {
			log.Printf("workload: skipping managed Secret %q with no experiment identity", secret.Name)
			continue
		}
		ids[id] = struct{}{}
	}
	return nil
}
