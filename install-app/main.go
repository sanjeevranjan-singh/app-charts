package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultChartsPath = "/charts"
	defaultNamespace  = "default"
)

// setFlags implements flag.Value to accumulate multiple --set flags.
// Go's flag package keeps only the last value for a flag; this type
// appends each occurrence so all --set values are preserved.
type setFlags []string

func (s *setFlags) String() string { return strings.Join(*s, ",") }
func (s *setFlags) Set(val string) error {
	*s = append(*s, val)
	return nil
}

type Config struct {
	FolderName    string
	ReleaseName   string
	Namespace     string
	ChartsPath    string
	ValuesFile    string
	SetValues     setFlags // supports multiple --set flags
	DryRun        bool
	Wait          bool
	Timeout       string
	CreateNS      bool
	Upgrade       bool
	KubeConfig    string
	KubeContext   string
}

func main() {
	config := parseFlags()

	if err := validateConfig(config); err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	if err := installChart(config); err != nil {
		log.Fatalf("Installation failed: %v", err)
	}

	log.Printf("Successfully installed chart from folder: %s", config.FolderName)
}

func parseFlags() *Config {
	config := &Config{}

	flag.StringVar(&config.FolderName, "folder", "", "Name of the folder containing Helm chart (required)")
	flag.StringVar(&config.ReleaseName, "release", "", "Helm release name (defaults to folder name)")
	flag.StringVar(&config.Namespace, "namespace", defaultNamespace, "Kubernetes namespace to install into")
	flag.StringVar(&config.ChartsPath, "charts-path", defaultChartsPath, "Base path where charts are located")
	flag.StringVar(&config.ValuesFile, "values", "", "Path to custom values file")
	flag.Var(&config.SetValues, "set", "Set values on command line (can be repeated: --set key=value --set key2=value2)")
	flag.BoolVar(&config.DryRun, "dry-run", false, "Simulate installation without applying")
	flag.BoolVar(&config.Wait, "wait", true, "Wait for resources to be ready")
	flag.StringVar(&config.Timeout, "timeout", "10m", "Timeout for installation")
	flag.BoolVar(&config.CreateNS, "create-namespace", true, "Create namespace if it doesn't exist")
	flag.BoolVar(&config.Upgrade, "upgrade", true, "Use helm upgrade --install for idempotent installs (set to false to use helm install)")
	flag.StringVar(&config.KubeConfig, "kubeconfig", "", "Path to kubeconfig file")
	flag.StringVar(&config.KubeContext, "context", "", "Kubernetes context to use")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: install-app [options]\n\n")
		fmt.Fprintf(os.Stderr, "A tool to install Helm charts from the packaged repository.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  # Install sock-shop chart into sock-shop namespace\n")
		fmt.Fprintf(os.Stderr, "  install-app -folder sock-shop -namespace sock-shop\n\n")
		fmt.Fprintf(os.Stderr, "  # Install with custom values file\n")
		fmt.Fprintf(os.Stderr, "  install-app -folder sock-shop -values /custom/values.yaml\n\n")
		fmt.Fprintf(os.Stderr, "  # Upgrade existing release\n")
		fmt.Fprintf(os.Stderr, "  install-app -folder sock-shop -upgrade -namespace sock-shop\n\n")
		fmt.Fprintf(os.Stderr, "  # Dry-run installation\n")
		fmt.Fprintf(os.Stderr, "  install-app -folder sock-shop -dry-run\n")
	}

	flag.Parse()

	// Default release name to folder name if not specified
	if config.ReleaseName == "" {
		config.ReleaseName = config.FolderName
	}

	return config
}

func validateConfig(config *Config) error {
	if config.FolderName == "" {
		return fmt.Errorf("folder name is required. Use -folder flag")
	}

	chartPath := filepath.Join(config.ChartsPath, config.FolderName)
	if _, err := os.Stat(chartPath); os.IsNotExist(err) {
		return fmt.Errorf("chart folder not found: %s", chartPath)
	}

	// Check for Chart.yaml to verify it's a valid Helm chart
	chartYaml := filepath.Join(chartPath, "Chart.yaml")
	if _, err := os.Stat(chartYaml); os.IsNotExist(err) {
		return fmt.Errorf("not a valid Helm chart - Chart.yaml not found in: %s", chartPath)
	}

	// Validate values file if specified
	if config.ValuesFile != "" {
		if _, err := os.Stat(config.ValuesFile); os.IsNotExist(err) {
			return fmt.Errorf("values file not found: %s", config.ValuesFile)
		}
	}

	return nil
}

func installChart(config *Config) error {
	chartPath := filepath.Join(config.ChartsPath, config.FolderName)

	// Pre-create namespace if requested, instead of relying on Helm's --create-namespace
	// which fails with "already exists" error on upgrade --install when namespace was
	// created outside of Helm
	if config.CreateNS {
		if err := ensureNamespace(config.Namespace, config.ReleaseName); err != nil {
			log.Printf("Warning: failed to ensure namespace %s: %v", config.Namespace, err)
		}
	}

	// Clean up any stuck Helm release before attempting install.
	if err := cleanupStuckRelease(config.ReleaseName, config.Namespace); err != nil {
		log.Printf("Warning: stuck release cleanup failed: %v", err)
	}

	// Adopt any pre-existing resources so Helm can manage them on upgrade --install.
	// Prevents "invalid ownership metadata" errors when resources were left behind
	// from a previous Helm release purged without deleting the underlying resources.
	if config.Upgrade {
		if err := adoptExistingResources(config); err != nil {
			log.Printf("Warning: failed to adopt existing resources: %v", err)
		}
	}

	// Build helm command
	var args []string

	if config.Upgrade {
		args = append(args, "upgrade", "--install")
	} else {
		args = append(args, "install")
	}

	args = append(args, config.ReleaseName, chartPath)
	args = append(args, "--namespace", config.Namespace)

	// Namespace is pre-created by ensureNamespace(), no need for --create-namespace

	if config.ValuesFile != "" {
		args = append(args, "-f", config.ValuesFile)
	}

	for _, setValue := range config.SetValues {
		args = append(args, "--set", setValue)
	}

	if config.DryRun {
		args = append(args, "--dry-run")
	}

	// NOTE: We intentionally do NOT pass --wait to Helm.
	// Helm v3.14's client-go rate limiter has a known bug that causes
	// "client rate limiter Wait returned an error: context deadline exceeded"
	// when polling pod readiness. Instead, we use kubectl rollout status below.

	if config.Timeout != "" {
		args = append(args, "--timeout", config.Timeout)
	}

	if config.KubeConfig != "" {
		args = append(args, "--kubeconfig", config.KubeConfig)
	}

	if config.KubeContext != "" {
		args = append(args, "--kube-context", config.KubeContext)
	}

	log.Printf("Executing: helm %s", strings.Join(args, " "))

	cmd := exec.Command("helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return err
	}

	// If --wait was requested, use kubectl rollout status instead of Helm's
	// built-in wait which suffers from client-go rate limiter bugs in v3.14
	if config.Wait {
		if err := waitForDeployments(config.Namespace, config.Timeout); err != nil {
			return fmt.Errorf("deployments not ready: %w", err)
		}
	}

	return nil
}

// waitForDeployments waits for all deployments in the namespace to be ready
// using kubectl rollout status, which doesn't suffer from Helm's rate limiter bug.
func waitForDeployments(namespace, timeout string) error {
	if timeout == "" {
		timeout = "10m"
	}

	log.Printf("Waiting for all deployments in namespace %s to be ready (timeout: %s)...", namespace, timeout)

	// Get list of deployments
	listCmd := exec.Command("kubectl", "get", "deployments", "-n", namespace, "-o", "jsonpath={.items[*].metadata.name}")
	out, err := listCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list deployments: %w", err)
	}

	deployments := strings.Fields(string(out))
	if len(deployments) == 0 {
		log.Printf("No deployments found in namespace %s, skipping wait", namespace)
		return nil
	}

	log.Printf("Found %d deployments: %s", len(deployments), strings.Join(deployments, ", "))

	// Wait for all deployments concurrently so slow Java services don't serialize the wait
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(deployments))

	for _, dep := range deployments {
		go func(d string) {
			log.Printf("Waiting for deployment %s...", d)
			waitCmd := exec.Command("kubectl", "rollout", "status", "deployment/"+d,
				"-n", namespace, "--timeout="+timeout)
			waitCmd.Stdout = os.Stdout
			waitCmd.Stderr = os.Stderr
			if err := waitCmd.Run(); err != nil {
				results <- result{d, fmt.Errorf("deployment %s not ready: %w", d, err)}
				return
			}
			log.Printf("Deployment %s is ready", d)
			results <- result{d, nil}
		}(dep)
	}

	var errs []string
	for i := 0; i < len(deployments); i++ {
		r := <-results
		if r.err != nil {
			errs = append(errs, r.err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}

	log.Printf("All deployments in namespace %s are ready", namespace)
	return nil
}

// cleanupStuckRelease checks if a Helm release exists in a broken state
// (pending-install, pending-upgrade, pending-rollback, or failed) and
// uninstalls it so that the next "helm upgrade --install" can succeed.
func cleanupStuckRelease(releaseName, namespace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get release status via helm
	statusCmd := exec.CommandContext(ctx, "helm", "status", releaseName,
		"-n", namespace, "-o", "json")
	out, err := statusCmd.Output()
	if err != nil {
		// Release doesn't exist — nothing to clean up
		return nil
	}

	status := string(out)
	stuckStates := []string{"pending-install", "pending-upgrade", "pending-rollback", "failed"}
	isStuck := false
	for _, state := range stuckStates {
		if strings.Contains(status, state) {
			isStuck = true
			log.Printf("Release %s is stuck in '%s' state, cleaning up...", releaseName, state)
			break
		}
	}

	if !isStuck {
		return nil
	}

	log.Printf("Uninstalling stuck release %s in namespace %s", releaseName, namespace)
	uninstallCmd := exec.CommandContext(ctx, "helm", "uninstall", releaseName,
		"-n", namespace, "--no-hooks")
	uninstallCmd.Stdout = os.Stdout
	uninstallCmd.Stderr = os.Stderr
	if err := uninstallCmd.Run(); err != nil {
		return fmt.Errorf("failed to uninstall stuck release %s: %w", releaseName, err)
	}

	log.Printf("Successfully cleaned up stuck release %s", releaseName)
	return nil
}

// ensureNamespace creates the namespace if it doesn't already exist and ensures
// it has the required Helm ownership labels and annotations so Helm can adopt it.
func ensureNamespace(namespace, releaseName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check if namespace exists
	checkCmd := exec.Command("kubectl", "get", "namespace", namespace)
	if err := checkCmd.Run(); err != nil {
		// Create namespace
		log.Printf("Creating namespace: %s", namespace)
		createCmd := exec.CommandContext(ctx, "kubectl", "create", "namespace", namespace)
		createCmd.Stdout = os.Stdout
		createCmd.Stderr = os.Stderr
		if err := createCmd.Run(); err != nil {
			return fmt.Errorf("failed to create namespace: %w", err)
		}
	} else {
		log.Printf("Namespace %s already exists", namespace)
	}

	// Add Helm ownership labels and annotations so Helm can adopt the namespace
	log.Printf("Labeling namespace %s for Helm ownership", namespace)
	labelCmd := exec.CommandContext(ctx, "kubectl", "label", "namespace", namespace,
		"app.kubernetes.io/managed-by=Helm", "--overwrite")
	labelCmd.Stdout = os.Stdout
	labelCmd.Stderr = os.Stderr
	if err := labelCmd.Run(); err != nil {
		return fmt.Errorf("failed to label namespace: %w", err)
	}

	annotateCmd := exec.CommandContext(ctx, "kubectl", "annotate", "namespace", namespace,
		fmt.Sprintf("meta.helm.sh/release-name=%s", releaseName),
		fmt.Sprintf("meta.helm.sh/release-namespace=%s", namespace),
		"--overwrite")
	annotateCmd.Stdout = os.Stdout
	annotateCmd.Stderr = os.Stderr
	if err := annotateCmd.Run(); err != nil {
		return fmt.Errorf("failed to annotate namespace: %w", err)
	}

	return nil
}

// adoptExistingResources uses `helm template` to discover all resources the chart will create,
// then labels/annotates any that already exist in the cluster without Helm ownership metadata.
// This prevents "invalid ownership metadata" errors on upgrade --install when resources were
// left behind after a previous release was purged without deleting the K8s resources.
func adoptExistingResources(config *Config) error {
	chartPath := filepath.Join(config.ChartsPath, config.FolderName)

	args := []string{"template", config.ReleaseName, chartPath, "--namespace", config.Namespace}
	if config.ValuesFile != "" {
		args = append(args, "-f", config.ValuesFile)
	}
	for _, setValue := range config.SetValues {
		args = append(args, "--set", setValue)
	}

	log.Printf("Discovering chart resources via: helm %s", strings.Join(args, " "))
	cmd := exec.Command("helm", args...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("helm template failed: %w", err)
	}

	resources := parseHelmTemplateOutput(string(out))
	if len(resources) == 0 {
		log.Printf("No resources discovered from chart template")
		return nil
	}

	log.Printf("Discovered %d resources from chart template", len(resources))
	adopted := 0
	for _, res := range resources {
		if adoptResource(res, config.ReleaseName, config.Namespace) {
			adopted++
		}
	}
	if adopted > 0 {
		log.Printf("Adopted %d pre-existing resources for Helm release %s", adopted, config.ReleaseName)
	}
	return nil
}

// k8sResource represents a Kubernetes resource extracted from helm template output.
type k8sResource struct {
	Kind      string
	Name      string
	Namespace string
}

// parseHelmTemplateOutput parses multi-document YAML from `helm template` and
// extracts the kind, name, and namespace of each resource.
func parseHelmTemplateOutput(output string) []k8sResource {
	docs := strings.Split(output, "---")
	var resources []k8sResource

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		var kind, name, namespace string
		inMetadata := false

		for _, line := range strings.Split(doc, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "kind:") && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				kind = strings.TrimSpace(strings.TrimPrefix(trimmed, "kind:"))
				continue
			}
			if trimmed == "metadata:" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				inMetadata = true
				continue
			}
			if inMetadata && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				inMetadata = false
			}
			if inMetadata {
				if strings.HasPrefix(trimmed, "name:") {
					indent := len(line) - len(strings.TrimLeft(line, " \t"))
					if indent <= 4 {
						name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")), "\"'")
					}
				}
				if strings.HasPrefix(trimmed, "namespace:") {
					namespace = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "namespace:")), "\"'")
				}
			}
		}
		if kind != "" && name != "" {
			resources = append(resources, k8sResource{Kind: kind, Name: name, Namespace: namespace})
		}
	}
	return resources
}

// adoptResource labels/annotates a pre-existing K8s resource with Helm ownership metadata.
// Returns true if the resource existed and was adopted.
func adoptResource(res k8sResource, releaseName, releaseNamespace string) bool {
	resourceType := strings.ToLower(res.Kind)
	ns := res.Namespace
	if ns == "" {
		ns = releaseNamespace
	}

	getArgs := []string{"get", resourceType, res.Name, "-n", ns, "--no-headers", "--ignore-not-found"}
	out, err := exec.Command("kubectl", getArgs...).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return false
	}

	log.Printf("Adopting existing %s/%s (ns=%s) for Helm release %s", res.Kind, res.Name, ns, releaseName)

	labelCmd := exec.Command("kubectl", "label", resourceType, res.Name, "-n", ns,
		"app.kubernetes.io/managed-by=Helm", "--overwrite")
	labelCmd.Stdout = os.Stdout
	labelCmd.Stderr = os.Stderr
	if err := labelCmd.Run(); err != nil {
		log.Printf("Warning: failed to label %s/%s: %v", res.Kind, res.Name, err)
	}

	annotateCmd := exec.Command("kubectl", "annotate", resourceType, res.Name, "-n", ns,
		fmt.Sprintf("meta.helm.sh/release-name=%s", releaseName),
		fmt.Sprintf("meta.helm.sh/release-namespace=%s", releaseNamespace),
		"--overwrite")
	annotateCmd.Stdout = os.Stdout
	annotateCmd.Stderr = os.Stderr
	if err := annotateCmd.Run(); err != nil {
		log.Printf("Warning: failed to annotate %s/%s: %v", res.Kind, res.Name, err)
	}
	return true
}

// ListAvailableCharts lists all available charts in the charts path
func ListAvailableCharts(chartsPath string) ([]string, error) {
	var charts []string

	entries, err := os.ReadDir(chartsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read charts directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			chartYaml := filepath.Join(chartsPath, entry.Name(), "Chart.yaml")
			if _, err := os.Stat(chartYaml); err == nil {
				charts = append(charts, entry.Name())
			}
		}
	}

	return charts, nil
}
