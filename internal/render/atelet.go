// Copyright 2026 The Agent Substrate Authors
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

package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ate-tooling/agent-substrate-operator/api/v1alpha1"
)

// AnnotationRegistryCAHash on atelet's pod template digests the registry
// trusted CA bundle. atelet loads its roots once, so a new bundle needs new
// pods.
const AnnotationRegistryCAHash = "substrate.ate-tooling.github.io/registry-ca-hash"

// ateletTrustDir holds the registry trusted CA bundle in atelet.
const ateletTrustDir = "/run/registry-trusted-ca"

// ateletCertDirs is SSL_CERT_DIR for atelet. atelet pulls over Go's default
// transport, which reads the system roots from these directories; listing the
// image's own directory keeps the public roots. Go skips the kubelet's
// same-directory symlinks, so each CA is read once.
const ateletCertDirs = "/etc/ssl/certs:" + ateletTrustDir

// trustedCAVolume builds a volume named name that mounts the bundle src
// names as ca.crt. field names src in errors.
func trustedCAVolume(src *v1alpha1.TrustedCASource, name, field string) (map[string]any, error) {
	ref, volume := src.ConfigMap, map[string]any{}
	if ref != nil {
		volume["configMap"] = map[string]any{"name": ref.Name}
	} else if ref = src.Secret; ref != nil {
		volume["secret"] = map[string]any{"secretName": ref.Name}
	}
	if ref == nil || ref.Name == "" {
		return nil, fmt.Errorf("%s needs a configMap or secret name", field)
	}
	for _, v := range volume {
		v.(map[string]any)["items"] = []any{map[string]any{"key": orDefault(ref.Key, "ca.crt"), "path": "ca.crt"}}
	}
	volume["name"] = name
	return volume, nil
}

// addAteletTrust mounts the registry trusted CA bundle into atelet, points
// its system roots at it as well, and stamps the digest of bundle, the
// bundle's contents, on the pod template.
func addAteletTrust(objs []*Object, src *v1alpha1.TrustedCASource, bundle []byte) error {
	volume, err := trustedCAVolume(src, "registry-trusted-ca", "atelet.registryTrustedCABundle")
	if err != nil {
		return err
	}
	ds, err := find(objs, "DaemonSet", "atelet")
	if err != nil {
		return err
	}
	if err := mutateContainer(ds, "containers", "atelet", func(c map[string]any) error {
		env, _ := c["env"].([]any)
		c["env"] = append(env, map[string]any{"name": "SSL_CERT_DIR", "value": ateletCertDirs})
		mounts, _ := c["volumeMounts"].([]any)
		c["volumeMounts"] = append(mounts, map[string]any{"name": "registry-trusted-ca", "mountPath": ateletTrustDir, "readOnly": true})
		return nil
	}); err != nil {
		return err
	}
	path := append(append([]string{}, podSpecPath...), "volumes")
	volumes, _, err := unstructured.NestedSlice(ds.Object, path...)
	if err != nil {
		return fmt.Errorf("%s: %w", describe(ds), err)
	}
	if err := unstructured.SetNestedSlice(ds.Object, append(volumes, volume), path...); err != nil {
		return err
	}
	sum := sha256.Sum256(bundle)
	return setPodAnnotation(ds, AnnotationRegistryCAHash, hex.EncodeToString(sum[:]))
}
