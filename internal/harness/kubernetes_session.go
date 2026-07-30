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

package harness

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

// SessionForAgent reads the Agent's API-key Secret and constructs a Harness
// SDK session. An explicit namespace is authoritative; an empty namespace
// keeps the direct-binary behavior of reading beside the Agent.
func SessionForAgent(
	ctx context.Context,
	reader client.Reader,
	apiKeySecretNamespace string,
	agent *infrastructurev1.HarnessGitopsAgent,
) (*Session, error) {
	secretNamespace := strings.TrimSpace(apiKeySecretNamespace)
	if secretNamespace == "" {
		secretNamespace = agent.Namespace
	}

	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{
		Name:      agent.Spec.ApiKeySecretRef,
		Namespace: secretNamespace,
	}, secret); err != nil {
		return nil, err
	}

	apiKey, ok := secret.Data["api_key"]
	if !ok || len(apiKey) == 0 {
		return nil, k8serrors.NewBadRequest("api_key not found in secret")
	}
	return NewSession(string(apiKey))
}
