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

package bootstrap

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	bootstraputil "k8s.io/cluster-bootstrap/token/util"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

func TestCreateToken(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := NewDefaultProvider(cs)

	before := time.Now().UTC()
	token, err := p.CreateToken(context.Background(), 30*time.Minute, "kpssh join host-a")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	tokenID, tokenSecret, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token %q not in id.secret form", token)
	}

	sec, err := cs.CoreV1().Secrets(metav1.NamespaceSystem).
		Get(context.Background(), bootstraputil.BootstrapTokenSecretName(tokenID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("token secret not created: %v", err)
	}
	if sec.Type != corev1.SecretTypeBootstrapToken {
		t.Fatalf("secret type = %q, want %q", sec.Type, corev1.SecretTypeBootstrapToken)
	}
	want := map[string]string{
		"token-id":                       tokenID,
		"token-secret":                   tokenSecret,
		"usage-bootstrap-authentication": "true",
		"usage-bootstrap-signing":        "true",
		"auth-extra-groups":              BootstrapGroup,
		"description":                    "kpssh join host-a",
	}
	for k, v := range want {
		if sec.StringData[k] != v {
			t.Errorf("StringData[%q] = %q, want %q", k, sec.StringData[k], v)
		}
	}
	exp, err := time.Parse(time.RFC3339, sec.StringData["expiration"])
	if err != nil {
		t.Fatalf("expiration %q not RFC3339: %v", sec.StringData["expiration"], err)
	}
	if exp.Before(before.Add(29*time.Minute)) || exp.After(before.Add(31*time.Minute)) {
		t.Errorf("expiration %v not ~30m from now (%v)", exp, before)
	}
}

func TestDeleteToken(t *testing.T) {
	t.Run("deletes the secret", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		p := NewDefaultProvider(cs)
		token, err := p.CreateToken(context.Background(), time.Hour, "x")
		if err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		if err := p.DeleteToken(context.Background(), token); err != nil {
			t.Fatalf("DeleteToken: %v", err)
		}
		tokenID, _, _ := strings.Cut(token, ".")
		if _, err := cs.CoreV1().Secrets(metav1.NamespaceSystem).
			Get(context.Background(), bootstraputil.BootstrapTokenSecretName(tokenID), metav1.GetOptions{}); err == nil {
			t.Fatal("token secret still present after DeleteToken")
		}
	})

	t.Run("absent token is not an error", func(t *testing.T) {
		p := NewDefaultProvider(fake.NewSimpleClientset())
		if err := p.DeleteToken(context.Background(), "abcdef.0123456789abcdef"); err != nil {
			t.Fatalf("DeleteToken on absent secret: %v", err)
		}
	})

	t.Run("malformed token is an error", func(t *testing.T) {
		p := NewDefaultProvider(fake.NewSimpleClientset())
		if err := p.DeleteToken(context.Background(), "no-dot-here"); err == nil {
			t.Fatal("want error for malformed token")
		}
	})
}

func clusterInfoConfigMap(kubeconfig string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: metav1.NamespacePublic},
		Data:       map[string]string{"kubeconfig": kubeconfig},
	}
}

func TestClusterInfo(t *testing.T) {
	caData := []byte("test-ca-bytes")
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: ""
  cluster:
    server: https://cp.example:6443
    certificate-authority-data: %s
`, base64.StdEncoding.EncodeToString(caData))

	t.Run("spec override wins", func(t *testing.T) {
		p := NewDefaultProvider(fake.NewSimpleClientset())
		nodeClass := &v1beta1.SSHNodeClass{
			Spec: v1beta1.SSHNodeClassSpec{
				Cluster: &v1beta1.ClusterAccess{Endpoint: "https://override:6443", CABundle: []byte("ca")},
			},
		}
		info, err := p.ClusterInfo(context.Background(), nodeClass)
		if err != nil {
			t.Fatalf("ClusterInfo: %v", err)
		}
		if info.Endpoint != "https://override:6443" || info.CACertB64 != base64.StdEncoding.EncodeToString([]byte("ca")) {
			t.Fatalf("info = %+v, want spec override", info)
		}
	})

	t.Run("falls back to cluster-info", func(t *testing.T) {
		p := NewDefaultProvider(fake.NewSimpleClientset(clusterInfoConfigMap(kubeconfig)))
		info, err := p.ClusterInfo(context.Background(), &v1beta1.SSHNodeClass{})
		if err != nil {
			t.Fatalf("ClusterInfo: %v", err)
		}
		if info.Endpoint != "https://cp.example:6443" {
			t.Fatalf("endpoint = %q", info.Endpoint)
		}
		if got, _ := base64.StdEncoding.DecodeString(info.CACertB64); string(got) != string(caData) {
			t.Fatalf("CA = %q, want %q", got, caData)
		}
	})

	t.Run("missing cluster-info errors with hint", func(t *testing.T) {
		p := NewDefaultProvider(fake.NewSimpleClientset())
		_, err := p.ClusterInfo(context.Background(), &v1beta1.SSHNodeClass{})
		if err == nil || !strings.Contains(err.Error(), "nodeClass.spec.cluster") {
			t.Fatalf("err = %v, want override hint", err)
		}
	})

	t.Run("kubeconfig key missing", func(t *testing.T) {
		cm := clusterInfoConfigMap("")
		cm.Data = map[string]string{}
		p := NewDefaultProvider(fake.NewSimpleClientset(cm))
		if _, err := p.ClusterInfo(context.Background(), &v1beta1.SSHNodeClass{}); err == nil {
			t.Fatal("want error for missing kubeconfig key")
		}
	})

	t.Run("unparseable kubeconfig", func(t *testing.T) {
		p := NewDefaultProvider(fake.NewSimpleClientset(clusterInfoConfigMap("{:not yaml")))
		if _, err := p.ClusterInfo(context.Background(), &v1beta1.SSHNodeClass{}); err == nil {
			t.Fatal("want error for unparseable kubeconfig")
		}
	})

	t.Run("kubeconfig without clusters", func(t *testing.T) {
		p := NewDefaultProvider(fake.NewSimpleClientset(clusterInfoConfigMap("apiVersion: v1\nkind: Config\n")))
		if _, err := p.ClusterInfo(context.Background(), &v1beta1.SSHNodeClass{}); err == nil {
			t.Fatal("want error for kubeconfig without clusters")
		}
	})
}
