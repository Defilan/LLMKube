/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

func TestNewDeleteCommand(t *testing.T) {
	cmd := NewDeleteCommand()

	if cmd.Use != "delete [NAME]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "delete [NAME]")
	}

	expectedAliases := []string{"del", "rm"}
	if len(cmd.Aliases) != len(expectedAliases) {
		t.Errorf("Aliases count = %d, want %d", len(cmd.Aliases), len(expectedAliases))
	}
	for i, alias := range expectedAliases {
		if i < len(cmd.Aliases) && cmd.Aliases[i] != alias {
			t.Errorf("Aliases[%d] = %q, want %q", i, cmd.Aliases[i], alias)
		}
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	} else {
		if f.Shorthand != "n" {
			t.Errorf("namespace shorthand = %q, want %q", f.Shorthand, "n")
		}
		if f.DefValue != testDefaultNamespace {
			t.Errorf("namespace default = %q, want %q", f.DefValue, testDefaultNamespace)
		}
	}
}

func TestDeleteCommandRequiresArg(t *testing.T) {
	cmd := NewDeleteCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("Expected error when no argument provided")
	}
}

func TestNewDeleteCommand_PurgeCacheFlag(t *testing.T) {
	cmd := NewDeleteCommand()

	if f := cmd.Flags().Lookup("purge-cache"); f == nil {
		t.Error("Missing --purge-cache flag")
	} else {
		if f.DefValue != "false" {
			t.Errorf("purge-cache default = %q, want %q", f.DefValue, "false")
		}
	}
}

func TestDeleteOptions_PurgeCacheDefault(t *testing.T) {
	opts := &deleteOptions{
		name:      "test-model",
		namespace: testDefaultNamespace,
	}

	if opts.purgeCache {
		t.Error("purgeCache should be false by default")
	}
}

func TestDeleteOptions_PurgeCacheSet(t *testing.T) {
	opts := &deleteOptions{
		name:       "test-model",
		namespace:  testDefaultNamespace,
		purgeCache: true,
	}

	if !opts.purgeCache {
		t.Error("purgeCache should be true")
	}
}

// TestRunDelete_PurgeResolvesClaimsBeforeDeleting pins the delete order: the
// cache claims and the key come from state read before the InferenceService
// delete, because in perService mode the claim is owner-ref'd to the service
// and is collected with it. Resolving afterwards would print "nothing to
// purge" on a cluster whose GC has already removed the claim.
func TestRunDelete_PurgeResolvesClaimsBeforeDeleting(t *testing.T) {
	const (
		namespace = "default"
		modelName = "my-model"
	)
	key := cachekey.Compute("hf://org/repo")

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/repo",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   modelName,
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "my-cache-claim"},
		},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cache-claim", Namespace: namespace},
	}

	// The mock server keeps the claim after the InferenceService delete, so the
	// probe sees exactly whether resolution happened before that delete.
	probe := &deleteProbeClient{
		Client: fake.NewClientBuilder().
			WithScheme(newCLIScheme()).
			WithObjects(model, isvc, claim).
			Build(),
	}

	origKubeConfig, origClientset, origNew := deleteKubeConfig, deleteClientset, newDeleteClient
	t.Cleanup(func() {
		deleteKubeConfig, deleteClientset, newDeleteClient = origKubeConfig, origClientset, origNew
	})
	deleteKubeConfig = func() (*rest.Config, error) { return nilRESTConfig, nil }
	clientset := fakeclientset.NewClientset()
	deleteClientset = func(*rest.Config) (kubernetes.Interface, error) { return clientset, nil }
	newDeleteClient = func(*rest.Config) (deleteCRClient, error) { return probe, nil }

	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		return "removed /models/" + key + "\n", nil
	}, &cmds)

	if err := runDelete(&deleteOptions{name: modelName, namespace: namespace, purgeCache: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if probe.claimReads == 0 {
		t.Fatal("the cache claim was never read; the purge had no target")
	}
	if probe.claimReadsAfterServiceDelete > 0 {
		t.Errorf("%d cache claim reads happened after the InferenceService delete; resolve them first",
			probe.claimReadsAfterServiceDelete)
	}
	if len(cmds) == 0 {
		t.Fatal("no purge ran")
	}
	if !strings.Contains(cmds[0][2], "'/models/"+key+"'") {
		t.Errorf("purge command = %q, want the model's key", cmds[0][2])
	}
}

// deleteProbeClient records whether a claim is read before or after the
// InferenceService delete is issued through it.
type deleteProbeClient struct {
	client.Client
	isvcDeleted                  bool
	claimReads                   int
	claimReadsAfterServiceDelete int
}

func (c *deleteProbeClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*inferencev1alpha1.InferenceService); ok {
		c.isvcDeleted = true
	}
	return nil
}

func (c *deleteProbeClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		c.claimReads++
		if c.isvcDeleted {
			c.claimReadsAfterServiceDelete++
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
