// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gcm "cloud.google.com/go/monitoring/apiv3/v2"
	gcmpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator"
	monitoringv1 "github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator/apis/monitoring/v1"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/cert"
	kyaml "sigs.k8s.io/yaml"
)

func TestRuleEvaluation(t *testing.T) {
	tctx := newOperatorContext(t)

	cert, key, err := cert.GenerateSelfSignedCertKey("test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("rule evaluator create alertmanager secrets", tctx.subtest(func(ctx context.Context, t *OperatorContext) {
		testCreateAlertmanagerSecrets(ctx, t, cert, key)
	}))
	t.Run("rule evaluator operatorconfig", tctx.subtest(testRuleEvaluatorOperatorConfig))
	t.Run("rule evaluator secrets", tctx.subtest(func(ctx context.Context, t *OperatorContext) {
		testRuleEvaluatorSecrets(ctx, t, cert, key)
	}))
	t.Run("rule evaluator config", tctx.subtest(testRuleEvaluatorConfig))
	t.Run("rule generation", tctx.subtest(testRulesGeneration))
	t.Run("rule evaluator deploy", tctx.subtest(testRuleEvaluatorDeployment))

	if !skipGCM {
		t.Log("Waiting rule results to become readable")
		t.Run("check rule metrics", tctx.subtest(testValidateRuleEvaluationMetrics))
	}
}

// testRuleEvaluatorOperatorConfig ensures an OperatorConfig can be deployed
// that contains rule-evaluator configuration.
func testRuleEvaluatorOperatorConfig(ctx context.Context, t *OperatorContext) {
	// Setup TLS secret selectors.
	certSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: "alertmanager-tls",
		},
		Key: "cert",
	}

	keySecret := certSecret.DeepCopy()
	keySecret.Key = "key"

	opCfg := &monitoringv1.OperatorConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: operator.NameOperatorConfig,
		},
		Rules: monitoringv1.RuleEvaluatorSpec{
			ExternalLabels: map[string]string{
				"external_key": "external_val",
			},
			QueryProjectID: projectID,
			Alerting: monitoringv1.AlertingSpec{
				Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
					{
						Name:       "test-am",
						Namespace:  t.namespace,
						Port:       intstr.IntOrString{IntVal: 19093},
						Timeout:    "30s",
						APIVersion: "v2",
						PathPrefix: "/test",
						Scheme:     "https",
						Authorization: &monitoringv1.Authorization{
							Type: "Bearer",
							Credentials: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "alertmanager-authorization",
								},
								Key: "token",
							},
						},
						TLS: &monitoringv1.TLSConfig{
							Cert: &monitoringv1.SecretOrConfigMap{
								Secret: certSecret,
							},
							KeySecret: keySecret,
						},
					},
				},
			},
		},
	}
	if gcpServiceAccount != "" {
		opCfg.Rules.Credentials = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: "user-gcp-service-account",
			},
			Key: "key.json",
		}
	}
	_, err := t.operatorClient.MonitoringV1().OperatorConfigs(t.pubNamespace).Create(ctx, opCfg, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create rules operatorconfig: %s", err)
	}
}

func testRuleEvaluatorSecrets(ctx context.Context, t *OperatorContext, cert, key []byte) {
	// Verify contents but without the GCP SA credentials file to not leak secrets in tests logs.
	// Whether the contents were copied correctly is implicitly verified by the credentials working.
	want := map[string][]byte{
		fmt.Sprintf("secret_%s_alertmanager-tls_cert", t.pubNamespace):            cert,
		fmt.Sprintf("secret_%s_alertmanager-tls_key", t.pubNamespace):             key,
		fmt.Sprintf("secret_%s_alertmanager-authorization_token", t.pubNamespace): []byte("auth-bearer-password"),
	}
	err := wait.Poll(1*time.Second, 1*time.Minute, func() (bool, error) {
		secret, err := t.kubeClient.CoreV1().Secrets(t.namespace).Get(ctx, operator.RulesSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("get secret: %w", err)
		}
		delete(secret.Data, fmt.Sprintf("secret_%s_user-gcp-service-account_key.json", t.pubNamespace))

		if diff := cmp.Diff(want, secret.Data); diff != "" {
			return false, fmt.Errorf("unexpected configuration (-want, +got): %s", diff)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("failed waiting for generated rule-evaluator config: %s", err)
	}

}

func testRuleEvaluatorConfig(ctx context.Context, t *OperatorContext) {
	replace := func(s string) string {
		return strings.NewReplacer(
			"{namespace}", t.namespace, "{pubNamespace}", t.pubNamespace,
		).Replace(s)
	}

	want := map[string]string{
		"config.yaml": replace(`global:
    external_labels:
        external_key: external_val
alerting:
    alertmanagers:
        - follow_redirects: true
          enable_http2: true
          scheme: http
          timeout: 10s
          api_version: v2
          static_configs:
            - targets:
                - alertmanager.{namespace}:9093
        - authorization:
            type: Bearer
            credentials_file: /etc/secrets/secret_{pubNamespace}_alertmanager-authorization_token
          tls_config:
            cert_file: /etc/secrets/secret_{pubNamespace}_alertmanager-tls_cert
            key_file: /etc/secrets/secret_{pubNamespace}_alertmanager-tls_key
            insecure_skip_verify: false
          follow_redirects: true
          enable_http2: true
          scheme: https
          path_prefix: /test
          timeout: 30s
          api_version: v2
          relabel_configs:
            - source_labels: [__meta_kubernetes_endpoints_name]
              regex: test-am
              action: keep
            - source_labels: [__address__]
              regex: (.+):\d+
              target_label: __address__
              replacement: $1:19093
              action: replace
          kubernetes_sd_configs:
            - role: endpoints
              kubeconfig_file: ""
              follow_redirects: true
              enable_http2: true
              namespaces:
                own_namespace: false
                names:
                    - {namespace}
rule_files:
    - /etc/rules/*.yaml
`),
	}
	err := wait.Poll(1*time.Second, 1*time.Minute, func() (bool, error) {
		cm, err := t.kubeClient.CoreV1().ConfigMaps(t.namespace).Get(ctx, "rule-evaluator", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("get configmap: %w", err)
		}
		if diff := cmp.Diff(want, cm.Data); diff != "" {
			return false, fmt.Errorf("unexpected configuration (-want, +got): %s", diff)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("failed waiting for generated rule-evaluator config: %s", err)
	}

}

func testRuleEvaluatorDeployment(ctx context.Context, t *OperatorContext) {
	err := wait.Poll(1*time.Second, 1*time.Minute, func() (bool, error) {
		deploy, err := t.kubeClient.AppsV1().Deployments(t.namespace).Get(ctx, "rule-evaluator", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("get deployment: %w", err)
		}
		// When not using GCM, we check the available replicas rather than ready ones
		// as the rule-evaluator's readyness probe does check for connectivity to GCM.
		if skipGCM {
			// TODO(pintohutch): stub CTS API during e2e tests to remove
			// this conditional.
			if *deploy.Spec.Replicas != deploy.Status.UpdatedReplicas {
				return false, nil
			}
		} else if *deploy.Spec.Replicas != deploy.Status.ReadyReplicas {
			return false, nil
		}

		// Assert we have the expected annotations.
		wantedAnnotations := map[string]string{
			"components.gke.io/component-name":               "managed_prometheus",
			"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
		}
		if diff := cmp.Diff(wantedAnnotations, deploy.Spec.Template.Annotations); diff != "" {
			return false, fmt.Errorf("unexpected annotations (-want, +got): %s", diff)
		}

		for _, c := range deploy.Spec.Template.Spec.Containers {
			if c.Name != "evaluator" {
				continue
			}
			// We're mainly interested in the dynamic flags but checking the entire set including
			// the static ones is ultimately simpler.
			wantArgs := []string{
				fmt.Sprintf("--export.label.project-id=%q", projectID),
				fmt.Sprintf("--export.label.location=%q", location),
				fmt.Sprintf("--export.label.cluster=%q", cluster),
				fmt.Sprintf("--query.project-id=%q", projectID),
			}
			if gcpServiceAccount != "" {
				filepath := fmt.Sprintf("/etc/secrets/secret_%s_user-gcp-service-account_key.json", t.pubNamespace)
				wantArgs = append(wantArgs,
					fmt.Sprintf("--export.credentials-file=%q", filepath),
					fmt.Sprintf("--query.credentials-file=%q", filepath),
				)
			}

			if diff := cmp.Diff(strings.Join(wantArgs, " "), getEnvVar(c.Env, "EXTRA_ARGS")); diff != "" {
				return false, fmt.Errorf("unexpected flags (-want, +got): %s", diff)
			}
			return true, nil
		}
		return false, errors.New("no container with name evaluator found")
	})
	if err != nil {
		t.Fatalf("failed waiting for generated rule-evaluator deployment: %s", err)
	}
}

func testRulesGeneration(_ context.Context, t *OperatorContext) {
	replace := strings.NewReplacer(
		"{project_id}", projectID,
		"{cluster}", cluster,
		"{location}", location,
		"{namespace}", t.namespace,
	).Replace

	// Create multiple rules in the cluster and expect their scoped equivalents
	// to be present in the generated rule file.
	content := replace(`
apiVersion: monitoring.googleapis.com/v1alpha1
kind: GlobalRules
metadata:
  name: global-rules
spec:
  groups:
  - name: group-1
    rules:
    - record: bar
      expr: avg(up)
      labels:
        flavor: test
`)
	var globalRules monitoringv1.GlobalRules
	if err := kyaml.Unmarshal([]byte(content), &globalRules); err != nil {
		t.Fatal(err)
	}
	globalRules.OwnerReferences = t.ownerReferences

	if _, err := t.operatorClient.MonitoringV1().GlobalRules().Create(context.TODO(), &globalRules, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	content = replace(`
apiVersion: monitoring.googleapis.com/v1alpha1
kind: ClusterRules
metadata:
  name: {namespace}-cluster-rules
spec:
  groups:
  - name: group-1
    rules:
    - record: foo
      expr: sum(up)
      labels:
        flavor: test
`)
	var clusterRules monitoringv1.ClusterRules
	if err := kyaml.Unmarshal([]byte(content), &clusterRules); err != nil {
		t.Fatal(err)
	}
	clusterRules.OwnerReferences = t.ownerReferences

	if _, err := t.operatorClient.MonitoringV1().ClusterRules().Create(context.TODO(), &clusterRules, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// TODO(freinartz): Instantiate structs directly rather than templating strings.
	content = `
apiVersion: monitoring.googleapis.com/v1alpha1
kind: Rules
metadata:
  name: rules
spec:
  groups:
  - name: group-1
    rules:
    - alert: Bar
      expr: avg(down) > 1
      annotations:
        description: "bar avg down"
      labels:
        flavor: test
    - record: always_one
      expr: vector(1)
`
	var rules monitoringv1.Rules
	if err := kyaml.Unmarshal([]byte(content), &rules); err != nil {
		t.Fatal(err)
	}
	if _, err := t.operatorClient.MonitoringV1().Rules(t.namespace).Create(context.TODO(), &rules, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		replace("globalrules__global-rules.yaml"): replace(`groups:
    - name: group-1
      rules:
        - record: bar
          expr: avg(up)
          labels:
            flavor: test
`),
		replace("clusterrules__{namespace}-cluster-rules.yaml"): replace(`groups:
    - name: group-1
      rules:
        - record: foo
          expr: sum(up{cluster="{cluster}",location="{location}",project_id="{project_id}"})
          labels:
            cluster: {cluster}
            flavor: test
            location: {location}
            project_id: {project_id}
`),
		replace("rules__{namespace}__rules.yaml"): replace(`groups:
    - name: group-1
      rules:
        - alert: Bar
          expr: avg(down{cluster="{cluster}",location="{location}",namespace="{namespace}",project_id="{project_id}"}) > 1
          labels:
            cluster: {cluster}
            flavor: test
            location: {location}
            namespace: {namespace}
            project_id: {project_id}
          annotations:
            description: bar avg down
        - record: always_one
          expr: vector(1)
          labels:
            cluster: {cluster}
            location: {location}
            namespace: {namespace}
            project_id: {project_id}
`),
	}

	var diff string

	err := wait.Poll(1*time.Second, time.Minute, func() (bool, error) {
		cm, err := t.kubeClient.CoreV1().ConfigMaps(t.namespace).Get(context.TODO(), "rules-generated", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("get ConfigMap: %w", err)
		}
		// The operator observes Rules across all namespaces. For the purpose of this test we drop
		// all outputs from the result that aren't in the expected set.
		for name := range cm.Data {
			if _, ok := want[name]; !ok {
				delete(cm.Data, name)
			}
		}
		diff = cmp.Diff(want, cm.Data)
		return diff == "", nil
	})
	if err != nil {
		t.Errorf("diff (-want, +got): %s", diff)
		t.Fatalf("failed waiting for generated rules: %s", err)
	}
}

func testValidateRuleEvaluationMetrics(ctx context.Context, t *OperatorContext) {
	// The project, location and cluster name in which we look for the metric data must
	// be provided by the user. Check this only in this test so tests that don't need these
	// flags can still be run without them.
	if projectID == "" {
		t.Fatalf("no project specified (--project-id flag)")
	}
	if location == "" {
		t.Fatalf("no location specified (--location flag)")
	}
	if cluster == "" {
		t.Fatalf("no cluster name specified (--cluster flag)")
	}

	// Wait for metric data to show up in Cloud Monitoring.
	metricClient, err := gcm.NewMetricClient(ctx)
	if err != nil {
		t.Fatalf("Create GCM metric client: %s", err)
	}
	defer metricClient.Close()

	err = wait.Poll(1*time.Second, 3*time.Minute, func() (bool, error) {
		now := time.Now()

		// Validate the majority of labels being set correctly by filtering along them.
		iter := metricClient.ListTimeSeries(ctx, &gcmpb.ListTimeSeriesRequest{
			Name: fmt.Sprintf("projects/%s", projectID),
			Filter: fmt.Sprintf(`
				resource.type = "prometheus_target" AND
				resource.labels.project_id = "%s" AND
				resource.labels.location = "%s" AND
				resource.labels.cluster = "%s" AND
				resource.labels.namespace = "%s" AND
				metric.type = "prometheus.googleapis.com/always_one/gauge"
				`,
				projectID, location, cluster, t.namespace,
			),
			Interval: &gcmpb.TimeInterval{
				EndTime:   timestamppb.New(now),
				StartTime: timestamppb.New(now.Add(-10 * time.Second)),
			},
		})
		series, err := iter.Next()
		if err == iterator.Done {
			t.Logf("No data, retrying...")
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("querying metrics failed: %w", err)
		}
		if len(series.Points) == 0 {
			return false, errors.New("unexpected zero points in result series")
		}
		// We expect exactly one result.
		series, err = iter.Next()
		if err != iterator.Done {
			return false, fmt.Errorf("expected iterator to be done but series %v: %w", series, err)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Waiting for rule metrics to appear in Cloud Monitoring failed: %s", err)
	}
}