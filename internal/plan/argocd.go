package plan

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/components"
)

// ArgoCDURL is the address Argo CD is reached at through a port-forward. It
// is also what the identity provider redirects back to after sign-in.
const ArgoCDURL = "https://localhost:8080"

// configureArgoCD writes the user's identity answers into Argo CD's rendered
// manifests. Sign-in goes to the identity provider, and only the named group
// gets a role; the rendered chart already disables the admin account and
// denies everyone else by default.
func configureArgoCD(manifests []byte, answers map[string]string) ([]byte, error) {
	// The client secret is a reference to a key in the argocd-secret Secret.
	// Its value never passes through this tool: the user adds it to the
	// cluster after the install.
	oidc, err := yaml.Marshal(map[string]any{
		"name":            "SSO",
		"issuer":          answers["identity.oidc.issuer"],
		"clientID":        answers["identity.oidc.clientId"],
		"clientSecret":    "$oidc.clientSecret",
		"requestedScopes": []string{"openid", "profile", "email", "groups"},
	})
	if err != nil {
		return nil, err
	}

	manifests, err = setConfigMapData(manifests, "argocd-cm", map[string]string{
		"url":         ArgoCDURL,
		"oidc.config": string(oidc),
	})
	if err != nil {
		return nil, err
	}
	// The group name was validated to contain no comma or line break, so it
	// cannot add a second rule to the policy.
	return setConfigMapData(manifests, "argocd-rbac-cm", map[string]string{
		"policy.csv": fmt.Sprintf("g, %s, role:admin\n", answers["identity.adminGroup"]),
	})
}

// setConfigMapData sets keys in the data of one ConfigMap inside a stream of
// rendered manifests, leaving every other document byte for byte as it was.
func setConfigMapData(manifests []byte, name string, data map[string]string) ([]byte, error) {
	const separator = "\n---\n"
	docs := strings.Split(string(manifests), separator)
	found := -1
	for i, doc := range docs {
		// A cheap text test first; only candidates are parsed.
		if !strings.Contains(doc, "kind: ConfigMap") || !strings.Contains(doc, "name: "+name+"\n") {
			continue
		}
		var parsed struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			return nil, fmt.Errorf("ConfigMap %s: %w", name, err)
		}
		if parsed.Kind == "ConfigMap" && parsed.Metadata.Name == name {
			if found >= 0 {
				return nil, fmt.Errorf("ConfigMap %s appears more than once", name)
			}
			found = i
		}
	}
	if found < 0 {
		return nil, fmt.Errorf("ConfigMap %s is not in the rendered manifests", name)
	}

	var object map[string]any
	if err := yaml.Unmarshal([]byte(docs[found]), &object); err != nil {
		return nil, err
	}
	existing, _ := object["data"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for key, value := range data {
		existing[key] = value
	}
	object["data"] = existing

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(object); err != nil {
		return nil, err
	}
	docs[found] = strings.TrimSuffix(buf.String(), "\n")
	if found == len(docs)-1 {
		docs[found] += "\n"
	}
	return []byte(strings.Join(docs, separator)), nil
}

// argoCDPolicies returns the generated resources that lock Argo CD down
// beyond what its chart does.
func argoCDPolicies(c components.Component) ([]byte, error) {
	if c.Namespace == "" {
		return nil, errors.New("Argo CD has no namespace")
	}
	return []byte(strings.ReplaceAll(argoCDPoliciesTemplate, "NAMESPACE", c.Namespace)), nil
}

// The chart's own NetworkPolicies allow ingress to each component from the
// pods that need it. These add the default deny and the egress rules.
//
// The default project is created empty before Argo CD starts. Argo CD would
// otherwise create it allowing every repository and destination.
const argoCDPoliciesTemplate = `# Everything is denied unless a policy below, or one of the chart's
# per-component ingress policies, allows it.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny
  namespace: NAMESPACE
  labels:
    app.kubernetes.io/part-of: gitops-platform
spec:
  podSelector: {}
  policyTypes:
    - Ingress
    - Egress
---
# Name resolution. The destination is not restricted, because where the
# cluster's DNS runs differs between distributions.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-dns
  namespace: NAMESPACE
  labels:
    app.kubernetes.io/part-of: gitops-platform
spec:
  podSelector: {}
  policyTypes:
    - Egress
  egress:
    - ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
---
# Argo CD's components talk to each other. What each one accepts is still
# limited by the chart's ingress policies.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-within-namespace
  namespace: NAMESPACE
  labels:
    app.kubernetes.io/part-of: gitops-platform
spec:
  podSelector: {}
  policyTypes:
    - Egress
  egress:
    - to:
        - podSelector: {}
---
# The Kubernetes API (443 or 6443), Git and registries over HTTPS, Git over
# SSH, and the identity provider. Redis needs none of these and is excluded.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-api-and-git
  namespace: NAMESPACE
  labels:
    app.kubernetes.io/part-of: gitops-platform
spec:
  podSelector:
    matchExpressions:
      - key: app.kubernetes.io/name
        operator: NotIn
        values:
          - argocd-redis
  policyTypes:
    - Egress
  egress:
    - ports:
        - protocol: TCP
          port: 443
        - protocol: TCP
          port: 6443
        - protocol: TCP
          port: 22
---
# The default project allows nothing. Applications must use a project that
# names their repositories and destinations.
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: default
  namespace: NAMESPACE
  labels:
    app.kubernetes.io/part-of: gitops-platform
spec:
  description: Locked. Use a dedicated project for every application.
  sourceRepos: []
  destinations: []
  clusterResourceWhitelist: []
`
