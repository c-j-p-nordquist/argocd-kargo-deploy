// Package kube builds a Kubernetes client from the user's kubeconfig.
package kube

import (
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// requestTimeout bounds every API request so a command never hangs on an
// unreachable cluster.
const requestTimeout = 15 * time.Second

// Client is a connection to one cluster.
type Client struct {
	kubernetes.Interface
	// Context is the name of the kubeconfig context in use.
	Context string
	// Config is the connection the client was built from, for building other
	// kinds of client against the same cluster.
	Config *rest.Config
}

// Connect loads the kubeconfig the same way kubectl does: the explicit path
// if one is given, otherwise $KUBECONFIG, otherwise ~/.kube/config. An empty
// contextName selects the kubeconfig's current context.
//
// Loading a kubeconfig can run a credential helper that the kubeconfig names
// (for example the GKE auth plugin). That is standard kubeconfig behaviour.
func Connect(kubeconfig, contextName string) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = requestTimeout

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	if contextName == "" {
		if raw, err := loader.RawConfig(); err == nil {
			contextName = raw.CurrentContext
		}
	}
	return &Client{Interface: clientset, Context: contextName, Config: cfg}, nil
}
