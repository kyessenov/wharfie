// Copyright 2017 Istio Authors
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

package inject

// NOTE: This tool only exists because kubernetes does not support
// dynamic/out-of-tree admission controller for transparent proxy
// injection. This file should be removed as soon as a proper kubernetes
// admission controller is written for istio.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/ghodss/yaml"
	multierror "github.com/hashicorp/go-multierror"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	yamlDecoder "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/pkg/api/v1"
	batch "k8s.io/client-go/pkg/apis/batch/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"

	proxyconfig "istio.io/api/proxy/v1/config"
)

// Defaults values for injecting istio proxy into kubernetes
// resources.
const (
	DefaultSidecarProxyUID = int64(1337)
	DefaultVerbosity       = 2
)

const (
	istioSidecarAnnotationSidecarKey   = "alpha.istio.io/sidecar"
	istioSidecarAnnotationSidecarValue = "injected"
	istioSidecarAnnotationVersionKey   = "alpha.istio.io/version"
	initContainerName                  = "init"
	proxyContainerName                 = "proxy"
	enableCoreDumpContainerName        = "enable-core-dump"
	enableCoreDumpImage                = "alpine"

	istioCertVolumeName   = "istio-certs"
	istioCertSecretPrefix = "istio."
)

// InitImageName returns the fully qualified image name for the istio
// init image given a docker hub and tag
func InitImageName(hub, tag string) string { return hub + "/init:" + tag }

// ProxyImageName returns the fully qualified image name for the istio
// proxy image given a docker hub and tag.
func ProxyImageName(hub, tag string) string { return hub + "/proxy_debug:" + tag }

// Params describes configurable parameters for injecting istio proxy
// into kubernetes resource.
type Params struct {
	InitImage         string
	ProxyImage        string
	Verbosity         int
	SidecarProxyUID   int64
	Version           string
	EnableCoreDump    bool
	Mesh              *proxyconfig.ProxyMeshConfig
	MeshConfigMapName string
	// Comma separated list of IP ranges in CIDR form. If set, only
	// redirect outbound traffic to Envoy for these IP
	// ranges. Otherwise all outbound traffic is redirected to Envoy.
	IncludeIPRanges string
}

var enableCoreDumpContainer = map[string]interface{}{
	"name":    enableCoreDumpContainerName,
	"image":   enableCoreDumpImage,
	"command": []string{"/bin/sh"},
	"args": []string{
		"-c",
		"sysctl -w kernel.core_pattern=/tmp/core.%e.%p.%t && ulimit -c unlimited",
	},
	"imagePullPolicy": "Always",
	"securityContext": map[string]interface{}{
		"privileged": true,
	},
}

func injectIntoPodTemplateSpec(p *Params, t *v1.PodTemplateSpec) error {
	if t.Annotations == nil {
		t.Annotations = make(map[string]string)
	} else if _, ok := t.Annotations[istioSidecarAnnotationSidecarKey]; ok {
		// Return unmodified resource if sidecar is already present or ignored.
		return nil
	}
	t.Annotations[istioSidecarAnnotationSidecarKey] = istioSidecarAnnotationSidecarValue
	t.Annotations[istioSidecarAnnotationVersionKey] = p.Version

	// init-container
	var annotations []interface{}
	if initContainer, ok := t.Annotations["pod.beta.kubernetes.io/init-containers"]; ok {
		if err := json.Unmarshal([]byte(initContainer), &annotations); err != nil {
			return err
		}
	}
	initArgs := []string{
		"-p", fmt.Sprintf("%d", p.Mesh.ProxyListenPort),
		"-u", strconv.FormatInt(p.SidecarProxyUID, 10),
	}
	if p.IncludeIPRanges != "" {
		initArgs = append(initArgs, "-i", p.IncludeIPRanges)
	}
	annotations = append(annotations, map[string]interface{}{
		"name":            initContainerName,
		"image":           p.InitImage,
		"args":            initArgs,
		"imagePullPolicy": "Always",
		"securityContext": map[string]interface{}{
			"capabilities": map[string]interface{}{
				"add": []string{"NET_ADMIN"},
			},
		},
	})

	if p.EnableCoreDump {
		annotations = append(annotations, enableCoreDumpContainer)
	}

	initAnnotationValue, err := json.Marshal(&annotations)
	if err != nil {
		return err
	}
	t.Annotations["pod.beta.kubernetes.io/init-containers"] = string(initAnnotationValue)

	// sidecar proxy container
	args := []string{
		"proxy",
		"sidecar",
	}

	if p.Verbosity > 0 {
		args = append(args, "-v", strconv.Itoa(p.Verbosity))
	}
	if p.MeshConfigMapName != "" {
		args = append(args, "--meshConfig", p.MeshConfigMapName)
	}

	ports, err := healthPorts(t)
	if err != nil {
		return err
	}
	for _, port := range ports {
		args = append(args, "--passthrough", strconv.Itoa(port))
	}

	var volumeMounts []v1.VolumeMount
	if p.Mesh.AuthPolicy == proxyconfig.ProxyMeshConfig_MUTUAL_TLS {
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			Name:      istioCertVolumeName,
			ReadOnly:  true,
			MountPath: p.Mesh.AuthCertsPath,
		})

		sa := t.Spec.ServiceAccountName
		if sa == "" {
			sa = "default"
		}
		t.Spec.Volumes = append(t.Spec.Volumes, v1.Volume{
			Name: istioCertVolumeName,
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: istioCertSecretPrefix + sa,
				},
			},
		})
	}

	sidecar := v1.Container{
		Name:  proxyContainerName,
		Image: p.ProxyImage,
		Args:  args,
		Env: []v1.EnvVar{{
			Name: "POD_NAME",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		}, {
			Name: "POD_NAMESPACE",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		}, {
			Name: "POD_IP",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		}},
		ImagePullPolicy: v1.PullAlways,
		SecurityContext: &v1.SecurityContext{
			RunAsUser: &p.SidecarProxyUID,
		},
		VolumeMounts: volumeMounts,
	}
	t.Spec.Containers = append(t.Spec.Containers, sidecar)

	return nil
}

func resolvePort(c v1.Container, port intstr.IntOrString) (int, error) {
	switch port.Type {
	case intstr.Int:
		return port.IntValue(), nil
	case intstr.String:
		for _, named := range c.Ports {
			if named.Name == port.String() {
				return int(named.ContainerPort), nil
			}
		}
		return 0, fmt.Errorf("missing named port %q", port)
	default:
		return 0, fmt.Errorf("incorrect port type %q", port)
	}
}

func healthPorts(t *v1.PodTemplateSpec) ([]int, error) {
	set := make(map[int]bool)
	var errs error
	for _, container := range t.Spec.Containers {
		if container.LivenessProbe != nil && container.LivenessProbe.HTTPGet != nil {
			port, err := resolvePort(container, container.LivenessProbe.HTTPGet.Port)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				set[port] = true
			}
		}
		if container.ReadinessProbe != nil && container.ReadinessProbe.HTTPGet != nil {
			port, err := resolvePort(container, container.ReadinessProbe.HTTPGet.Port)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				set[port] = true
			}
		}
	}

	out := make([]int, 0, len(set))
	for port := range set {
		out = append(out, port)
	}
	sort.Ints(out)
	return out, errs

}

// IntoResourceFile injects the istio proxy into the specified
// kubernetes YAML file.
func IntoResourceFile(p *Params, in io.Reader, out io.Writer) error {
	reader := yamlDecoder.NewYAMLReader(bufio.NewReaderSize(in, 4096))
	for {
		raw, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		kinds := map[string]struct {
			typ    interface{}
			inject func(typ interface{}) error
		}{
			"Job": {
				typ: &batch.Job{},
				inject: func(typ interface{}) error {
					return injectIntoPodTemplateSpec(p, &((typ.(*batch.Job)).Spec.Template))
				},
			},
			"DaemonSet": {
				typ: &v1beta1.DaemonSet{},
				inject: func(typ interface{}) error {
					return injectIntoPodTemplateSpec(p, &((typ.(*v1beta1.DaemonSet)).Spec.Template))
				},
			},
			"ReplicaSet": {
				typ: &v1beta1.ReplicaSet{},
				inject: func(typ interface{}) error {
					return injectIntoPodTemplateSpec(p, &((typ.(*v1beta1.ReplicaSet)).Spec.Template))
				},
			},
			"Deployment": {
				typ: &v1beta1.Deployment{},
				inject: func(typ interface{}) error {
					return injectIntoPodTemplateSpec(p, &((typ.(*v1beta1.Deployment)).Spec.Template))
				},
			},
			"ReplicationController": {
				typ: &v1.ReplicationController{},
				inject: func(typ interface{}) error {
					return injectIntoPodTemplateSpec(p, ((typ.(*v1.ReplicationController)).Spec.Template))
				},
			},
		}
		var updated []byte
		var meta metav1.TypeMeta
		if err = yaml.Unmarshal(raw, &meta); err != nil {
			return err
		}
		if kind, ok := kinds[meta.Kind]; ok {
			if err = yaml.Unmarshal(raw, kind.typ); err != nil {
				return err
			}
			if err = kind.inject(kind.typ); err != nil {
				return err
			}
			if updated, err = yaml.Marshal(kind.typ); err != nil {
				return err
			}
		} else {
			updated = raw // unchanged
		}

		if _, err = out.Write(updated); err != nil {
			return err
		}
		if _, err = fmt.Fprint(out, "---\n"); err != nil {
			return err
		}
	}
	return nil
}
