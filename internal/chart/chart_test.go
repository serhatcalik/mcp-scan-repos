package chart

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func helm(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("HELM_BIN"); path != "" {
		return path
	}
	if path, err := exec.LookPath("helm"); err == nil {
		return path
	}
	name := "helm"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join("..", "..", ".tools", name)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	t.Skip("Helm is required for chart render tests")
	return ""
}

func render(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	command := exec.Command(helm(t), append([]string{"template", "test", "../../deploy/helm/gitlab-doc-platform", "--namespace", "gitlab-mcp", "--set", "notion.enabled=false,confluence.enabled=false"}, args...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("render: %v %s", err, stderr.String())
	}
	decoder := yaml.NewDecoder(bytes.NewReader(output))
	var docs []map[string]any
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

func TestChartDefaultRestrictedWorkloadsAndMonitoringLabels(t *testing.T) {
	docs := render(t)
	deployments := 0
	services := 0
	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		if kind == "HTTPRoute" {
			t.Fatalf("optional CRD emitted by default: %s", kind)
		}
		if kind == "Service" {
			meta := doc["metadata"].(map[string]any)
			name := meta["name"].(string)
			if strings.HasSuffix(name, "ollama") {
				continue
			}
			labels, ok := meta["labels"].(map[string]any)
			if !ok || labels["app.kubernetes.io/instance"] != "test" {
				t.Fatalf("unselectable service %s", name)
			}
			services++
		}
		if kind == "Deployment" {
			deployments++
			spec := doc["spec"].(map[string]any)
			pod := spec["template"].(map[string]any)["spec"].(map[string]any)
			containers := pod["containers"].([]any)
			for _, entry := range containers {
				container := entry.(map[string]any)
				security := container["securityContext"].(map[string]any)
				if security["allowPrivilegeEscalation"] != false || security["readOnlyRootFilesystem"] != true {
					t.Fatal("unrestricted workload")
				}
				for _, probe := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
					if container[probe] == nil {
						t.Fatalf("missing probe %s", probe)
					}
				}
			}
			if strategy, ok := spec["strategy"].(map[string]any); ok && strategy["type"] == "Recreate" && strategy["rollingUpdate"] != nil {
				t.Fatal("Recreate includes rollingUpdate")
			}
		}
	}
	if deployments != 3 || services != 4 {
		t.Fatalf("deployments=%d services=%d", deployments, services)
	}
}

func TestChartSharedServiceMonitorTargets(t *testing.T) {
	for _, profile := range []struct {
		name       string
		args       []string
		components []string
		interval   string
	}{
		{name: "default", components: []string{"api", "worker", "mcp"}, interval: "30s"},
		{name: "disabled", args: []string{"--set", "serviceMonitor.enabled=false"}},
		{name: "without-mcp", args: []string{"--set", "mcp.enabled=false"}, components: []string{"api", "worker"}, interval: "30s"},
		{name: "custom", args: []string{"--set", "serviceMonitor.interval=15s,serviceMonitor.additionalLabels.release=prometheus,worker.metricsPort=9191"}, components: []string{"api", "worker", "mcp"}, interval: "15s"},
	} {
		t.Run(profile.name, func(t *testing.T) {
			docs := render(t, profile.args...)
			var monitor map[string]any
			for _, doc := range docs {
				if doc["kind"] == "ServiceMonitor" {
					if monitor != nil {
						t.Fatal("expected one shared ServiceMonitor")
					}
					monitor = doc
				}
			}
			if len(profile.components) == 0 {
				if monitor != nil {
					t.Fatal("disabled ServiceMonitor emitted")
				}
				return
			}
			if monitor == nil {
				t.Fatal("missing shared ServiceMonitor")
			}
			if profile.name == "custom" && monitor["metadata"].(map[string]any)["labels"].(map[string]any)["release"] != "prometheus" {
				t.Fatal("missing Prometheus selector label")
			}
			spec := monitor["spec"].(map[string]any)
			names := spec["namespaceSelector"].(map[string]any)["matchNames"].([]any)
			if len(names) != 1 || names[0] != "gitlab-mcp" {
				t.Fatalf("unexpected scrape namespaces: %v", names)
			}
			endpoints := spec["endpoints"].([]any)
			if len(endpoints) != 1 {
				t.Fatal("expected one shared scrape endpoint")
			}
			endpoint := endpoints[0].(map[string]any)
			if endpoint["port"] != "http" || endpoint["path"] != "/metrics" || endpoint["interval"] != profile.interval {
				t.Fatalf("unexpected scrape endpoint: %v", endpoint)
			}
			relabelings := endpoint["relabelings"].([]any)
			if len(relabelings) != 1 {
				t.Fatal("expected component relabeling")
			}
			relabel := relabelings[0].(map[string]any)
			sources := relabel["sourceLabels"].([]any)
			if relabel["targetLabel"] != "component" || len(sources) != 1 || sources[0] != "__meta_kubernetes_service_label_app_kubernetes_io_component" {
				t.Fatalf("unexpected component relabeling: %v", relabel)
			}
			selector := spec["selector"].(map[string]any)["matchLabels"].(map[string]any)
			selected := make(map[string]bool)
			for _, doc := range docs {
				if doc["kind"] != "Service" {
					continue
				}
				meta := doc["metadata"].(map[string]any)
				labels, _ := meta["labels"].(map[string]any)
				matches := true
				for key, value := range selector {
					if labels[key] != value {
						matches = false
					}
				}
				if !matches {
					continue
				}
				component, _ := labels["app.kubernetes.io/component"].(string)
				if component == "" || selected[component] {
					t.Fatalf("unexpected or duplicate selected service: %v", meta)
				}
				selected[component] = true
				if component == "mcp" && !strings.HasSuffix(meta["name"].(string), "-mcp-metrics") {
					t.Fatal("public MCP service selected for scraping")
				}
				ports := doc["spec"].(map[string]any)["ports"].([]any)
				if len(ports) != 1 || ports[0].(map[string]any)["name"] != endpoint["port"] {
					t.Fatalf("scrape port missing for %s", component)
				}
				if component == "mcp" && ports[0].(map[string]any)["targetPort"] != "metrics" {
					t.Fatal("MCP scrape must use private metrics port")
				}
				if component == "worker" {
					port := ports[0].(map[string]any)
					expected := "9090"
					if profile.name == "custom" {
						expected = "9191"
					}
					if fmt.Sprint(port["port"]) != expected || port["targetPort"] != "metrics" {
						t.Fatalf("unexpected worker metrics port: %v", port)
					}
				}
			}
			if len(selected) != len(profile.components) {
				t.Fatalf("unexpected scrape targets (Ollama must be excluded): %v", selected)
			}
			for _, component := range profile.components {
				if !selected[component] {
					t.Fatalf("missing scrape target: %s", component)
				}
			}
		})
	}
}

func TestChartGatewayMonitoringAndExternalProfiles(t *testing.T) {
	docs := render(t, "--set", "gateway.enabled=true,gateway.apiHostname=api.example.com,gateway.mcpHostname=mcp.example.com,serviceMonitor.enabled=true,ollama.enabled=false,networkPolicy.externalOllamaCIDRs[0]=192.0.2.4/32")
	routes, monitor, ollama := 0, 0, false
	for _, doc := range docs {
		switch doc["kind"] {
		case "HTTPRoute":
			routes++
		case "ServiceMonitor":
			monitor++
		case "StatefulSet":
			ollama = true
		}
	}
	if routes != 2 || monitor != 1 || ollama {
		t.Fatalf("routes=%d monitor=%d ollama=%v", routes, monitor, ollama)
	}
	docs = render(t, "--set", "mcp.enabled=false,networkPolicy.enabled=false")
	for _, doc := range docs {
		meta := doc["metadata"].(map[string]any)
		if strings.HasSuffix(meta["name"].(string), "-mcp") || doc["kind"] == "NetworkPolicy" {
			t.Fatal("disabled resource emitted")
		}
	}
}

func TestChartMCPManagementIsolation(t *testing.T) {
	docs := render(t, "--set", "networkPolicy.enabled=true,mcp.metricsPort=9291")
	serviceFound, deploymentFound, policyFound := false, false, false
	for _, doc := range docs {
		name := doc["metadata"].(map[string]any)["name"].(string)
		spec, _ := doc["spec"].(map[string]any)
		if doc["kind"] == "Service" && strings.HasSuffix(name, "-mcp-metrics") {
			serviceFound = true
			port := spec["ports"].([]any)[0].(map[string]any)
			if spec["type"] != "ClusterIP" || fmt.Sprint(port["port"]) != "9291" || port["targetPort"] != "metrics" {
				t.Fatalf("unsafe MCP metrics service: %v", spec)
			}
		}
		if doc["kind"] == "Deployment" && strings.HasSuffix(name, "-mcp") {
			deploymentFound = true
			container := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			for _, probe := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
				if container[probe].(map[string]any)["httpGet"].(map[string]any)["port"] != "metrics" {
					t.Fatalf("%s uses public MCP port", probe)
				}
			}
		}
		if doc["kind"] == "NetworkPolicy" && strings.HasSuffix(name, "-ingress") && !strings.HasSuffix(name, "-ollama-ingress") {
			policyFound = true
			rules := spec["ingress"].([]any)
			if len(rules) != 2 {
				t.Fatalf("unexpected ingress rules: %v", rules)
			}
			for i, entry := range rules {
				found := false
				for _, port := range entry.(map[string]any)["ports"].([]any) {
					found = found || fmt.Sprint(port.(map[string]any)["port"]) == "9291"
				}
				if found != (i == 0) {
					t.Fatal("MCP management port must be allowed only in monitoring ingress rule")
				}
			}
		}
	}
	if !serviceFound || !deploymentFound || !policyFound {
		t.Fatal("missing MCP management resources")
	}
}

func TestChartRejectsUnsafeRWOProfile(t *testing.T) {
	output, err := exec.Command(helm(t), "template", "test", "../../deploy/helm/gitlab-doc-platform", "--set", "storage.coLocate=false,notion.enabled=false,confluence.enabled=false").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "ReadWriteMany") {
		t.Fatalf("unsafe RWO profile accepted: %s %v", output, err)
	}
	render(t, "--set", "storage.coLocate=false,storage.accessModes[0]=ReadWriteMany,worker.replicas=2,api.replicas=2")
}

func TestEmptyAllowListsDoNotBecomeUnrestrictedPolicies(t *testing.T) {
	values := filepath.Join(t.TempDir(), "empty-lists.yaml")
	if err := os.WriteFile(values, []byte("networkPolicy:\n  httpsCIDRs: []\n  monitoringNamespaces: []\n  ingressNamespaces: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docs := render(t, "-f", values, "--set", "networkPolicy.enabled=true")
	for _, doc := range docs {
		if doc["kind"] != "NetworkPolicy" {
			continue
		}
		name := doc["metadata"].(map[string]any)["name"].(string)
		spec := doc["spec"].(map[string]any)
		if strings.HasSuffix(name, "-https") {
			if rules := spec["egress"].([]any); len(rules) != 0 {
				t.Fatal("empty HTTPS list allowed egress")
			}
		}
		if strings.HasSuffix(name, "-ingress") && !strings.HasSuffix(name, "ollama-ingress") {
			if rules := spec["ingress"].([]any); len(rules) != 0 {
				t.Fatal("empty ingress lists allowed ingress")
			}
		}
	}
}

func TestDocumentedClusterProfilesRender(t *testing.T) {
	for _, profile := range []string{"values-kubernetes.yaml", "values-okd.yaml", "values-openai.yaml", "values-anthropic.yaml"} {
		t.Run(profile, func(t *testing.T) {
			render(t, "-f", "../../deploy/examples/"+profile)
		})
	}
}

func TestChartAIProviderProfiles(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			args := []string{"--set", "networkPolicy.enabled=true,llm.provider=" + provider}
			if provider != "ollama" {
				args = append(args, "--set", "llm.model=test-model,llm.apiKeySecretName=ai-key,ollama.enabled=false")
			}
			docs := render(t, args...)
			configFound, workerFound, egressFound := false, false, false
			for _, doc := range docs {
				name := doc["metadata"].(map[string]any)["name"].(string)
				if doc["kind"] == "ConfigMap" && name == "gitlab-doc-platform" {
					configFound = true
					data := doc["data"].(map[string]any)
					endpoint, model := "http://gitlab-doc-platform-ollama:11434", "qwen2.5-coder:7b"
					if provider == "openai" {
						endpoint, model = "https://api.openai.com/v1", "test-model"
					}
					if provider == "anthropic" {
						endpoint, model = "https://api.anthropic.com/v1", "test-model"
					}
					if data["LLM_ENDPOINT"] != endpoint || data["LLM_MODEL"] != model || data["LLM_PROVIDER"] != provider {
						t.Fatalf("wrong provider config: %v", data)
					}
					if _, ok := data["LLM_API_KEY"]; ok {
						t.Fatal("key in shared ConfigMap")
					}
				}
				if doc["kind"] == "StatefulSet" && provider != "ollama" {
					t.Fatal("hosted profile deploys Ollama")
				}
				if doc["kind"] == "Deployment" {
					spec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
					keyMounted := false
					for _, v := range spec["volumes"].([]any) {
						volume := v.(map[string]any)
						if volume["name"] == "llm-secret" {
							keyMounted = true
							if volume["secret"].(map[string]any)["secretName"] != "ai-key" {
								t.Fatal("wrong key Secret")
							}
						}
					}
					isWorker := strings.HasSuffix(name, "-worker")
					workerFound = workerFound || isWorker
					if keyMounted != (isWorker && provider != "ollama") {
						t.Fatalf("incorrect credential access: %s", name)
					}
				}
				if doc["kind"] == "NetworkPolicy" && strings.HasSuffix(name, "-https") {
					egressFound = true
					spec := doc["spec"].(map[string]any)
					values := spec["podSelector"].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)["values"].([]any)
					workerAllowed := false
					for _, value := range values {
						workerAllowed = workerAllowed || value == "worker"
					}
					if workerAllowed != (provider != "ollama") {
						t.Fatal("incorrect hosted inference egress")
					}
				}
			}
			if !configFound || !workerFound || !egressFound {
				t.Fatal("provider resources missing")
			}
		})
	}
}

func TestChartRejectsInvalidAIProviders(t *testing.T) {
	for _, settings := range []string{
		"llm.provider=unknown",
		"llm.provider=openai",
		"llm.provider=anthropic,llm.model=test-model",
		"llm.provider=openai,llm.model=test-model,llm.apiKeySecretName=ai-key,llm.endpoint=http://insecure.example.com/v1",
	} {
		output, err := exec.Command(helm(t), "template", "test", "../../deploy/helm/gitlab-doc-platform", "--set", "notion.enabled=false,confluence.enabled=false,"+settings).CombinedOutput()
		if err == nil {
			t.Fatalf("unsafe provider settings accepted: %s %s", settings, output)
		}
	}
}

func TestChartGitHubConfiguration(t *testing.T) {
	// Operator values may enable GitHub and populate allowlists. Set a complete
	// fixture so validation cases do not depend on those deployment settings.
	const githubSettings = "github.issuer=https://token.actions.githubusercontent.com,github.jwksURL=https://token.actions.githubusercontent.com/.well-known/jwks,github.audience=gitlab-doc-platform,github.allowedOwners=101,github.allowedRepositories=42"
	for _, enabled := range []bool{false, true} {
		docs := render(t, "--set", githubSettings, "--set", fmt.Sprintf("github.enabled=%v", enabled))
		found := false
		for _, doc := range docs {
			if doc["kind"] != "ConfigMap" || doc["metadata"].(map[string]any)["name"] != "gitlab-doc-platform" {
				continue
			}
			found = true
			data := doc["data"].(map[string]any)
			if data["GITHUB_ENABLED"] != fmt.Sprint(enabled) || data["ALLOWED_GITHUB_OWNERS"] != "101" || data["ALLOWED_GITHUB_REPOSITORIES"] != "42" || data["GITHUB_JWKS_URL"] != "https://token.actions.githubusercontent.com/.well-known/jwks" {
				t.Fatalf("incorrect GitHub configuration: %v", data)
			}
		}
		if !found {
			t.Fatal("ConfigMap missing")
		}
	}
	for _, test := range []struct {
		name, settings, message string
	}{
		{"empty allowlists", "github.enabled=true,github.allowedOwners=,github.allowedRepositories=", "GitHub requires github.allowedOwners or github.allowedRepositories"},
		{"insecure issuer", "github.enabled=true,github.issuer=http://insecure.example.com", "GitHub requires an HTTPS issuer, HTTPS JWKS URL, and audience"},
		{"insecure JWKS", "github.enabled=true,github.jwksURL=http://insecure.example.com/keys", "GitHub requires an HTTPS issuer, HTTPS JWKS URL, and audience"},
		{"empty audience", "github.enabled=true,github.audience=", "GitHub requires an HTTPS issuer, HTTPS JWKS URL, and audience"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := exec.Command(helm(t), "template", "test", "../../deploy/helm/gitlab-doc-platform", "--set", githubSettings, "--set", "notion.enabled=false,confluence.enabled=false,"+test.settings).CombinedOutput()
			if err == nil {
				t.Fatalf("unsafe GitHub configuration accepted: %s", test.settings)
			}
			if !strings.Contains(string(output), test.message) {
				t.Fatalf("unexpected Helm validation failure: %v %s", err, output)
			}
		})
	}
}
