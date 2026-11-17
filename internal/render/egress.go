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

// The marker splices below are ported from upstream
// cmd/ate-setup/internal/steps/overlay.go at the embedded release.

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
)

// credentialProvider is the provider the egress gateway asks for credentials.
// The zero value turns injection off.
type credentialProvider struct {
	name    string
	address string
}

func (p credentialProvider) enabled() bool { return p.name != "" }

// serverName is the SAN the gateway expects on the provider's serving
// certificate: the address without its port.
func (p credentialProvider) serverName() string {
	if i := strings.LastIndex(p.address, ":"); i >= 0 {
		return p.address[:i]
	}
	return p.address
}

// patchEgress splices the additional ext_proc filter, when extproc names one,
// and the credential provider flags into the Envoy egress manifest. With
// trust, it repoints the re-originating clusters at the bundle
// addEgressTrust mounts.
func patchEgress(raw []byte, extproc string, provider credentialProvider, trust bool) ([]byte, error) {
	if extproc != "" {
		var err error
		if raw, err = spliceExtproc(raw, extproc); err != nil {
			return nil, err
		}
	}
	if trust {
		var err error
		if raw, err = repointOriginTrust(raw); err != nil {
			return nil, err
		}
	}
	var flags string
	if provider.enabled() {
		flags = fmt.Sprintf(`- --credential-provider-name=%s
- --credential-provider-address=%s
- --credential-provider-server-name=%s
- --credential-provider-ca-file=/run/servicedns.podcert.ate.dev/trust-bundle.pem
- --credential-provider-client-cert=/run/podidentity.podcert.ate.dev/credential-bundle.pem`, provider.name, provider.address, provider.serverName())
	}
	return replaceMarker(raw, "#ATE_EGRESS_INJECT_FLAGS", flags, 1)
}

// The egress trusted CA bundle is a stopgap until upstream lets the egress
// gateway trust a private CA itself; drop it then.
const (
	egressTrustDir    = "/run/egress-trusted-ca"
	egressTrustSecret = "egress_origin_validation_context"
	egressTrustSDS    = "sds-egress-origin-validation.yaml"
)

// egressTrustValidation replaces the public root bundle the re-originating
// clusters trust inline, which Envoy reads once at startup, with filesystem
// SDS, which it re-reads when the kubelet swaps the mounted volume.
const egressTrustValidation = `validation_context_sds_secret_config:
  name: ` + egressTrustSecret + `
  sds_config:
    resource_api_version: V3
    path_config_source:
      path: /etc/envoy/` + egressTrustSDS

const egressTrustSecretResource = `resources:
- "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret
  name: ` + egressTrustSecret + `
  validation_context:
    trusted_ca: { filename: ` + egressTrustDir + `/ca.crt }
    watched_directory: { path: ` + egressTrustDir + ` }
`

// repointOriginTrust swaps each inline validation_context trusting the
// image's public roots for egressTrustValidation, and fails unless it finds
// the two of egress_forward_proxy and egress_forward_proxy_grpc.
func repointOriginTrust(raw []byte) ([]byte, error) {
	lines := strings.Split(string(raw), "\n")
	var out []string
	replaced := 0
	for i := 0; i < len(lines); i++ {
		if i+2 < len(lines) && strings.TrimSpace(lines[i]) == "validation_context:" &&
			strings.TrimSpace(lines[i+1]) == "trusted_ca:" &&
			strings.TrimSpace(lines[i+2]) == "filename: /etc/ssl/certs/ca-certificates.crt" {
			indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
			for _, l := range strings.Split(egressTrustValidation, "\n") {
				out = append(out, indent+l)
			}
			i += 2
			replaced++
			continue
		}
		out = append(out, lines[i])
	}
	if replaced != 2 {
		return nil, fmt.Errorf("expected 2 origin validation contexts in the egress config, found %d", replaced)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// addEgressTrust adds the SDS resource for the trusted bundle to the egress
// ConfigMap and mounts the bundle into Envoy. It mounts the whole volume, not
// a subPath, so the kubelet's updates reach Envoy.
func addEgressTrust(objs []*Object, src *v1alpha1.TrustedCASource) error {
	ref, volume := src.ConfigMap, map[string]any{}
	if ref != nil {
		volume["configMap"] = map[string]any{"name": ref.Name}
	} else if ref = src.Secret; ref != nil {
		volume["secret"] = map[string]any{"secretName": ref.Name}
	}
	if ref == nil || ref.Name == "" {
		return fmt.Errorf("atenet.egressTrustedCABundle needs a configMap or secret name")
	}
	for _, v := range volume {
		v.(map[string]any)["items"] = []any{map[string]any{"key": orDefault(ref.Key, "ca.crt"), "path": "ca.crt"}}
	}
	volume["name"] = "egress-trusted-ca"

	cm, err := find(objs, "ConfigMap", "atenet-egress")
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedField(cm.Object, egressTrustSecretResource, "data", egressTrustSDS); err != nil {
		return err
	}

	dep, err := find(objs, "Deployment", "atenet-egress")
	if err != nil {
		return err
	}
	if err := mutateContainer(dep, "containers", "envoy", func(c map[string]any) error {
		mounts, _ := c["volumeMounts"].([]any)
		c["volumeMounts"] = append(mounts, map[string]any{"name": "egress-trusted-ca", "mountPath": egressTrustDir, "readOnly": true})
		return nil
	}); err != nil {
		return err
	}
	path := append(append([]string{}, podSpecPath...), "volumes")
	volumes, _, err := unstructured.NestedSlice(dep.Object, path...)
	if err != nil {
		return fmt.Errorf("%s: %w", describe(dep), err)
	}
	return unstructured.SetNestedSlice(dep.Object, append(volumes, volume), path...)
}

// replaceMarker replaces each line starting with marker by block, indented
// like the marker, and fails unless it finds want of them.
func replaceMarker(raw []byte, marker, block string, want int) ([]byte, error) {
	var out []string
	replaced := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), marker) {
			out = append(out, line)
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if block != "" {
			for _, l := range strings.Split(block, "\n") {
				if l == "" {
					out = append(out, "")
				} else {
					out = append(out, indent+l)
				}
			}
		}
		replaced++
	}
	if replaced != want {
		return nil, fmt.Errorf("expected %d %s markers, found %d", want, marker, replaced)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// spliceExtproc adds the external processor authorization filter to both
// egress listeners and its cluster to the cluster list. spec is
// NAMESPACE/SERVICE:PORT.
func spliceExtproc(raw []byte, spec string) ([]byte, error) {
	namespace, svcPort, ok := strings.Cut(spec, "/")
	service, port, ok2 := strings.Cut(svcPort, ":")
	if !ok || !ok2 {
		return nil, fmt.Errorf("additionalEgressExtprocService %q is not NAMESPACE/SERVICE:PORT", spec)
	}
	address := fmt.Sprintf("%s.%s.svc.cluster.local", service, namespace)
	serverName := fmt.Sprintf("%s.%s.svc", service, namespace)

	raw, err := replaceMarker(raw, "#ATE_MITM_EXTPROC_FILTER", extprocFilter, 2)
	if err != nil {
		return nil, err
	}
	return replaceMarker(raw, "#ATE_MITM_EXTPROC_CLUSTER", fmt.Sprintf(extprocCluster,
		serverName, serverName, address, port), 1)
}

const extprocFilter = `- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    grpc_service:
      envoy_grpc:
        cluster_name: additional_egress_ext_proc
      timeout: 2s
    failure_mode_allow: false
    message_timeout: 2s
    request_attributes:
    - filter_state['dev.ate.actor.identity']
    processing_mode:
      request_header_mode: SEND
      response_header_mode: SKIP
      request_body_mode: NONE
      response_body_mode: NONE
      request_trailer_mode: SKIP
      response_trailer_mode: SKIP
    mutation_rules:
      disallow_system: true
      disallow_is_error: true`

// extprocCluster takes the SNI, the SAN to match, the address, and the port.
const extprocCluster = `- name: additional_egress_ext_proc
  type: STRICT_DNS
  lb_policy: ROUND_ROBIN
  connect_timeout: 1s
  typed_extension_protocol_options:
    envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
      "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
      explicit_http_config:
        http2_protocol_options: {}
  transport_socket:
    name: envoy.transport_sockets.tls
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
      sni: %s
      common_tls_context:
        tls_params:
          tls_minimum_protocol_version: TLSv1_3
          tls_maximum_protocol_version: TLSv1_3
        tls_certificate_sds_secret_configs:
        - name: podidentity_client_cert
          sds_config:
            resource_api_version: V3
            path_config_source:
              path: /etc/envoy/sds-podidentity-cert.yaml
        combined_validation_context:
          default_validation_context:
            match_typed_subject_alt_names:
            - san_type: DNS
              matcher:
                exact: %s
          validation_context_sds_secret_config:
            name: servicedns_validation_context
            sds_config:
              resource_api_version: V3
              path_config_source:
                path: /etc/envoy/sds-servicedns-validation.yaml
  load_assignment:
    cluster_name: additional_egress_ext_proc
    endpoints:
    - lb_endpoints:
      - endpoint:
          address:
            socket_address:
              address: %s
              port_value: %s`
