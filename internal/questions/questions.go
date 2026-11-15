// Package questions defines the installation questionnaire.
//
// The questionnaire is data, not an interactive prompt. An agent reads it,
// walks the user through each question, and records the answers in
// platform.yaml at the path given by each question's Field.
package questions

// SchemaVersion identifies the shape of the questionnaire output.
const SchemaVersion = "v1alpha1"

// Question types.
const (
	TypeChoice = "choice"
	TypeText   = "text"
)

// Text formats.
const (
	// FormatHTTPSURL is an absolute https:// URL.
	FormatHTTPSURL = "https-url"
	// FormatIdentifier is a name without spaces, such as an OIDC client ID.
	FormatIdentifier = "identifier"
	// FormatGroupName is the name of a group as an identity provider reports
	// it. It may contain spaces but no commas or line breaks, because it is
	// written into an access policy.
	FormatGroupName = "group-name"
)

// Patterns holds the regular expression for each format that has one.
var Patterns = map[string]string{
	FormatIdentifier: `^[A-Za-z0-9._@:/-]{1,200}$`,
	FormatGroupName:  `^[A-Za-z0-9._@:/-]([A-Za-z0-9._@:/ -]{0,198}[A-Za-z0-9._@:/-])?$`,
}

// Questionnaire is the full set of questions for one installation.
type Questionnaire struct {
	SchemaVersion string     `json:"schemaVersion"`
	Questions     []Question `json:"questions"`
}

// Question is one decision the user has to make.
type Question struct {
	// ID is stable and safe to reference from tooling.
	ID string `json:"id"`
	// Field is the path in platform.yaml where the answer is recorded.
	Field  string `json:"field"`
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
	Help   string `json:"help,omitempty"`
	// Format constrains the answer to a text question.
	Format string `json:"format,omitempty"`
	// Options is set for choice questions.
	Options []Option `json:"options,omitempty"`
	// Default is the most secure option. It is empty when the answer depends
	// on the user's environment and no safe default exists.
	Default string `json:"default,omitempty"`
}

// Option is one possible answer to a choice question.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// Security states the security consequence of choosing this option.
	Security string `json:"security"`
	// RequiresAcknowledgement marks options that weaken the baseline. Choosing
	// one must be recorded explicitly in platform.yaml.
	RequiresAcknowledgement bool `json:"requiresAcknowledgement,omitempty"`
}

// Default returns the questionnaire before it is narrowed by preflight
// results.
func Default() Questionnaire {
	return Questionnaire{
		SchemaVersion: SchemaVersion,
		Questions: []Question{
			{
				ID:      "access-exposure",
				Field:   "access.exposure",
				Type:    TypeChoice,
				Prompt:  "How should the Argo CD and Kargo UIs be reached?",
				Default: "port-forward",
				Options: []Option{
					{
						Value:    "port-forward",
						Label:    "Port-forward only",
						Security: "Nothing is exposed. Access requires a kubeconfig, so the Kubernetes API is the only entry point.",
					},
					{
						Value:    "internal-gateway",
						Label:    "Internal gateway",
						Security: "Reachable from the private network behind SSO. Anyone on that network can reach the login page.",
					},
					{
						Value:                   "public",
						Label:                   "Public endpoint with SSO",
						Security:                "The login page is reachable from the internet. Safety depends on the identity provider and on patching Argo CD and Kargo promptly.",
						RequiresAcknowledgement: true,
					},
				},
			},
			{
				ID:     "identity-oidc-issuer",
				Field:  "identity.oidc.issuer",
				Type:   TypeText,
				Format: FormatHTTPSURL,
				Prompt: "What is the OIDC issuer URL of your identity provider?",
				Help:   "Sign-in is SSO only. The built-in admin account is disabled.",
			},
			{
				ID:     "identity-oidc-client-id",
				Field:  "identity.oidc.clientId",
				Type:   TypeText,
				Format: FormatIdentifier,
				Prompt: "What is the client ID of the OIDC application registered for Argo CD?",
				Help:   "Register the redirect URI https://localhost:8080/auth/callback with it. The client secret is never asked for here: after the install you add it to the cluster yourself.",
			},
			{
				ID:     "identity-admin-group",
				Field:  "identity.adminGroup",
				Type:   TypeText,
				Format: FormatGroupName,
				Prompt: "Which group in your identity provider should administer Argo CD?",
				Help:   "Members of this group get the admin role. Everyone else gets no access until a policy grants it.",
			},
			{
				ID:     "git-host",
				Field:  "git.host",
				Type:   TypeChoice,
				Prompt: "Where is the platform Git repository hosted?",
				Options: []Option{
					{
						Value:    "github",
						Label:    "GitHub",
						Security: "Supports GitHub Apps, which give short-lived, repository-scoped tokens.",
					},
					{
						Value:    "gitlab",
						Label:    "GitLab",
						Security: "Supports deploy keys and project access tokens, which are long-lived and need rotation.",
					},
					{
						Value:    "other",
						Label:    "Another Git host",
						Security: "Credential options depend on the host. SSH deploy keys are the usual least-privilege choice.",
					},
				},
			},
			{
				ID:      "git-credential",
				Field:   "git.credential",
				Type:    TypeChoice,
				Prompt:  "How should Argo CD and Kargo authenticate to Git?",
				Default: "github-app",
				Options: []Option{
					{
						Value:    "github-app",
						Label:    "GitHub App",
						Security: "Short-lived tokens scoped to selected repositories. Only available on GitHub.",
					},
					{
						Value:    "deploy-key",
						Label:    "SSH deploy key per repository",
						Security: "Scoped to one repository, but long-lived. Rotation is manual.",
					},
					{
						Value:                   "token",
						Label:                   "Personal or project access token",
						Security:                "Long-lived and often broader than one repository. A leaked token exposes everything it can reach.",
						RequiresAcknowledgement: true,
					},
				},
			},
			{
				ID:     "secrets-backend",
				Field:  "secrets.backend",
				Type:   TypeChoice,
				Prompt: "Where should External Secrets Operator read secrets from?",
				Help:   "There is no default. Pick the store that matches where the cluster runs.",
				Options: []Option{
					{
						Value:                   "in-cluster",
						Label:                   "In-cluster store (demo)",
						Security:                "Secrets exist only inside the cluster. There is no external copy, audit trail or rotation, protection at rest depends on etcd encryption, and losing the cluster loses the secrets.",
						RequiresAcknowledgement: true,
					},
					{
						Value:    "gcp-secret-manager",
						Label:    "GCP Secret Manager",
						Security: "Secrets are held outside the cluster with IAM, audit logs and versioning. Access uses workload identity, so no static key is stored.",
					},
					{
						Value:    "aws-secrets-manager",
						Label:    "AWS Secrets Manager",
						Security: "Secrets are held outside the cluster with IAM, audit logs and rotation. Access uses workload identity where the cluster supports it.",
					},
					{
						Value:    "azure-key-vault",
						Label:    "Azure Key Vault",
						Security: "Secrets are held outside the cluster with access policies, audit logs and versioning. Access uses workload identity where the cluster supports it.",
					},
					{
						Value:    "vault",
						Label:    "Vault or OpenBao",
						Security: "Secrets are held outside the cluster with policies and audit logs. You operate and secure the Vault yourself.",
					},
				},
			},
			{
				ID:      "tenancy-mode",
				Field:   "tenancy.mode",
				Type:    TypeChoice,
				Prompt:  "Will one team or several teams deploy through this platform?",
				Default: "multi-tenant",
				Options: []Option{
					{
						Value:    "multi-tenant",
						Label:    "Several teams",
						Security: "Each team gets its own AppProject, namespaces and RBAC, so one team cannot deploy into another's namespaces.",
					},
					{
						Value:    "single-team",
						Label:    "One team",
						Security: "One shared AppProject. Simpler, but every user of the platform can deploy to every application namespace.",
					},
				},
			},
			{
				ID:      "promotion-production",
				Field:   "promotion.production",
				Type:    TypeChoice,
				Prompt:  "How should changes be promoted to production?",
				Default: "pull-request",
				Options: []Option{
					{
						Value:    "pull-request",
						Label:    "Pull request with approval",
						Security: "A person reviews and merges every production change, and Git records who approved it.",
					},
					{
						Value:                   "automatic",
						Label:                   "Automatic after verification",
						Security:                "Production changes without human review. A compromised image or upstream stage reaches production on its own.",
						RequiresAcknowledgement: true,
					},
				},
			},
			{
				ID:      "components-argo-rollouts",
				Field:   "components.argoRollouts",
				Type:    TypeChoice,
				Prompt:  "Install Argo Rollouts for analysis-based verification of promotions?",
				Default: "disabled",
				Options: []Option{
					{
						Value:    "disabled",
						Label:    "Do not install",
						Security: "One less third-party controller running in the cluster.",
					},
					{
						Value:    "enabled",
						Label:    "Install",
						Security: "Adds a controller with write access to workloads. Needed only if Kargo stages should run analysis checks.",
					},
				},
			},
		},
	}
}
