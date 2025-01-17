// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

// Package config implements the webhook that injects DD_AGENT_HOST and
// DD_ENTITY_ID into a pod template as needed
package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/samber/lo"
	admiv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/DataDog/datadog-agent/cmd/cluster-agent/admission"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	admCommon "github.com/DataDog/datadog-agent/pkg/clusteragent/admission/common"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/admission/metrics"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/admission/mutate/autoinstrumentation"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/admission/mutate/common"
	"github.com/DataDog/datadog-agent/pkg/config"
	apiCommon "github.com/DataDog/datadog-agent/pkg/util/kubernetes/apiserver/common"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	// Env vars
	agentHostEnvVarName      = "DD_AGENT_HOST"
	ddEntityIDEnvVarName     = "DD_ENTITY_ID"
	ddExternalDataEnvVarName = "DD_EXTERNAL_ENV"
	traceURLEnvVarName       = "DD_TRACE_AGENT_URL"
	dogstatsdURLEnvVarName   = "DD_DOGSTATSD_URL"
	podUIDEnvVarName         = "DD_INTERNAL_POD_UID"

	// External Data Prefixes
	// These prefixes are used to build the External Data Environment Variable.
	// This variable is then used for Origin Detection.
	externalDataInitPrefix          = "it-"
	externalDataContainerNamePrefix = "cn-"
	externalDataPodUIDPrefix        = "pu-"

	// Config injection modes
	hostIP  = "hostip"
	socket  = "socket"
	service = "service"

	// DatadogVolumeName is the name of the volume used to mount the socket
	DatadogVolumeName = "datadog"

	webhookName = "agent_config"
)

var (
	agentHostIPEnvVar = corev1.EnvVar{
		Name:  agentHostEnvVarName,
		Value: "",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "status.hostIP",
			},
		},
	}

	agentHostServiceEnvVar = corev1.EnvVar{
		Name:  agentHostEnvVarName,
		Value: config.Datadog().GetString("admission_controller.inject_config.local_service_name") + "." + apiCommon.GetMyNamespace() + ".svc.cluster.local",
	}

	defaultDdEntityIDEnvVar = corev1.EnvVar{
		Name:  ddEntityIDEnvVarName,
		Value: "",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.uid",
			},
		},
	}

	traceURLSocketEnvVar = corev1.EnvVar{
		Name:  traceURLEnvVarName,
		Value: config.Datadog().GetString("admission_controller.inject_config.trace_agent_socket"),
	}

	dogstatsdURLSocketEnvVar = corev1.EnvVar{
		Name:  dogstatsdURLEnvVarName,
		Value: config.Datadog().GetString("admission_controller.inject_config.dogstatsd_socket"),
	}
)

// conf is the configuration for the webhook
type conf struct {
	injectContName bool
}

// Webhook is the webhook that injects DD_AGENT_HOST and DD_ENTITY_ID into a pod
type Webhook struct {
	name       string
	config     conf
	isEnabled  bool
	endpoint   string
	resources  []string
	operations []admiv1.OperationType
	mode       string
	wmeta      workloadmeta.Component
}

// NewWebhook returns a new Webhook
func NewWebhook(wmeta workloadmeta.Component) *Webhook {
	return &Webhook{
		name: webhookName,
		config: conf{
			injectContName: config.Datadog().GetBool("admission_controller.inject_config.inject_container_name"),
		},
		isEnabled:  config.Datadog().GetBool("admission_controller.inject_config.enabled"),
		endpoint:   config.Datadog().GetString("admission_controller.inject_config.endpoint"),
		resources:  []string{"pods"},
		operations: []admiv1.OperationType{admiv1.Create},
		mode:       config.Datadog().GetString("admission_controller.inject_config.mode"),
		wmeta:      wmeta,
	}
}

// Name returns the name of the webhook
func (w *Webhook) Name() string {
	return w.name
}

// IsEnabled returns whether the webhook is enabled
func (w *Webhook) IsEnabled() bool {
	return w.isEnabled
}

// Endpoint returns the endpoint of the webhook
func (w *Webhook) Endpoint() string {
	return w.endpoint
}

// Resources returns the kubernetes resources for which the webhook should
// be invoked
func (w *Webhook) Resources() []string {
	return w.resources
}

// Operations returns the operations on the resources specified for which
// the webhook should be invoked
func (w *Webhook) Operations() []admiv1.OperationType {
	return w.operations
}

// LabelSelectors returns the label selectors that specify when the webhook
// should be invoked
func (w *Webhook) LabelSelectors(useNamespaceSelector bool) (namespaceSelector *metav1.LabelSelector, objectSelector *metav1.LabelSelector) {
	return common.DefaultLabelSelectors(useNamespaceSelector)
}

// MutateFunc returns the function that mutates the resources
func (w *Webhook) MutateFunc() admission.WebhookFunc {
	return w.mutate
}

// mutate adds the DD_AGENT_HOST and DD_ENTITY_ID env vars to the pod template if they don't exist
func (w *Webhook) mutate(request *admission.MutateRequest) ([]byte, error) {
	return common.Mutate(request.Raw, request.Namespace, w.Name(), w.inject, request.DynamicClient)
}

// inject injects the following environment variables into the pod template:
// - DD_AGENT_HOST: the host IP of the node
// - DD_ENTITY_ID: the entity ID of the pod
// - DD_EXTERNAL_ENV: the External Data Environment Variable
func (w *Webhook) inject(pod *corev1.Pod, _ string, _ dynamic.Interface) (bool, error) {
	var injectedConfig, injectedEntity, injectedExternalEnv bool

	if pod == nil {
		return false, errors.New(metrics.InvalidInput)
	}

	if !autoinstrumentation.ShouldInject(pod, w.wmeta) {
		return false, nil
	}

	// Inject DD_AGENT_HOST
	switch injectionMode(pod, w.mode) {
	case hostIP:
		injectedConfig = common.InjectEnv(pod, agentHostIPEnvVar)
	case service:
		injectedConfig = common.InjectEnv(pod, agentHostServiceEnvVar)
	case socket:
		volume, volumeMount := buildVolume(DatadogVolumeName, config.Datadog().GetString("admission_controller.inject_config.socket_path"), true)
		injectedVol := common.InjectVolume(pod, volume, volumeMount)
		injectedEnv := common.InjectEnv(pod, traceURLSocketEnvVar)
		injectedEnv = common.InjectEnv(pod, dogstatsdURLSocketEnvVar) || injectedEnv
		injectedConfig = injectedEnv || injectedVol
	default:
		log.Errorf("invalid injection mode %q", w.mode)
		return false, errors.New(metrics.InvalidInput)
	}

	// Inject DD_ENTITY_ID
	if w.config.injectContName {
		injectedEntity = injectFullIdentity(pod)
	} else {
		injectedEntity = common.InjectEnv(pod, defaultDdEntityIDEnvVar)
	}

	// Inject External Data Environment Variable
	injectedExternalEnv = injectExternalDataEnvVar(pod)

	return injectedConfig || injectedEntity || injectedExternalEnv, nil
}

// injectionMode returns the injection mode based on the global mode and pod labels
func injectionMode(pod *corev1.Pod, globalMode string) string {
	if val, found := pod.GetLabels()[admCommon.InjectionModeLabelKey]; found {
		mode := strings.ToLower(val)
		switch mode {
		case hostIP, service, socket:
			return mode
		default:
			log.Warnf("Invalid label value '%s=%s' on pod %s should be either 'hostip', 'service' or 'socket', defaulting to %q", admCommon.InjectionModeLabelKey, val, common.PodString(pod), globalMode)
			return globalMode
		}
	}

	return globalMode
}

// injectExternalDataEnvVar injects the External Data environment variable.
// The format is: it-<init>,cn-<container_name>,pu-<pod_uid>
func injectExternalDataEnvVar(pod *corev1.Pod) (injected bool) {
	type containerInjection struct {
		container *corev1.Container
		init      bool
	}
	var containerInjections []containerInjection

	// Collect all containers and init containers
	for i := range pod.Spec.Containers {
		containerInjections = append(containerInjections, containerInjection{&pod.Spec.Containers[i], false})
	}
	for i := range pod.Spec.InitContainers {
		containerInjections = append(containerInjections, containerInjection{&pod.Spec.InitContainers[i], true})
	}

	// Inject External Data Environment Variable for each container
	for _, containerInjection := range containerInjections {
		if containerInjection.container == nil {
			_ = log.Errorf("Cannot inject identity into nil container")
			continue
		}

		containerInjection.container.Env = append([]corev1.EnvVar{
			{
				Name: podUIDEnvVarName,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.uid",
					},
				},
			},
			{
				Name:  ddExternalDataEnvVarName,
				Value: fmt.Sprintf("%s%t,%s%s,%s$(%s)", externalDataInitPrefix, containerInjection.init, externalDataContainerNamePrefix, containerInjection.container.Name, externalDataPodUIDPrefix, podUIDEnvVarName),
			},
		}, containerInjection.container.Env...)

		injected = true
	}

	return injected
}

func injectIdentityInContainer(container *corev1.Container, prefix, podStr string) bool {
	if container == nil {
		_ = log.Errorf("Cannot inject identity into nil container")
		return false
	}
	// Do not override DD_ENTITY_ID if it's already set
	if lo.ContainsBy(container.Env, func(v corev1.EnvVar) bool { return v.Name == ddEntityIDEnvVarName }) {
		log.Debugf("Ignoring container '%s' in pod %s: env var '%s' already exist", container.Name, podStr, ddEntityIDEnvVarName)
		return false
	}

	// We can and should override DD_INTERNAL_* variables if they are already set
	container.Env = lo.Filter(container.Env, func(v corev1.EnvVar, _ int) bool { return v.Name != podUIDEnvVarName })
	addedEnv := []corev1.EnvVar{
		// DD_INTERNAL_POD_UID must precede DD_ENTITY_ID to be referenced in the latter.
		// See https://kubernetes.io/docs/tasks/inject-data-application/define-interdependent-environment-variables/
		{
			Name: podUIDEnvVarName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.uid",
				},
			},
		},
		{
			Name:  ddEntityIDEnvVarName,
			Value: fmt.Sprintf("en-%s$(%s)/%s", prefix, podUIDEnvVarName, container.Name),
		},
	}

	// prepend rather than append so that our new vars precede container vars in the final list, so that they
	// can be referenced in other env vars downstream.  (see:  Kubernetes dependent environment variables.)
	container.Env = append(addedEnv, container.Env...)
	return true
}

// injectFullIdentity injects `DD_INTERNAL_CONTAINER_NAME`, `DD_INTERNAL_POD_UID` and `DD_ENTITY_ID`
// as `en-(init.)$(DD_INTERNAL_POD_UID)/$(DD_INTERNAL_CONTAINER_NAME)`.
func injectFullIdentity(pod *corev1.Pod) bool {
	injected := false
	podStr := common.PodString(pod)
	for i := range pod.Spec.Containers {
		injected = injectIdentityInContainer(&pod.Spec.Containers[i], "", podStr) || injected
	}
	for i := range pod.Spec.InitContainers {
		injected = injectIdentityInContainer(&pod.Spec.InitContainers[i], "init.", podStr) || injected
	}
	return injected
}

func buildVolume(volumeName, path string, readOnly bool) (corev1.Volume, corev1.VolumeMount) {
	pathType := corev1.HostPathDirectoryOrCreate
	volume := corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: path,
				Type: &pathType,
			},
		},
	}

	volumeMount := corev1.VolumeMount{
		Name:      volumeName,
		MountPath: path,
		ReadOnly:  readOnly,
	}

	return volume, volumeMount
}
