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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	statusActive   = "active"
	statusOrphaned = "orphaned"
)

// CacheEntry represents a cached model
type CacheEntry struct {
	CacheKey              string
	Source                string
	Size                  int64
	SizeHuman             string
	ModTime               time.Time
	ModelNames            []string // Models using this cache entry
	InferenceServiceNames []string // InferenceServices using this cache entry
	Status                string   // "active" or "orphaned"
}

// NewCacheCommand creates the cache command
func NewCacheCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage the model cache",
		Long: `Manage the persistent model cache.

The model cache stores downloaded models to avoid re-downloading
when Models or InferenceServices are deleted and recreated.

Examples:
  # List cached models
  llmkube cache list

  # Clear all cached models
  llmkube cache clear

  # Clear a specific cached model by name
  llmkube cache clear --model llama-3.1-8b

  # Pre-download a catalog model to the cache
  llmkube cache preload llama-3.1-8b
`,
	}

	cmd.AddCommand(newCacheListCommand())
	cmd.AddCommand(newCacheClearCommand())
	cmd.AddCommand(newCachePreloadCommand())

	return cmd
}

func newCacheListCommand() *cobra.Command {
	var namespace string
	var allNamespaces bool
	var orphanedOnly bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached models",
		Long: `List all models in the persistent cache.

Shows cache entries with their size, status, and which Model resources
are using each cache entry. Inspects the actual PVC contents to detect
orphaned cache entries that have no corresponding Model resource.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCacheList(namespace, allNamespaces, orphanedOnly)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List models from all namespaces")
	cmd.Flags().BoolVar(&orphanedOnly, "orphaned", false, "Show only orphaned cache entries (no matching Model)")

	return cmd
}

func newCacheClearCommand() *cobra.Command {
	var modelName string
	var namespace string
	var force bool

	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Clear cached models",
		Long: `Clear models from the persistent cache.

By default, clears all cached models. Use --model to clear a specific
model's cache entry.

WARNING: Clearing the cache will cause models to be re-downloaded
when InferenceServices restart or new pods are created.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCacheClear(modelName, namespace, force)
		},
	}

	cmd.Flags().StringVar(&modelName, "model", "", "Clear cache for a specific model")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace (used with --model)")
	cmd.Flags().BoolVar(&force, "force", false, "Force clear without confirmation")

	return cmd
}

func newCachePreloadCommand() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "preload MODEL_ID",
		Short: "Pre-download a model to the cache",
		Long: `Pre-download a catalog model to the persistent cache.

This allows you to download models before deploying them, useful for:
- Air-gapped environments (pre-populate cache)
- Reducing deployment time (model already cached)
- Bandwidth management (download during off-peak hours)

Examples:
  # Preload a catalog model
  llmkube cache preload llama-3.1-8b

  # Preload to a specific namespace
  llmkube cache preload llama-3.1-8b -n production
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCachePreload(args[0], namespace)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}

func runCacheList(namespace string, allNamespaces bool, orphanedOnly bool) error {
	ctx := context.Background()

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	listOpts := buildListOpts(namespace, allNamespaces)

	modelList, err := listCacheModels(ctx, k8sClient, listOpts)
	if err != nil {
		return err
	}

	cacheEntries := buildCacheEntriesFromModels(modelList, allNamespaces)

	isvcList, err := listCacheInferenceServices(ctx, k8sClient, listOpts)
	if err != nil {
		return err
	}

	modelCacheKey := buildModelCacheKeyMap(modelList)
	mapInferenceServicesToCache(cacheEntries, isvcList, modelCacheKey, allNamespaces)

	pvcInspected, err := inspectAndMergePVC(ctx, cfg, k8sClient, namespace, allNamespaces, cacheEntries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not inspect PVC contents: %v\n", err)
	}

	if orphanedOnly {
		filterOrphaned(cacheEntries)
	}

	if len(cacheEntries) == 0 {
		printEmptyCache(orphanedOnly)
		return nil
	}

	printCacheTable(cacheEntries, pvcInspected, modelList, isvcList)

	return nil
}

// buildListOpts returns the list options for the given namespace and allNamespaces flag.
func buildListOpts(namespace string, allNamespaces bool) []client.ListOption {
	if allNamespaces {
		return nil
	}
	return []client.ListOption{client.InNamespace(namespace)}
}

// listCacheModels lists all Model resources in the given namespace(s).
func listCacheModels(
	ctx context.Context, k8sClient client.Client, listOpts []client.ListOption,
) (*inferencev1alpha1.ModelList, error) {
	modelList := &inferencev1alpha1.ModelList{}
	if err := k8sClient.List(ctx, modelList, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	return modelList, nil
}

// listCacheInferenceServices lists all InferenceService resources in the given namespace(s).
func listCacheInferenceServices(
	ctx context.Context, k8sClient client.Client, listOpts []client.ListOption,
) (*inferencev1alpha1.InferenceServiceList, error) {
	isvcList := &inferencev1alpha1.InferenceServiceList{}
	if err := k8sClient.List(ctx, isvcList, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list inferenceservices: %w", err)
	}
	return isvcList, nil
}

// buildCacheEntriesFromModels creates cache entries from the model list.
func buildCacheEntriesFromModels(modelList *inferencev1alpha1.ModelList, allNamespaces bool) map[string]*CacheEntry {
	cacheEntries := make(map[string]*CacheEntry)
	for _, model := range modelList.Items {
		cacheKey := model.Status.CacheKey
		if cacheKey == "" {
			cacheKey = computeCacheKey(model.Spec.Source)
		}

		entry, exists := cacheEntries[cacheKey]
		if !exists {
			entry = &CacheEntry{
				CacheKey:   cacheKey,
				Source:     model.Spec.Source,
				ModelNames: []string{},
				Status:     statusActive,
			}
			cacheEntries[cacheKey] = entry
		}

		modelName := model.Name
		if allNamespaces {
			modelName = fmt.Sprintf("%s/%s", model.Namespace, model.Name)
		}
		entry.ModelNames = append(entry.ModelNames, modelName)

		if model.Status.Size != "" {
			entry.SizeHuman = model.Status.Size
		}
	}
	return cacheEntries
}

// buildModelCacheKeyMap builds a model-name → cache-key lookup.
func buildModelCacheKeyMap(modelList *inferencev1alpha1.ModelList) map[string]string {
	modelCacheKey := make(map[string]string, len(modelList.Items))
	for _, model := range modelList.Items {
		cacheKey := model.Status.CacheKey
		if cacheKey == "" {
			cacheKey = computeCacheKey(model.Spec.Source)
		}
		modelCacheKey[model.Name] = cacheKey
	}
	return modelCacheKey
}

// mapInferenceServicesToCache maps InferenceServices to their referenced model's cache entry.
func mapInferenceServicesToCache(
	cacheEntries map[string]*CacheEntry,
	isvcList *inferencev1alpha1.InferenceServiceList,
	modelCacheKey map[string]string,
	allNamespaces bool,
) {
	for _, isvc := range isvcList.Items {
		cacheKey, ok := modelCacheKey[isvc.Spec.ModelRef]
		if !ok {
			continue
		}

		entry, exists := cacheEntries[cacheKey]
		if !exists {
			entry = &CacheEntry{
				CacheKey:              cacheKey,
				ModelNames:            []string{},
				InferenceServiceNames: []string{},
				Status:                statusOrphaned,
			}
			cacheEntries[cacheKey] = entry
		}

		isvcName := isvc.Name
		if allNamespaces {
			isvcName = fmt.Sprintf("%s/%s", isvc.Namespace, isvc.Name)
		}
		entry.InferenceServiceNames = append(entry.InferenceServiceNames, isvcName)
	}
}

// inspectAndMergePVC inspects the PVC cache and merges results into cacheEntries.
func inspectAndMergePVC(
	ctx context.Context,
	cfg *rest.Config,
	k8sClient client.Client,
	namespace string,
	allNamespaces bool,
	cacheEntries map[string]*CacheEntry,
) (bool, error) {
	if allNamespaces {
		return false, nil
	}
	pvcEntries, err := inspectPVCCache(ctx, cfg, k8sClient, namespace)
	if err != nil {
		return false, err
	}
	if pvcEntries == nil {
		return false, nil
	}
	for _, pe := range pvcEntries {
		if entry, exists := cacheEntries[pe.CacheKey]; exists {
			entry.Size = pe.SizeBytes
			entry.SizeHuman = formatBytes(pe.SizeBytes)
		} else {
			cacheEntries[pe.CacheKey] = &CacheEntry{
				CacheKey:  pe.CacheKey,
				Size:      pe.SizeBytes,
				SizeHuman: formatBytes(pe.SizeBytes),
				Status:    statusOrphaned,
			}
		}
	}
	return true, nil
}

// filterOrphaned removes non-orphaned entries from the cache.
func filterOrphaned(cacheEntries map[string]*CacheEntry) {
	for key, entry := range cacheEntries {
		if entry.Status != statusOrphaned {
			delete(cacheEntries, key)
		}
	}
}

// printEmptyCache prints a message when no cache entries are found.
func printEmptyCache(orphanedOnly bool) {
	if orphanedOnly {
		fmt.Println("No orphaned cache entries found.")
	} else {
		fmt.Println("No cache entries found.")
	}
}

// printCacheTable prints the cache entries in a formatted table.
func printCacheTable(
	cacheEntries map[string]*CacheEntry,
	pvcInspected bool,
	modelList *inferencev1alpha1.ModelList,
	isvcList *inferencev1alpha1.InferenceServiceList,
) {
	fmt.Printf("\nModel Cache Entries\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════════════════\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if pvcInspected {
		_, _ = fmt.Fprintln(w, "CACHE KEY\tSTATUS\tSIZE\tMODELS\tISVCS\tSOURCE")
	} else {
		_, _ = fmt.Fprintln(w, "CACHE KEY\tSIZE\tMODELS\tISVCS\tSOURCE")
	}

	var activeCount, orphanedCount int
	var totalBytes int64
	for _, entry := range cacheEntries {
		models := strings.Join(entry.ModelNames, ", ")
		if len(models) > 40 {
			models = models[:37] + "..."
		}

		isvcs := strings.Join(entry.InferenceServiceNames, ", ")
		if len(isvcs) > 40 {
			isvcs = isvcs[:37] + "..."
		}
		if isvcs == "" {
			isvcs = "-"
		}

		source := entry.Source
		if len(source) > 50 {
			source = "..." + source[len(source)-47:]
		}

		size := entry.SizeHuman
		if size == "" {
			size = "-"
		}

		if entry.Status == statusOrphaned {
			orphanedCount++
		} else {
			activeCount++
		}
		totalBytes += entry.Size

		if pvcInspected {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", entry.CacheKey, entry.Status, size, models, isvcs, source)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", entry.CacheKey, size, models, isvcs, source)
		}
	}
	_ = w.Flush()

	if pvcInspected {
		fmt.Printf("\nTotal: %d cache entries (%d active, %d orphaned), %s used\n",
			len(cacheEntries), activeCount, orphanedCount, formatBytes(totalBytes))
	} else {
		fmt.Printf("\nTotal: %d cache entries, %d models, %d inferenceservices\n",
			len(cacheEntries), len(modelList.Items), len(isvcList.Items))
	}
}

func runCacheClear(modelName, namespace string, force bool) error {
	ctx := context.Background()

	// Get Kubernetes client
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	if modelName != "" {
		// Clear specific model's cache
		model := &inferencev1alpha1.Model{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: modelName, Namespace: namespace}, model); err != nil {
			return fmt.Errorf("failed to get model '%s': %w", modelName, err)
		}

		if model.Status.CacheKey == "" {
			return fmt.Errorf("model '%s' does not have a cache key (may not be cached)", modelName)
		}

		if !force {
			fmt.Printf("This will clear the cache entry for model '%s' (cache key: %s)\n", modelName, model.Status.CacheKey)
			fmt.Printf("The model will be re-downloaded when the InferenceService restarts.\n")
			fmt.Printf("Continue? [y/N] ")

			var response string
			_, _ = fmt.Scanln(&response)
			if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
				fmt.Println("Cancelled.")
				return nil
			}
		}

		// Clear cache by deleting the model's cache key directory
		// Note: This requires exec into the controller pod or direct PVC access
		fmt.Printf("To clear the cache, delete the directory on the model-cache PVC:\n")
		fmt.Printf("  kubectl exec -n llmkube-system deploy/llmkube-controller-manager -- \\\n")
		fmt.Printf("    rm -rf /models/%s\n", model.Status.CacheKey)
		fmt.Printf("\nAlternatively, delete and recreate the Model resource to trigger a re-download.\n")

		return nil
	}

	// Clear all cache
	if !force {
		fmt.Printf("This will clear ALL cached models.\n")
		fmt.Printf("All models will be re-downloaded when InferenceServices restart.\n")
		fmt.Printf("Continue? [y/N] ")

		var response string
		_, _ = fmt.Scanln(&response)
		if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	fmt.Printf("To clear all cache, run:\n")
	fmt.Printf("  kubectl exec -n llmkube-system deploy/llmkube-controller-manager -- rm -rf /models/*\n")
	fmt.Printf("\nNote: Do not delete the /models directory itself, only its contents.\n")

	return nil
}

func runCachePreload(modelID, namespace string) error {
	ctx := context.Background()

	// Get model from catalog
	catalogModel, err := GetModel(modelID)
	if err != nil {
		return fmt.Errorf("model '%s' not found in catalog: %w", modelID, err)
	}

	fmt.Printf("Preloading model: %s\n", catalogModel.Name)
	fmt.Printf("Source: %s\n", catalogModel.Source)
	fmt.Printf("Size: %s (estimated)\n\n", catalogModel.Resources.Memory)

	// Get Kubernetes client
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	// Check if model already exists
	existingModel := &inferencev1alpha1.Model{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: modelID, Namespace: namespace}, existingModel)
	if err == nil {
		if existingModel.Status.Phase == phaseReady {
			fmt.Printf("Model '%s' already exists and is ready (cached).\n", modelID)
			fmt.Printf("Cache key: %s\n", existingModel.Status.CacheKey)
			return nil
		}
		fmt.Printf("Model '%s' exists but is in phase '%s'. Waiting for it to become ready...\n",
			modelID, existingModel.Status.Phase)
	} else {
		// Create the Model resource to trigger download
		fmt.Printf("Creating Model resource to trigger download...\n")

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{
				Name:      modelID,
				Namespace: namespace,
				Labels: map[string]string{
					"llmkube.dev/preload": "true",
				},
			},
			Spec: inferencev1alpha1.ModelSpec{
				Source:       catalogModel.Source,
				Format:       "gguf",
				Quantization: catalogModel.Quantization,
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cpu", // Preload doesn't need GPU
				},
				Resources: &inferencev1alpha1.ResourceRequirements{
					CPU:    catalogModel.Resources.CPU,
					Memory: catalogModel.Resources.Memory,
				},
			},
		}

		if err := k8sClient.Create(ctx, model); err != nil {
			return fmt.Errorf("failed to create Model: %w", err)
		}
		fmt.Printf("Model resource created.\n")
	}

	// Wait for model to be ready
	fmt.Printf("\nDownloading model (this may take a while)...\n")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timeout := time.After(30 * time.Minute)

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for model download")
		case <-ticker.C:
			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: modelID, Namespace: namespace}, model); err != nil {
				continue
			}

			switch model.Status.Phase {
			case phaseReady:
				fmt.Printf("\n✅ Model preloaded successfully!\n")
				fmt.Printf("   Cache key: %s\n", model.Status.CacheKey)
				fmt.Printf("   Size: %s\n", model.Status.Size)
				fmt.Printf("   Path: %s\n", model.Status.Path)
				fmt.Printf("\nYou can now deploy this model without waiting for download:\n")
				fmt.Printf("  llmkube deploy %s --gpu\n", modelID)
				return nil
			case phaseFailed:
				return fmt.Errorf("model download failed")
			case "Downloading":
				fmt.Printf(".")
			}
		}
	}
}

// computeCacheKey generates a SHA256 hash of the source URL (same as controller)
func computeCacheKey(source string) string {
	hash := sha256.Sum256([]byte(source))
	return hex.EncodeToString(hash[:])[:16]
}
