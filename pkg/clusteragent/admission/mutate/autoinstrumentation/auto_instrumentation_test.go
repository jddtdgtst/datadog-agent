// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package autoinstrumentation

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/DataDog/datadog-agent/comp/core"
	configComp "github.com/DataDog/datadog-agent/comp/core/config"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	workloadmetafxmock "github.com/DataDog/datadog-agent/comp/core/workloadmeta/fx-mock"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/admission/mutate/common"
	configmock "github.com/DataDog/datadog-agent/pkg/config/mock"
	model "github.com/DataDog/datadog-agent/pkg/config/model"
	"github.com/DataDog/datadog-agent/pkg/languagedetection/util"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/pointer"
)

const commonRegistry = "gcr.io/datadoghq"

func TestInjectAutoInstruConfig(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		libsToInject   []libInfo
		expectedEnvKey string
		expectedEnvVal string
		wantErr        bool
	}{
		{
			name: "nominal case: java",
			pod:  common.FakePod("java-pod"),
			libsToInject: []libInfo{
				{
					lang:  "java",
					image: "gcr.io/datadoghq/dd-lib-java-init:v1",
				},
			},
			expectedEnvKey: "JAVA_TOOL_OPTIONS",
			expectedEnvVal: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
			wantErr:        false,
		},
		{
			name: "JAVA_TOOL_OPTIONS not empty",
			pod:  common.FakePodWithEnvValue("java-pod", "JAVA_TOOL_OPTIONS", "predefined"),
			libsToInject: []libInfo{
				{
					lang:  "java",
					image: "gcr.io/datadoghq/dd-lib-java-init:v1",
				},
			},
			expectedEnvKey: "JAVA_TOOL_OPTIONS",
			expectedEnvVal: "predefined -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
			wantErr:        false,
		},
		{
			name: "JAVA_TOOL_OPTIONS set via ValueFrom",
			pod:  common.FakePodWithEnvFieldRefValue("java-pod", "JAVA_TOOL_OPTIONS", "path"),
			libsToInject: []libInfo{
				{
					lang:  "java",
					image: "gcr.io/datadoghq/dd-lib-java-init:v1",
				},
			},
			wantErr: true,
		},
		{
			name: "nominal case: js",
			pod:  common.FakePod("js-pod"),
			libsToInject: []libInfo{
				{
					lang:  "js",
					image: "gcr.io/datadoghq/dd-lib-js-init:v1",
				},
			},
			expectedEnvKey: "NODE_OPTIONS",
			expectedEnvVal: " --require=/datadog-lib/node_modules/dd-trace/init",
			wantErr:        false,
		},
		{
			name: "NODE_OPTIONS not empty",
			pod:  common.FakePodWithEnvValue("js-pod", "NODE_OPTIONS", "predefined"),
			libsToInject: []libInfo{
				{
					lang:  "js",
					image: "gcr.io/datadoghq/dd-lib-js-init:v1",
				},
			},
			expectedEnvKey: "NODE_OPTIONS",
			expectedEnvVal: "predefined --require=/datadog-lib/node_modules/dd-trace/init",
			wantErr:        false,
		},
		{
			name: "NODE_OPTIONS set via ValueFrom",
			pod:  common.FakePodWithEnvFieldRefValue("js-pod", "NODE_OPTIONS", "path"),
			libsToInject: []libInfo{
				{
					lang:  "js",
					image: "gcr.io/datadoghq/dd-lib-js-init:v1",
				},
			},
			wantErr: true,
		},
		{
			name: "nominal case: python",
			pod:  common.FakePod("python-pod"),
			libsToInject: []libInfo{
				{
					lang:  "python",
					image: "gcr.io/datadoghq/dd-lib-python-init:v1",
				},
			},
			expectedEnvKey: "PYTHONPATH",
			expectedEnvVal: "/datadog-lib/",
			wantErr:        false,
		},
		{
			name: "PYTHONPATH not empty",
			pod:  common.FakePodWithEnvValue("python-pod", "PYTHONPATH", "predefined"),
			libsToInject: []libInfo{
				{
					lang:  "python",
					image: "gcr.io/datadoghq/dd-lib-python-init:v1",
				},
			},
			expectedEnvKey: "PYTHONPATH",
			expectedEnvVal: "/datadog-lib/:predefined",
			wantErr:        false,
		},
		{
			name: "PYTHONPATH set via ValueFrom",
			pod:  common.FakePodWithEnvFieldRefValue("python-pod", "PYTHONPATH", "path"),
			libsToInject: []libInfo{
				{
					lang:  "python",
					image: "gcr.io/datadoghq/dd-lib-python-init:v1",
				},
			},
			wantErr: true,
		},
		{
			name: "Unknown language",
			pod:  common.FakePod("unknown-pod"),
			libsToInject: []libInfo{
				{
					lang:  "unknown",
					image: "gcr.io/datadoghq/dd-lib-unknown-init:v1",
				},
			},
			wantErr: true,
		},
		{
			name: "nominal case: dotnet",
			pod:  common.FakePod("dotnet-pod"),
			libsToInject: []libInfo{
				{
					lang:  "dotnet",
					image: "gcr.io/datadoghq/dd-lib-dotnet-init:v1",
				},
			},
			expectedEnvKey: "CORECLR_PROFILER",
			expectedEnvVal: "{846F5F1C-F9AE-4B07-969E-05C26BC060D8}",
			wantErr:        false,
		},
		{
			name: "CORECLR_ENABLE_PROFILING not empty",
			pod:  common.FakePodWithEnvValue("dotnet-pod", "CORECLR_PROFILER", "predefined"),
			libsToInject: []libInfo{
				{
					lang:  "dotnet",
					image: "gcr.io/datadoghq/dd-lib-dotnet-init:v1",
				},
			},
			expectedEnvKey: "CORECLR_PROFILER",
			expectedEnvVal: "{846F5F1C-F9AE-4B07-969E-05C26BC060D8}",
			wantErr:        false,
		},
		{
			name: "CORECLR_ENABLE_PROFILING set via ValueFrom",
			pod:  common.FakePodWithEnvFieldRefValue("dotnet-pod", "CORECLR_PROFILER", "path"),
			libsToInject: []libInfo{
				{
					lang:  dotnet,
					image: "gcr.io/datadoghq/dd-lib-dotnet-init:v1",
				},
			},
			wantErr: true,
		},
		{
			name: "nominal case: ruby",
			pod:  common.FakePod("ruby-pod"),
			libsToInject: []libInfo{
				{
					lang:  ruby,
					image: "gcr.io/datadoghq/dd-lib-ruby-init:v1",
				},
			},
			expectedEnvKey: "RUBYOPT",
			expectedEnvVal: " -r/datadog-lib/auto_inject",
			wantErr:        false,
		},
		{
			name: "RUBYOPT not empty",
			pod:  common.FakePodWithEnvValue("ruby-pod", "RUBYOPT", "predefined"),
			libsToInject: []libInfo{
				{
					lang:  "ruby",
					image: "gcr.io/datadoghq/dd-lib-ruby-init:v1",
				},
			},
			expectedEnvKey: "RUBYOPT",
			expectedEnvVal: "predefined -r/datadog-lib/auto_inject",
			wantErr:        false,
		},
		{
			name: "RUBYOPT set via ValueFrom",
			pod:  common.FakePodWithEnvFieldRefValue("ruby-pod", "RUBYOPT", "path"),
			libsToInject: []libInfo{
				{
					lang:  "ruby",
					image: "gcr.io/datadoghq/dd-lib-ruby-init:v1",
				},
			},
			wantErr: true,
		},
	}
	wmeta := fxutil.Test[workloadmeta.Component](t, core.MockBundle(), workloadmetafxmock.MockModule(), fx.Supply(workloadmeta.NewParams()))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			webhook, err := GetWebhook(wmeta)
			require.NoError(t, err)

			err = webhook.injectAutoInstruConfig(tt.pod, tt.libsToInject, false, "")
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if err != nil {
				return
			}
			assertLibReq(t, tt.pod, tt.libsToInject[0].lang, tt.libsToInject[0].image, tt.expectedEnvKey, tt.expectedEnvVal)
		})
	}
}

func assertLibReq(t *testing.T, pod *corev1.Pod, lang language, image, envKey, envVal string) {
	// Empty dir volume
	volumeFound := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "datadog-auto-instrumentation" {
			require.NotNil(t, volume.VolumeSource.EmptyDir)
			volumeFound = true
			break
		}
	}
	require.True(t, volumeFound)

	// Init container
	initContainerFound := false
	for _, container := range pod.Spec.InitContainers {
		if container.Name == fmt.Sprintf("datadog-lib-%s-init", lang) {
			require.Equal(t, image, container.Image)
			require.Equal(t, []string{"sh", "copy-lib.sh", "/datadog-lib"}, container.Command)
			require.Equal(t, "datadog-auto-instrumentation", container.VolumeMounts[0].Name)
			require.Equal(t, "/datadog-lib", container.VolumeMounts[0].MountPath)
			initContainerFound = true
			break
		}
	}
	require.True(t, initContainerFound)

	// App container
	container := pod.Spec.Containers[0]
	require.Equal(t, "datadog-auto-instrumentation", container.VolumeMounts[0].Name)
	require.Equal(t, "/datadog-lib", container.VolumeMounts[0].MountPath)
	envFound := false
	for _, env := range container.Env {
		if env.Name == envKey {
			require.Equal(t, envVal, env.Value)
			envFound = true
			break
		}
	}
	require.True(t, envFound, "expected to find env %s with value %s", envKey, envVal)
}

func TestExtractLibInfo(t *testing.T) {
	// TODO: Add new entry when a new language is supported
	allLatestLibs := []libInfo{
		{
			lang:  "java",
			image: "registry/dd-lib-java-init:v1",
		},
		{
			lang:  "js",
			image: "registry/dd-lib-js-init:v5",
		},
		{
			lang:  "python",
			image: "registry/dd-lib-python-init:v2",
		},
		{
			lang:  "dotnet",
			image: "registry/dd-lib-dotnet-init:v2",
		},
		{
			lang:  "ruby",
			image: "registry/dd-lib-ruby-init:v2",
		},
	}

	var mockConfig model.Config
	tests := []struct {
		name                 string
		pod                  *corev1.Pod
		containerRegistry    string
		expectedLibsToInject []libInfo
		expectedPodEligible  *bool
		setupConfig          func()
	}{
		{
			name:              "java",
			pod:               common.FakePodWithAnnotation("admission.datadoghq.com/java-lib.version", "v1"),
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: "registry/dd-lib-java-init:v1",
				},
			},
		},
		{
			name:              "java from common registry",
			pod:               common.FakePodWithAnnotation("admission.datadoghq.com/java-lib.version", "v1"),
			containerRegistry: "",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: fmt.Sprintf("%s/dd-lib-java-init:v1", commonRegistry),
				},
			},
		},
		{
			name:              "js",
			pod:               common.FakePodWithAnnotation("admission.datadoghq.com/js-lib.version", "v1"),
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "js",
					image: "registry/dd-lib-js-init:v1",
				},
			},
		},
		{
			name:                "python",
			pod:                 common.FakePodWithAnnotation("admission.datadoghq.com/python-lib.version", "v1"),
			containerRegistry:   "registry",
			expectedPodEligible: pointer.Ptr(true),
			expectedLibsToInject: []libInfo{
				{
					lang:  "python",
					image: "registry/dd-lib-python-init:v1",
				},
			},
		},
		{
			name:                 "python with unlabelled injection off",
			pod:                  common.FakePodWithAnnotation("admission.datadoghq.com/python-lib.version", "v1"),
			containerRegistry:    "registry",
			expectedPodEligible:  pointer.Ptr(false),
			expectedLibsToInject: []libInfo{},
			setupConfig: func() {
				mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false)
			},
		},
		{
			name:              "custom",
			pod:               common.FakePodWithAnnotation("admission.datadoghq.com/java-lib.custom-image", "custom/image"),
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: "custom/image",
				},
			},
		},
		{
			name:                 "unknown",
			pod:                  common.FakePodWithAnnotation("admission.datadoghq.com/unknown-lib.version", "v1"),
			containerRegistry:    "registry",
			expectedLibsToInject: []libInfo{},
		},
		{
			name: "java and js",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"admission.datadoghq.com/java-lib.version": "v1",
						"admission.datadoghq.com/js-lib.version":   "v1",
					},
				},
			},
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: "registry/dd-lib-java-init:v1",
				},
				{
					lang:  "js",
					image: "registry/dd-lib-js-init:v1",
				},
			},
		},
		{
			name: "java and js on specific containers",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"admission.datadoghq.com/java-app.java-lib.version": "v1",
						"admission.datadoghq.com/node-app.js-lib.version":   "v1",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "java-app",
						},
						{
							Name: "node-app",
						},
					},
				},
			},
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					ctrName: "java-app",
					lang:    "java",
					image:   "registry/dd-lib-java-init:v1",
				},
				{
					ctrName: "node-app",
					lang:    "js",
					image:   "registry/dd-lib-js-init:v1",
				},
			},
		},
		{
			name:              "ruby",
			pod:               common.FakePodWithAnnotation("admission.datadoghq.com/ruby-lib.version", "v1"),
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "ruby",
					image: "registry/dd-lib-ruby-init:v1",
				},
			},
		},
		{
			name:                 "all",
			pod:                  common.FakePodWithAnnotation("admission.datadoghq.com/all-lib.version", "latest"),
			containerRegistry:    "registry",
			expectedPodEligible:  pointer.Ptr(true),
			expectedLibsToInject: allLatestLibs,
		},
		{
			name:                 "all with mutate_unlabelled off",
			pod:                  common.FakePodWithAnnotation("admission.datadoghq.com/all-lib.version", "latest"),
			containerRegistry:    "registry",
			expectedPodEligible:  pointer.Ptr(false),
			expectedLibsToInject: allLatestLibs,
			setupConfig: func() {
				mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false)
			},
		},
		{
			name: "all with mutate_unlabelled off, but labelled admission enabled",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"admission.datadoghq.com/all-lib.version": "latest",
					},
					Labels: map[string]string{
						"admission.datadoghq.com/enabled": "true",
					},
				},
			},
			containerRegistry:    "registry",
			expectedPodEligible:  pointer.Ptr(true),
			expectedLibsToInject: allLatestLibs,
			setupConfig: func() {
				mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false)
			},
		},
		{
			name:                 "all with mutate_unlabelled off",
			pod:                  common.FakePodWithAnnotation("admission.datadoghq.com/all-lib.version", "latest"),
			containerRegistry:    "registry",
			expectedPodEligible:  pointer.Ptr(false),
			expectedLibsToInject: allLatestLibs,
			setupConfig: func() {
				mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false)
			},
		},
		{
			name: "all with mutate_unlabelled off, but labelled admission enabled",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"admission.datadoghq.com/all-lib.version": "latest",
					},
					Labels: map[string]string{
						"admission.datadoghq.com/enabled": "true",
					},
				},
			},
			containerRegistry:    "registry",
			expectedPodEligible:  pointer.Ptr(true),
			expectedLibsToInject: allLatestLibs,
			setupConfig: func() {
				mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false)
			},
		},
		{
			name: "java + all",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"admission.datadoghq.com/all-lib.version":  "latest",
						"admission.datadoghq.com/java-lib.version": "v1",
					},
				},
			},
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: "registry/dd-lib-java-init:v1",
				},
			},
		},
		{
			name:                 "all with unsupported version",
			pod:                  common.FakePodWithAnnotation("admission.datadoghq.com/all-lib.version", "unsupported"),
			containerRegistry:    "registry",
			expectedLibsToInject: allLatestLibs,
			setupConfig:          func() { mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", false) },
		},
		{
			name:                 "single step instrumentation with no pinned versions",
			pod:                  common.FakePodWithNamespaceAndLabel("ns", "", ""),
			containerRegistry:    "registry",
			expectedLibsToInject: allLatestLibs,
			setupConfig:          func() { mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true) },
		},
		{
			name:              "single step instrumentation with pinned java version",
			pod:               common.FakePodWithNamespaceAndLabel("ns", "", ""),
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: "registry/dd-lib-java-init:v1.20.0",
				},
			},
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.lib_versions", map[string]string{"java": "v1.20.0"})
			},
		},
		{
			name:              "single step instrumentation with pinned java and python versions",
			pod:               common.FakePodWithNamespaceAndLabel("ns", "", ""),
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: "registry/dd-lib-java-init:v1.20.0",
				},
				{
					lang:  "python",
					image: "registry/dd-lib-python-init:v1.19.0",
				},
			},
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.lib_versions", map[string]string{"java": "v1.20.0", "python": "v1.19.0"})
			},
		},
		{
			name:              "single step instrumentation with pinned java version and java annotation",
			pod:               common.FakePodWithAnnotation("admission.datadoghq.com/java-lib.version", "v1"),
			containerRegistry: "registry",
			expectedLibsToInject: []libInfo{
				{
					lang:  "java",
					image: "registry/dd-lib-java-init:v1",
				},
			},
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.lib_versions", map[string]string{"java": "v1.20.0"})
				mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", true)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fix me: since comp-config and pkg-config don't work together yet we have to set settings
			// twice
			overrides := map[string]interface{}{
				"admission_controller.mutate_unlabelled":  true,
				"admission_controller.container_registry": commonRegistry,
			}
			if tt.containerRegistry != "" {
				overrides["admission_controller.auto_instrumentation.container_registry"] = tt.containerRegistry
			}

			wmeta := fxutil.Test[workloadmeta.Component](t,
				core.MockBundle(),
				fx.Replace(configComp.MockParams{Overrides: overrides}),
				workloadmetafxmock.MockModule(),
				fx.Supply(workloadmeta.NewParams()),
			)
			mockConfig = configmock.New(t)
			for k, v := range overrides {
				mockConfig.SetWithoutSource(k, v)
			}

			if tt.setupConfig != nil {
				tt.setupConfig()
			}

			// Need to create a new instance of the webhook to take into account
			// the config changes.
			UnsetWebhook()
			apmInstrumentationWebhook, errInitAPMInstrumentation := GetWebhook(wmeta)
			require.NoError(t, errInitAPMInstrumentation)

			if tt.expectedPodEligible != nil {
				require.Equal(t, *tt.expectedPodEligible, apmInstrumentationWebhook.isPodEligible(tt.pod))
			}

			libsToInject, _ := apmInstrumentationWebhook.extractLibInfo(tt.pod)
			require.ElementsMatch(t, tt.expectedLibsToInject, libsToInject)
		})
	}
}

func TestInjectLibConfig(t *testing.T) {
	tests := []struct {
		name         string
		pod          *corev1.Pod
		lang         language
		wantErr      bool
		expectedEnvs []corev1.EnvVar
	}{
		{
			name:    "nominal case",
			pod:     common.FakePodWithAnnotation("admission.datadoghq.com/java-lib.config.v1", `{"version":1,"service_language":"java","runtime_metrics_enabled":true,"tracing_rate_limit":50}`),
			lang:    java,
			wantErr: false,
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "50",
				},
			},
		},
		{
			name:    "inject all case",
			pod:     common.FakePodWithAnnotation("admission.datadoghq.com/all-lib.config.v1", `{"version":1,"service_language":"all","runtime_metrics_enabled":true,"tracing_rate_limit":50}`),
			lang:    "all",
			wantErr: false,
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "50",
				},
			},
		},
		{
			name:         "invalid json",
			pod:          common.FakePodWithAnnotation("admission.datadoghq.com/java-lib.config.v1", "invalid"),
			lang:         java,
			wantErr:      true,
			expectedEnvs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := injectLibConfig(tt.pod, tt.lang)
			require.False(t, (err != nil) != tt.wantErr)
			if err != nil {
				return
			}
			container := tt.pod.Spec.Containers[0]
			envCount := 0
			for _, expectEnv := range tt.expectedEnvs {
				for _, contEnv := range container.Env {
					if expectEnv.Name == contEnv.Name {
						require.Equal(t, expectEnv.Value, contEnv.Value)
						envCount++
						break
					}
				}
			}
			require.Equal(t, len(tt.expectedEnvs), envCount)
		})
	}
}

func TestInjectLibInitContainer(t *testing.T) {
	tests := []struct {
		name    string
		cpu     string
		mem     string
		pod     *corev1.Pod
		image   string
		lang    language
		wantErr bool
		wantCPU string
		wantMem string
	}{
		{
			name:    "no resources",
			pod:     common.FakePod("java-pod"),
			image:   "gcr.io/datadoghq/dd-lib-java-init:v1",
			lang:    java,
			wantErr: false,
			wantCPU: "50m",
			wantMem: "20Mi",
		},
		{
			name:    "with resources",
			pod:     common.FakePod("java-pod"),
			cpu:     "100m",
			mem:     "500",
			image:   "gcr.io/datadoghq/dd-lib-java-init:v1",
			lang:    java,
			wantErr: false,
			wantCPU: "100m",
			wantMem: "500",
		},
		{
			name:    "cpu only",
			pod:     common.FakePod("java-pod"),
			cpu:     "200m",
			image:   "gcr.io/datadoghq/dd-lib-java-init:v1",
			lang:    java,
			wantErr: false,
			wantCPU: "200m",
			wantMem: "20Mi",
		},
		{
			name:    "memory only",
			pod:     common.FakePod("java-pod"),
			mem:     "512Mi",
			image:   "gcr.io/datadoghq/dd-lib-java-init:v1",
			lang:    java,
			wantErr: false,
			wantCPU: "50m",
			wantMem: "512Mi",
		},
		{
			name:    "with invalid resources",
			pod:     common.FakePod("java-pod"),
			cpu:     "foo",
			image:   "gcr.io/datadoghq/dd-lib-java-init:v1",
			lang:    java,
			wantErr: true,
			wantCPU: "50m",
			wantMem: "20Mi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := configmock.New(t)
			if tt.cpu != "" {
				conf.SetWithoutSource("admission_controller.auto_instrumentation.init_resources.cpu", tt.cpu)
			}
			if tt.mem != "" {
				conf.SetWithoutSource("admission_controller.auto_instrumentation.init_resources.memory", tt.mem)
			}
			err := injectLibInitContainer(tt.pod, tt.image, tt.lang)
			if (err != nil) != tt.wantErr {
				t.Errorf("injectLibInitContainer() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			require.Len(t, tt.pod.Spec.InitContainers, 1)

			req := tt.pod.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU]
			lim := tt.pod.Spec.InitContainers[0].Resources.Limits[corev1.ResourceCPU]
			wantCPUQuantity := resource.MustParse(tt.wantCPU)
			require.Zero(t, wantCPUQuantity.Cmp(req)) // Cmp returns 0 if equal
			require.Zero(t, wantCPUQuantity.Cmp(lim))

			req = tt.pod.Spec.InitContainers[0].Resources.Requests[corev1.ResourceMemory]
			lim = tt.pod.Spec.InitContainers[0].Resources.Limits[corev1.ResourceMemory]
			wantMemQuantity := resource.MustParse(tt.wantMem)
			require.Zero(t, wantMemQuantity.Cmp(req))
			require.Zero(t, wantMemQuantity.Cmp(lim))
		})
	}
}

func expBasicConfig() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:  "DD_RUNTIME_METRICS_ENABLED",
			Value: "true",
		},
		{
			Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
			Value: "true",
		},
		{
			Name:  "DD_TRACE_ENABLED",
			Value: "true",
		},
		{
			Name:  "DD_LOGS_INJECTION",
			Value: "true",
		},
	}
}

func injectAllEnvs() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:  "PYTHONPATH",
			Value: "/datadog-lib/",
		},
		{
			Name:  "RUBYOPT",
			Value: " -r/datadog-lib/auto_inject",
		},
		{
			Name:  "NODE_OPTIONS",
			Value: " --require=/datadog-lib/node_modules/dd-trace/init",
		},
		{
			Name:  "JAVA_TOOL_OPTIONS",
			Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
		},
		{
			Name:  "DD_DOTNET_TRACER_HOME",
			Value: "/datadog-lib",
		},
		{
			Name:  "CORECLR_ENABLE_PROFILING",
			Value: "1",
		},
		{
			Name:  "CORECLR_PROFILER",
			Value: "{846F5F1C-F9AE-4B07-969E-05C26BC060D8}",
		},
		{
			Name:  "CORECLR_PROFILER_PATH",
			Value: "/datadog-lib/Datadog.Trace.ClrProfiler.Native.so",
		},
		{
			Name:  "DD_TRACE_LOG_DIRECTORY",
			Value: "/datadog-lib/logs",
		},
	}
}

func TestInjectAutoInstrumentation(t *testing.T) {
	var (
		mockConfig model.Config
		wConfig    = func(k string, v any) func() {
			return func() {
				mockConfig.SetWithoutSource(k, v)
			}
		}

		enableAPMInstrumentation  = wConfig("apm_config.instrumentation.enabled", true)
		disableAPMInstrumentation = wConfig("apm_config.instrumentation.enabled", false)
		disableNamespaces         = func(ns ...string) func() {
			return wConfig("apm_config.instrumentation.disabled_namespaces", ns)
		}
		enabledNamespaces = func(ns ...string) func() {
			return wConfig("apm_config.instrumentation.enabled_namespaces", ns)
		}
		withLibVersions = func(vs map[string]string) func() {
			return wConfig("apm_config.instrumentation.lib_versions", vs)
		}
	)

	type funcs = []func()

	uuid := uuid.New().String()
	installTime := strconv.FormatInt(time.Now().Unix(), 10)

	defaultLibraries := map[string]string{
		"java":   "v1",
		"python": "v2",
		"ruby":   "v2",
		"dotnet": "v2",
		"js":     "v5",
	}

	defaultLibrariesFor := func(languages ...string) map[string]string {
		out := map[string]string{}
		for _, l := range languages {
			out[l] = defaultLibraries[l]
		}
		return out
	}

	tests := []struct {
		name                      string
		pod                       *corev1.Pod
		expectedEnvs              []corev1.EnvVar
		expectedInjectedLibraries map[string]string
		langDetectionDeployments  []common.MockDeployment
		wantErr                   bool
		setupConfig               funcs
	}{
		{
			name: "inject all with dotnet-profiler",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/all-lib.version":   "latest",
					"admission.datadoghq.com/all-lib.config.v1": `{"version":1,"runtime_metrics_enabled":true,"tracing_rate_limit":50,"tracing_sampling_rate":0.3}`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "true",
				},
				[]corev1.EnvVar{},
				"replicaset",
				"deployment-1234",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "50",
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.30",
				},
				{
					Name:  "PYTHONPATH",
					Value: "/datadog-lib/",
				},
				{
					Name:  "RUBYOPT",
					Value: " -r/datadog-lib/auto_inject",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
				{
					Name:  "DD_DOTNET_TRACER_HOME",
					Value: "/datadog-lib",
				},
				{
					Name:  "CORECLR_ENABLE_PROFILING",
					Value: "1",
				},
				{
					Name:  "CORECLR_PROFILER",
					Value: "{846F5F1C-F9AE-4B07-969E-05C26BC060D8}",
				},
				{
					Name:  "CORECLR_PROFILER_PATH",
					Value: "/datadog-lib/Datadog.Trace.ClrProfiler.Native.so",
				},
				{
					Name:  "DD_TRACE_LOG_DIRECTORY",
					Value: "/datadog-lib/logs",
				},
			},
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
		},
		{
			name: "inject all",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/all-lib.version":   "latest",
					"admission.datadoghq.com/all-lib.config.v1": `{"version":1,"runtime_metrics_enabled":true,"tracing_rate_limit":50,"tracing_sampling_rate":0.3}`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "true",
				},
				[]corev1.EnvVar{},
				"replicaset",
				"deployment-1234",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "50",
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.30",
				},
				{
					Name:  "PYTHONPATH",
					Value: "/datadog-lib/",
				},
				{
					Name:  "RUBYOPT",
					Value: " -r/datadog-lib/auto_inject",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
				{
					Name:  "DD_DOTNET_TRACER_HOME",
					Value: "/datadog-lib",
				},
				{
					Name:  "CORECLR_ENABLE_PROFILING",
					Value: "1",
				},
				{
					Name:  "CORECLR_PROFILER",
					Value: "{846F5F1C-F9AE-4B07-969E-05C26BC060D8}",
				},
				{
					Name:  "CORECLR_PROFILER_PATH",
					Value: "/datadog-lib/Datadog.Trace.ClrProfiler.Native.so",
				},
				{
					Name:  "DD_TRACE_LOG_DIRECTORY",
					Value: "/datadog-lib/logs",
				},
			},
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig:               funcs{},
		},
		{
			name: "inject library and all",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/all-lib.version":   "latest",
					"admission.datadoghq.com/all-lib.config.v1": `{"version":1,"runtime_metrics_enabled":true,"tracing_rate_limit":50,"tracing_sampling_rate":0.3}`,
					"admission.datadoghq.com/js-lib.version":    "v1.10",
					"admission.datadoghq.com/js-lib.config.v1":  `{"version":1,"tracing_sampling_rate":0.4}`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "true",
				},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "50",
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.40",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{"js": "v1.10"},
			wantErr:                   false,
		},
		{
			name: "inject library and all no library version",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/all-lib.version":   "latest",
					"admission.datadoghq.com/all-lib.config.v1": `{"version":1,"runtime_metrics_enabled":true,"tracing_rate_limit":50,"tracing_sampling_rate":0.3}`,
					"admission.datadoghq.com/js-lib.config.v1":  `{"version":1,"tracing_sampling_rate":0.4}`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "true",
				},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "50",
				},
				{
					Name:  "PYTHONPATH",
					Value: "/datadog-lib/",
				},
				{
					Name:  "RUBYOPT",
					Value: " -r/datadog-lib/auto_inject",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
				{
					Name:  "DD_DOTNET_TRACER_HOME",
					Value: "/datadog-lib",
				},
				{
					Name:  "CORECLR_ENABLE_PROFILING",
					Value: "1",
				},
				{
					Name:  "CORECLR_PROFILER",
					Value: "{846F5F1C-F9AE-4B07-969E-05C26BC060D8}",
				},
				{
					Name:  "CORECLR_PROFILER_PATH",
					Value: "/datadog-lib/Datadog.Trace.ClrProfiler.Native.so",
				},
				{
					Name:  "DD_TRACE_LOG_DIRECTORY",
					Value: "/datadog-lib/logs",
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.40",
				},
			},
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
		},
		{
			name: "inject all error - bad json",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					// TODO: we might not want to be injecting the libraries if the config is malformed
					"admission.datadoghq.com/all-lib.version":   "latest",
					"admission.datadoghq.com/all-lib.config.v1": `{"version":1,"runtime_metrics_enabled":true,`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "true",
				},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				{
					Name:  "PYTHONPATH",
					Value: "/datadog-lib/",
				},
				{
					Name:  "RUBYOPT",
					Value: " -r/datadog-lib/auto_inject",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
				{
					Name:  "DD_DOTNET_TRACER_HOME",
					Value: "/datadog-lib",
				},
				{
					Name:  "CORECLR_ENABLE_PROFILING",
					Value: "1",
				},
				{
					Name:  "CORECLR_PROFILER",
					Value: "{846F5F1C-F9AE-4B07-969E-05C26BC060D8}",
				},
				{
					Name:  "CORECLR_PROFILER_PATH",
					Value: "/datadog-lib/Datadog.Trace.ClrProfiler.Native.so",
				},
				{
					Name:  "DD_TRACE_LOG_DIRECTORY",
					Value: "/datadog-lib/logs",
				},
			},
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   true,
		},
		{
			name: "inject java",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/java-lib.version":   "latest",
					"admission.datadoghq.com/java-lib.config.v1": `{"version":1,"tracing_sampling_rate":0.3}`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "true",
				},
				[]corev1.EnvVar{},
				"replicaset",
				"deployment-1234",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.30",
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
			},
			expectedInjectedLibraries: map[string]string{"java": "latest"},
			wantErr:                   false,
		},
		{
			name: "inject python",
			pod:  common.FakePodWithParent("ns", map[string]string{"admission.datadoghq.com/python-lib.version": "latest", "admission.datadoghq.com/python-lib.config.v1": `{"version":1,"tracing_sampling_rate":0.3}`}, map[string]string{"admission.datadoghq.com/enabled": "true"}, []corev1.EnvVar{}, "", ""),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.30",
				},
				{
					Name:  "PYTHONPATH",
					Value: "/datadog-lib/",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{"python": "latest"},
			wantErr:                   false,
		},
		{
			name: "inject node",
			pod:  common.FakePodWithParent("ns", map[string]string{"admission.datadoghq.com/js-lib.version": "latest", "admission.datadoghq.com/js-lib.config.v1": `{"version":1,"tracing_sampling_rate":0.3}`}, map[string]string{"admission.datadoghq.com/enabled": "true"}, []corev1.EnvVar{}, "", ""),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.30",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{"js": "latest"},
			wantErr:                   false,
		},
		{
			name: "inject java bad json",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/java-lib.version":   "latest",
					"admission.datadoghq.com/java-lib.config.v1": `{"version":1,"runtime_metrics_enabled":true,`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "true",
				},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_lib_injection",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
			},
			expectedInjectedLibraries: map[string]string{"java": "latest"},
			wantErr:                   true,
		},
		{
			name: "inject with enabled false",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/java-lib.version":   "latest",
					"admission.datadoghq.com/java-lib.config.v1": `{"version":1,"runtime_metrics_enabled":true,`,
				},
				map[string]string{
					"admission.datadoghq.com/enabled": "false",
				},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{},
			wantErr:                   false,
		},
		{
			name: "Single Step Instrumentation: user configuration is respected",
			pod: common.FakePodWithParent("ns", map[string]string{}, map[string]string{}, []corev1.EnvVar{
				{
					Name:  "DD_SERVICE",
					Value: "user-deployment",
				},
				{
					Name:  "DD_TRACE_ENABLED",
					Value: "false",
				},
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "false",
				},
				{
					Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
					Value: "false",
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.5",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "2",
				},
				{
					Name:  "DD_LOGS_INJECTION",
					Value: "false",
				},
			}, "replicaset", "test-deployment-123"),
			expectedEnvs: append(injectAllEnvs(), []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				{
					Name:  "DD_SERVICE",
					Value: "user-deployment",
				},
				{
					Name:  "DD_TRACE_ENABLED",
					Value: "false",
				},
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "false",
				},
				{
					Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
					Value: "false",
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.5",
				},
				{
					Name:  "DD_TRACE_RATE_LIMIT",
					Value: "2",
				},
				{
					Name:  "DD_LOGS_INJECTION",
					Value: "false",
				},
			}...),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig:               funcs{enableAPMInstrumentation},
		},
		{
			name: "Single Step Instrumentation: disable with label",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{}, map[string]string{
					"admission.datadoghq.com/enabled": "false",
				}, []corev1.EnvVar{}, "replicaset", "test-deployment-123"),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{},
			wantErr:                   false,
			setupConfig:               funcs{enableAPMInstrumentation},
		},
		{
			name: "Single Step Instrumentation: default service name for ReplicaSet",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"replicaset",
				"test-deployment-123",
			),
			expectedEnvs: append(append(injectAllEnvs(), expBasicConfig()...), corev1.EnvVar{
				Name:  "DD_SERVICE",
				Value: "test-deployment",
			},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig:               funcs{enableAPMInstrumentation},
		},
		{
			name: "Single Step Instrumentation: default service name for StatefulSet",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"statefulset",
				"test-statefulset-123",
			),
			expectedEnvs: append(append(injectAllEnvs(), expBasicConfig()...), corev1.EnvVar{
				Name:  "DD_SERVICE",
				Value: "test-statefulset-123",
			},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig:               funcs{enableAPMInstrumentation},
		},
		{
			name: "Single Step Instrumentation: default service name (disabled)",
			pod:  common.FakePodWithParent("ns", map[string]string{}, map[string]string{}, []corev1.EnvVar{}, "replicaset", "test-deployment-123"),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{},
			wantErr:                   false,
			setupConfig:               []func(){disableAPMInstrumentation},
		},
		{
			name: "Single Step Instrumentation: disabled namespaces should not be instrumented",
			pod:  common.FakePodWithParent("ns", map[string]string{}, map[string]string{}, []corev1.EnvVar{}, "replicaset", "test-app-123"),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{},
			wantErr:                   false,
			setupConfig:               funcs{enableAPMInstrumentation, disableNamespaces("ns")},
		},
		{
			name: "Single Step Instrumentation: enabled namespaces should be instrumented",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{{Name: "DD_INSTRUMENTATION_INSTALL_TYPE", Value: "k8s_single_step"}},
				"replicaset", "test-app-123",
			),
			expectedEnvs: append(append(injectAllEnvs(), expBasicConfig()...),
				corev1.EnvVar{
					Name:  "DD_SERVICE",
					Value: "test-app",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig:               funcs{enableAPMInstrumentation, enabledNamespaces("ns")},
		},
		{
			name: "Single Step Instrumentation enabled and language annotation provided",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/js-lib.version":   "v1.10",
					"admission.datadoghq.com/js-lib.config.v1": `{"version":1,"tracing_sampling_rate":0.4}`,
				},
				map[string]string{},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_SAMPLE_RATE",
					Value: "0.40",
				},
				{
					Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_LOGS_INJECTION",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_ENABLED",
					Value: "true",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{"js": "v1.10"},
			wantErr:                   false,
			setupConfig:               funcs{enableAPMInstrumentation},
		},
		{
			name: "Single Step Instrumentation enabled with libVersions set",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_LOGS_INJECTION",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_ENABLED",
					Value: "true",
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
				{
					Name:  "PYTHONPATH",
					Value: "/datadog-lib/",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{"java": "v1.28.0", "python": "v2.5.1"},
			wantErr:                   false,
			setupConfig: funcs{
				enableAPMInstrumentation,
				withLibVersions(map[string]string{"java": "v1.28.0", "python": "v2.5.1"}),
			},
		},
		{
			name: "Single Step Instrumentation enabled, with language annotation and libVersions set",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{
					"admission.datadoghq.com/js-lib.version": "v1.10",
				},
				map[string]string{},
				[]corev1.EnvVar{},
				"",
				"",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_LOGS_INJECTION",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_ENABLED",
					Value: "true",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{"js": "v1.10"},
			wantErr:                   false,
			setupConfig: funcs{
				enableAPMInstrumentation,
				withLibVersions(map[string]string{"java": "v1.28.0", "python": "v2.5.1"}),
			},
		},
		{
			name: "Single Step Instrumentation enabled and language detection",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"replicaset",
				"test-app-689695b6cc",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_SERVICE",
					Value: "test-app",
				},
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_LOGS_INJECTION",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_ENABLED",
					Value: "true",
				},
				{
					Name:  "JAVA_TOOL_OPTIONS",
					Value: " -javaagent:/datadog-lib/dd-java-agent.jar -XX:OnError=/datadog-lib/java/continuousprofiler/tmp/dd_crash_uploader.sh -XX:ErrorFile=/datadog-lib/java/continuousprofiler/tmp/hs_err_pid_%p.log",
				},
				{
					Name:  "PYTHONPATH",
					Value: "/datadog-lib/",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: defaultLibrariesFor("python", "java"),
			langDetectionDeployments: []common.MockDeployment{
				{
					ContainerName:  "pod",
					DeploymentName: "test-app",
					Namespace:      "ns",
					Languages:      languageSetOf("python", "java"),
				},
			},
			wantErr: false,
			setupConfig: funcs{
				wConfig("admission_controller.auto_instrumentation.inject_auto_detected_libraries", true),
				enableAPMInstrumentation,
			},
		},
		{
			name: "Library annotation, Single Step Instrumentation with library pinned and language detection",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{"admission.datadoghq.com/js-lib.version": "v1.10"},
				map[string]string{},
				[]corev1.EnvVar{},
				"replicaset",
				"test-app-689695b6cc",
			),
			expectedEnvs: []corev1.EnvVar{
				{
					Name:  "DD_SERVICE",
					Value: "test-app",
				},
				{
					Name:  "DD_RUNTIME_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_HEALTH_METRICS_ENABLED",
					Value: "true",
				},
				{
					Name:  "DD_LOGS_INJECTION",
					Value: "true",
				},
				{
					Name:  "DD_TRACE_ENABLED",
					Value: "true",
				},
				{
					Name:  "NODE_OPTIONS",
					Value: " --require=/datadog-lib/node_modules/dd-trace/init",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
			},
			expectedInjectedLibraries: map[string]string{"js": "v1.10"},
			langDetectionDeployments: []common.MockDeployment{
				{
					ContainerName:  "pod",
					DeploymentName: "test-app",
					Namespace:      "ns",
					Languages:      languageSetOf("python", "java"),
				},
			},
			wantErr: false,
			setupConfig: funcs{
				wConfig("admission_controller.auto_instrumentation.inject_auto_detected_libraries", true),
				enableAPMInstrumentation,
				withLibVersions(map[string]string{"ruby": "v1.2.3"}),
			},
		},
		{
			name: "Single Step Instrumentation: enable ASM",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"replicaset", "test-app-123",
			),
			expectedEnvs: append(append(injectAllEnvs(), expBasicConfig()...),
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				corev1.EnvVar{
					Name:  "DD_SERVICE",
					Value: "test-app",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				corev1.EnvVar{
					Name:  "DD_APPSEC_ENABLED",
					Value: "true",
				},
			),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig: funcs{
				enableAPMInstrumentation,
				wConfig("admission_controller.auto_instrumentation.asm.enabled", true),
			},
		},
		{
			name: "Single Step Instrumentation: enable iast",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"replicaset", "test-app-123",
			),
			expectedEnvs: append(append(injectAllEnvs(), expBasicConfig()...),
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				corev1.EnvVar{
					Name:  "DD_SERVICE",
					Value: "test-app",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				corev1.EnvVar{
					Name:  "DD_IAST_ENABLED",
					Value: "true",
				},
			),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig: funcs{
				enableAPMInstrumentation,
				wConfig("admission_controller.auto_instrumentation.iast.enabled", true),
			},
		},
		{
			name: "Single Step Instrumentation: disable sca",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"replicaset", "test-app-123",
			),
			expectedEnvs: append(append(injectAllEnvs(), expBasicConfig()...),
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				corev1.EnvVar{
					Name:  "DD_SERVICE",
					Value: "test-app",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				corev1.EnvVar{
					Name:  "DD_APPSEC_SCA_ENABLED",
					Value: "false",
				},
			),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig: funcs{
				enableAPMInstrumentation,
				wConfig("admission_controller.auto_instrumentation.asm_sca.enabled", false),
			},
		},
		{
			name: "Single Step Instrumentation: enable profiling",
			pod: common.FakePodWithParent(
				"ns",
				map[string]string{},
				map[string]string{},
				[]corev1.EnvVar{},
				"replicaset", "test-app-123",
			),
			expectedEnvs: append(append(injectAllEnvs(), expBasicConfig()...),
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TYPE",
					Value: "k8s_single_step",
				},
				corev1.EnvVar{
					Name:  "DD_SERVICE",
					Value: "test-app",
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_TIME",
					Value: installTime,
				},
				corev1.EnvVar{
					Name:  "DD_INSTRUMENTATION_INSTALL_ID",
					Value: uuid,
				},
				corev1.EnvVar{
					Name:  "DD_PROFILING_ENABLED",
					Value: "auto",
				},
			),
			expectedInjectedLibraries: defaultLibraries,
			wantErr:                   false,
			setupConfig: funcs{
				enableAPMInstrumentation,
				wConfig("admission_controller.auto_instrumentation.profiling.enabled", "auto"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DD_INSTRUMENTATION_INSTALL_ID", uuid)
			t.Setenv("DD_INSTRUMENTATION_INSTALL_TIME", installTime)

			wmeta := common.FakeStoreWithDeployment(t, tt.langDetectionDeployments)

			mockConfig = configmock.New(t)
			if tt.setupConfig != nil {
				for _, f := range tt.setupConfig {
					f()
				}
			}

			// Need to create a new instance of the webhook to take into account
			// the config changes.
			UnsetWebhook()
			apmInstrumentationWebhook, errInitAPMInstrumentation := GetWebhook(wmeta)
			require.NoError(t, errInitAPMInstrumentation)

			_, err := apmInstrumentationWebhook.inject(tt.pod, "", fake.NewSimpleDynamicClient(scheme.Scheme))
			require.False(t, (err != nil) != tt.wantErr)

			container := tt.pod.Spec.Containers[0]
			for _, contEnv := range container.Env {
				found := false
				for _, expectEnv := range tt.expectedEnvs {
					if expectEnv.Name == contEnv.Name {
						found = true
						break
					}
				}
				if !found {
					require.Failf(t, "Unexpected env var injected in container", contEnv.Name)
				}
			}

			for _, expectEnv := range tt.expectedEnvs {
				found := false
				for _, contEnv := range container.Env {
					if expectEnv.Name == contEnv.Name {
						found = true
						break
					}
				}
				if !found {
					require.Failf(t, "Unexpected env var injected in container", expectEnv.Name)
				}
			}

			envCount := 0
			for _, contEnv := range container.Env {
				for _, expectEnv := range tt.expectedEnvs {
					if expectEnv.Name == contEnv.Name {
						require.Equal(t, expectEnv.Value, contEnv.Value)
						envCount++
						break
					}
				}
			}
			require.Equal(t, len(tt.expectedEnvs), envCount)

			initContainers := tt.pod.Spec.InitContainers

			require.Equal(t, len(tt.expectedInjectedLibraries), len(initContainers))
			for _, c := range initContainers {
				language := getLanguageFromInitContainerName(c.Name)
				require.Contains(t,
					tt.expectedInjectedLibraries, language,
					"unexpected injected language %s", language)
				require.Equal(t,
					tt.expectedInjectedLibraries[language], strings.Split(c.Image, ":")[1],
					"unexpected language version %s", language)
			}
		})
	}
}

func getLanguageFromInitContainerName(initContainerName string) string {
	trimmedSuffix := strings.TrimSuffix(initContainerName, "-init")
	return strings.TrimPrefix(trimmedSuffix, "datadog-lib-")
}

func TestShouldInject(t *testing.T) {
	var mockConfig model.Config
	tests := []struct {
		name        string
		pod         *corev1.Pod
		setupConfig func()
		want        bool
	}{
		{
			name:        "instrumentation on, no label",
			pod:         common.FakePodWithNamespaceAndLabel("ns", "", ""),
			setupConfig: func() { mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true) },
			want:        true,
		},
		{
			name:        "instrumentation on, label disabled",
			pod:         common.FakePodWithNamespaceAndLabel("ns", "admission.datadoghq.com/enabled", "false"),
			setupConfig: func() { mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true) },
			want:        false,
		},
		{
			name: "instrumentation on with disabled namespace, no label",
			pod:  common.FakePodWithNamespaceAndLabel("ns", "", ""),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.disabled_namespaces", []string{"ns"})
			},
			want: false,
		},
		{
			name: "instrumentation on with disabled namespace, no label",
			pod:  common.FakePodWithNamespaceAndLabel("ns2", "", ""),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.disabled_namespaces", []string{"ns"})
			},
			want: true,
		},
		{
			name: "instrumentation on with disabled namespace, disabled label",
			pod:  common.FakePodWithNamespaceAndLabel("ns", "admission.datadoghq.com/enabled", "false"),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.disabled_namespaces", []string{"ns"})
			},
			want: false,
		},
		{
			name: "instrumentation on with disabled namespace, label enabled",
			pod:  common.FakePodWithNamespaceAndLabel("ns", "admission.datadoghq.com/enabled", "true"),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.disabled_namespaces", []string{"ns"})
			},
			want: true,
		},
		{
			name:        "instrumentation off, label enabled",
			pod:         common.FakePodWithNamespaceAndLabel("ns", "admission.datadoghq.com/enabled", "true"),
			setupConfig: func() { mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", false) },
			want:        true,
		},
		{
			name:        "instrumentation off, no label",
			pod:         common.FakePodWithNamespaceAndLabel("ns", "", ""),
			setupConfig: func() { mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", false) },
			want:        false,
		},
		{
			name: "instrumentation off with enabled namespace, label enabled",
			pod:  common.FakePodWithNamespaceAndLabel("ns", "admission.datadoghq.com/enabled", "true"),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled_namespaces", []string{"ns"})
			},
			want: true,
		},
		{
			name: "instrumentation off with enabled namespace, label disabled",
			pod:  common.FakePodWithNamespaceAndLabel("ns", "admission.datadoghq.com/enabled", "false"),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", false)
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled_namespaces", []string{"ns"})
			},
			want: false,
		},
		{
			name: "instrumentation off with enabled namespace, no label",
			pod:  common.FakePodWithNamespaceAndLabel("ns", "", ""),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", false)
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled_namespaces", []string{"ns"})
			},
			want: false,
		},
		{
			name: "instrumentation on with enabled namespace, no label",
			pod:  common.FakePodWithNamespaceAndLabel("ns", "", ""),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled_namespaces", []string{"ns"})
			},
			want: true,
		},
		{
			name: "instrumentation on with enabled other namespace, no label",
			pod:  common.FakePodWithNamespaceAndLabel("ns2", "", ""),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled_namespaces", []string{"ns"})
			},
			want: false,
		},
		{
			name: "instrumentation on in kube-system namespace, no label",
			pod:  common.FakePodWithNamespaceAndLabel("kube-system", "", ""),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
			},
			want: false,
		},
		{
			name: "instrumentation on in default (datadog) namespace, no label",
			pod:  common.FakePodWithNamespaceAndLabel("default", "", ""),
			setupConfig: func() {
				mockConfig.SetWithoutSource("apm_config.instrumentation.enabled", true)
				mockConfig.SetWithoutSource("kube_resources_namespace", "default")
			},
			want: false,
		},
		{
			name:        "Mutate unlabelled, no label",
			pod:         common.FakePodWithLabel("", ""),
			setupConfig: func() { mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", true) },
			want:        true,
		},
		{
			name:        "Mutate unlabelled, label enabled",
			pod:         common.FakePodWithLabel("admission.datadoghq.com/enabled", "true"),
			setupConfig: func() { mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", true) },
			want:        true,
		},
		{
			name:        "Mutate unlabelled, label disabled",
			pod:         common.FakePodWithLabel("admission.datadoghq.com/enabled", "false"),
			setupConfig: func() { mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", true) },
			want:        false,
		},
		{
			name:        "no Mutate unlabelled, no label",
			pod:         common.FakePodWithLabel("", ""),
			setupConfig: func() { mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false) },
			want:        false,
		},
		{
			name:        "no Mutate unlabelled, label enabled",
			pod:         common.FakePodWithLabel("admission.datadoghq.com/enabled", "true"),
			setupConfig: func() { mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false) },
			want:        true,
		},
		{
			name:        "no Mutate unlabelled, label disabled",
			pod:         common.FakePodWithLabel("admission.datadoghq.com/enabled", "false"),
			setupConfig: func() { mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false) },
			want:        false,
		},
		{
			name:        "no Mutate unlabelled, label disabled",
			pod:         common.FakePodWithLabel("admission.datadoghq.com/enabled", "false"),
			setupConfig: func() { mockConfig.SetWithoutSource("admission_controller.mutate_unlabelled", false) },
			want:        false,
		},
	}
	wmeta := fxutil.Test[workloadmeta.Component](t, core.MockBundle(), workloadmetafxmock.MockModule(), fx.Supply(workloadmeta.NewParams()))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockConfig = configmock.New(t)
			tt.setupConfig()

			// Need to create a new instance of the webhook to take into account
			// the config changes.
			UnsetWebhook()
			webhook, errInitAPMInstrumentation := GetWebhook(wmeta)
			require.NoError(t, errInitAPMInstrumentation)

			require.Equal(t, tt.want, ShouldInject(tt.pod, webhook.wmeta), "expected ShouldInject() to be %t", tt.want)
			require.Equal(t, tt.want, webhook.isPodEligible(tt.pod), "expected webhook.isPodEligible() to be %t", tt.want)
		})
	}
}

func languageSetOf(languages ...string) util.LanguageSet {
	set := util.LanguageSet{}
	for _, l := range languages {
		_ = set.Add(util.Language(l))
	}
	return set
}
