// Copyright The karpenter-provider-ssh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package bootstrap mints kubelet TLS-bootstrap tokens and discovers the
// cluster endpoint + CA. Discovery order: SSHNodeClass.spec.cluster override,
// then the kube-public/cluster-info ConfigMap (standard bootstrap discovery —
// correct because the provider runs inside the cluster it joins nodes to).
package bootstrap

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	bootstraputil "k8s.io/cluster-bootstrap/token/util"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// BootstrapGroup is granted to minted tokens; bind node-bootstrapper +
// CSR auto-approval RBAC to it.
const BootstrapGroup = "system:bootstrappers:kpssh"

// ClusterInfo is what a joining kubelet needs.
type ClusterInfo struct {
	Endpoint  string
	CACertB64 string
}

// Provider mints tokens and resolves cluster connection info.
type Provider interface {
	CreateToken(ctx context.Context, ttl time.Duration, description string) (string, error)
	// DeleteToken removes a token's secret early (e.g. after a failed join).
	// Deleting an already-absent token is not an error.
	DeleteToken(ctx context.Context, token string) error
	// DeleteTokenByID removes a token's secret given only its public id — the
	// part recorded on the host, so a token can still be cleaned up by a
	// controller that never held the secret half (restart, node registration).
	DeleteTokenByID(ctx context.Context, tokenID string) error
	ClusterInfo(ctx context.Context, nodeClass *v1beta1.SSHNodeClass) (*ClusterInfo, error)
}

// TokenID returns the public id half of a "<id>.<secret>" bootstrap token.
func TokenID(token string) string {
	id, _, _ := strings.Cut(token, ".")
	return id
}

// DefaultProvider implements Provider with a kubernetes clientset.
type DefaultProvider struct {
	kubernetesInterface kubernetes.Interface
}

// NewDefaultProvider returns a bootstrap provider.
func NewDefaultProvider(kubernetesInterface kubernetes.Interface) *DefaultProvider {
	return &DefaultProvider{kubernetesInterface: kubernetesInterface}
}

// CreateToken implements Provider.
func (p *DefaultProvider) CreateToken(ctx context.Context, ttl time.Duration, description string) (string, error) {
	token, err := bootstraputil.GenerateBootstrapToken()
	if err != nil {
		return "", fmt.Errorf("generating bootstrap token: %w", err)
	}
	tokenID, tokenSecret, ok := strings.Cut(token, ".")
	if !ok {
		return "", fmt.Errorf("generated bootstrap token has unexpected format")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstraputil.BootstrapTokenSecretName(tokenID),
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id":                       tokenID,
			"token-secret":                   tokenSecret,
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              BootstrapGroup,
			"expiration":                     time.Now().Add(ttl).UTC().Format(time.RFC3339),
			"description":                    description,
		},
	}
	if _, err := p.kubernetesInterface.CoreV1().Secrets(metav1.NamespaceSystem).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("creating bootstrap token secret: %w", err)
	}
	return token, nil
}

// DeleteToken implements Provider. Expired tokens are otherwise only removed
// by kube-controller-manager's tokencleaner controller, which is disabled by
// default upstream — so tokens are cleaned up here proactively: unused ones as
// soon as the join fails, used ones once the node has registered.
func (p *DefaultProvider) DeleteToken(ctx context.Context, token string) error {
	tokenID, _, ok := strings.Cut(token, ".")
	if !ok {
		return fmt.Errorf("malformed bootstrap token")
	}
	return p.DeleteTokenByID(ctx, tokenID)
}

// DeleteTokenByID implements Provider.
func (p *DefaultProvider) DeleteTokenByID(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return fmt.Errorf("empty bootstrap token id")
	}
	err := p.kubernetesInterface.CoreV1().Secrets(metav1.NamespaceSystem).
		Delete(ctx, bootstraputil.BootstrapTokenSecretName(tokenID), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ClusterInfo implements Provider.
func (p *DefaultProvider) ClusterInfo(ctx context.Context, nodeClass *v1beta1.SSHNodeClass) (*ClusterInfo, error) {
	if c := nodeClass.Spec.Cluster; c != nil && c.Endpoint != "" {
		return &ClusterInfo{
			Endpoint:  c.Endpoint,
			CACertB64: base64.StdEncoding.EncodeToString(c.CABundle),
		}, nil
	}

	cm, err := p.kubernetesInterface.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading kube-public/cluster-info (set nodeClass.spec.cluster to override): %w", err)
	}
	kubeconfig, ok := cm.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("cluster-info has no kubeconfig key")
	}
	cfg, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("parsing cluster-info kubeconfig: %w", err)
	}
	names := slices.Sorted(maps.Keys(cfg.Clusters))
	if len(names) == 0 {
		return nil, fmt.Errorf("cluster-info kubeconfig has no clusters")
	}
	c := cfg.Clusters[names[0]]
	return &ClusterInfo{
		Endpoint:  c.Server,
		CACertB64: base64.StdEncoding.EncodeToString(c.CertificateAuthorityData),
	}, nil
}
