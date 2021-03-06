/*
Copyright 2018 The Knative Authors

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

package route

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	// Inject the informers this controller depends on.
	_ "knative.dev/pkg/client/injection/kube/informers/core/v1/service/fake"
	fakeservingclient "knative.dev/serving/pkg/client/injection/client/fake"
	_ "knative.dev/serving/pkg/client/injection/informers/networking/v1alpha1/certificate/fake"
	fakeingressinformer "knative.dev/serving/pkg/client/injection/informers/networking/v1alpha1/ingress/fake"
	fakecfginformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/configuration/fake"
	fakerevisioninformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/revision/fake"
	fakerouteinformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/route/fake"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/ptr"
	"knative.dev/pkg/system"
	netv1alpha1 "knative.dev/serving/pkg/apis/networking/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	"knative.dev/serving/pkg/gc"
	"knative.dev/serving/pkg/network"
	"knative.dev/serving/pkg/reconciler/route/config"
	"knative.dev/serving/pkg/reconciler/route/domains"

	. "knative.dev/pkg/reconciler/testing"
	. "knative.dev/serving/pkg/testing/v1"
)

const (
	testNamespace       = "test"
	defaultDomainSuffix = "test-domain.dev"
	prodDomainSuffix    = "prod-domain.com"
)

func getTestRouteWithTrafficTargets(trafficTarget RouteOption) *v1.Route {
	return Route(testNamespace, "test-route", WithRouteLabel(map[string]string{"route": "test-route"}), trafficTarget)
}

func getTestRevision(name string) *v1.Revision {
	return getTestRevisionWithCondition(name, apis.Condition{
		Type:   v1.RevisionConditionReady,
		Status: corev1.ConditionTrue,
		Reason: "ServiceReady",
	})
}

func getTestRevisionWithCondition(name string, cond apis.Condition) *v1.Revision {
	return &v1.Revision{
		ObjectMeta: metav1.ObjectMeta{
			SelfLink:  fmt.Sprintf("/apis/serving/v1/namespaces/test/revisions/%s", name),
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: v1.RevisionSpec{
			PodSpec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Image: "test-image",
				}},
			},
		},
		Status: v1.RevisionStatus{
			ServiceName: name,
			Status: duckv1.Status{
				Conditions: duckv1.Conditions{cond},
			},
		},
	}
}

func getTestConfiguration() *v1.Configuration {
	return &v1.Configuration{
		ObjectMeta: metav1.ObjectMeta{
			SelfLink:  "/apis/serving/v1/namespaces/test/revisiontemplates/test-config",
			Name:      "test-config",
			Namespace: testNamespace,
		},
		Spec: v1.ConfigurationSpec{
			Template: v1.RevisionTemplateSpec{
				Spec: v1.RevisionSpec{
					PodSpec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image: "test-image",
						}},
					},
				},
			},
		},
	}
}

func getTestRevisionForConfig(config *v1.Configuration) *v1.Revision {
	rev := &v1.Revision{
		ObjectMeta: metav1.ObjectMeta{
			SelfLink:  "/apis/serving/v1/namespaces/test/revisions/p-deadbeef",
			Name:      "p-deadbeef",
			Namespace: testNamespace,
			Labels: map[string]string{
				serving.ConfigurationLabelKey: config.Name,
			},
		},
		Spec: *config.Spec.GetTemplate().Spec.DeepCopy(),
		Status: v1.RevisionStatus{
			ServiceName: "p-deadbeef",
		},
	}
	rev.Status.MarkResourcesAvailableTrue()
	rev.Status.MarkContainerHealthyTrue()
	return rev
}

func newTestSetup(t *testing.T, opts ...reconcilerOption) (
	ctx context.Context,
	informers []controller.Informer,
	ctrl *controller.Impl,
	configMapWatcher *configmap.ManualWatcher,
	cf context.CancelFunc) {

	ctx, cf, informers = SetupFakeContextWithCancel(t)
	configMapWatcher = &configmap.ManualWatcher{Namespace: system.Namespace()}
	ctrl = newControllerWithClock(ctx, configMapWatcher, system.RealClock{}, opts...)

	cms := []*corev1.ConfigMap{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.DomainConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			defaultDomainSuffix: "",
			prodDomainSuffix:    "selector:\n  app: prod",
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      network.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      gc.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{},
	}}

	for _, cfg := range cms {
		configMapWatcher.OnChange(cfg)
	}
	return
}

func getRouteIngressFromClient(ctx context.Context, t *testing.T, route *v1.Route) *netv1alpha1.Ingress {
	opts := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			serving.RouteLabelKey:          route.Name,
			serving.RouteNamespaceLabelKey: route.Namespace,
		}).String(),
	}
	ingresses, err := fakeservingclient.Get(ctx).NetworkingV1alpha1().Ingresses(route.Namespace).List(opts)
	if err != nil {
		t.Errorf("Ingress.Get(%v) = %v", opts, err)
	}

	if len(ingresses.Items) != 1 {
		t.Errorf("Ingress.Get(%v), expect 1 instance, but got %d", opts, len(ingresses.Items))
	}

	return &ingresses.Items[0]
}
func getCertificateFromClient(t *testing.T, ctx context.Context, desired *netv1alpha1.Certificate) *netv1alpha1.Certificate {
	created, err := fakeservingclient.Get(ctx).NetworkingV1alpha1().Certificates(desired.Namespace).Get(desired.Name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Certificates(%s).Get(%s) = %v", desired.Namespace, desired.Name, err)
	}
	return created
}

func addResourcesToInformers(t *testing.T, ctx context.Context, route *v1.Route) {
	t.Helper()

	ns := route.Namespace

	route, err := fakeservingclient.Get(ctx).ServingV1().Routes(ns).Get(route.Name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Route.Get(%v) = %v", route.Name, err)
	}
	fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)

	if ci := getRouteIngressFromClient(ctx, t, route); ci != nil {
		fakeingressinformer.Get(ctx).Informer().GetIndexer().Add(ci)
	}
	ingress := getRouteIngressFromClient(ctx, t, route)
	fakeingressinformer.Get(ctx).Informer().GetIndexer().Add(ingress)
}

// Test the only revision in the route is in Reserve (inactive) serving status.
func TestCreateRouteForOneReserveRevision(t *testing.T) {
	ctx, _, ctl, _, cf := newTestSetup(t)
	defer cf()

	fakeRecorder := controller.GetEventRecorder(ctx).(*record.FakeRecorder)

	// An inactive revision
	rev := getTestRevision("test-rev")
	rev.Status.MarkActiveFalse("NoTraffic", "no message")

	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(rev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(rev)

	// A route targeting the revision
	route := getTestRouteWithTrafficTargets(WithSpecTraffic(v1.TrafficTarget{
		RevisionName:      "test-rev",
		ConfigurationName: "test-config",
		Percent:           ptr.Int64(100),
	}))
	fakeservingclient.Get(ctx).ServingV1().Routes(testNamespace).Create(route)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)

	ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))

	ci := getRouteIngressFromClient(ctx, t, route)

	// Check labels
	expectedLabels := map[string]string{
		serving.RouteLabelKey:          route.Name,
		serving.RouteNamespaceLabelKey: route.Namespace,
		"route":                        "test-route",
	}
	if diff := cmp.Diff(expectedLabels, ci.Labels); diff != "" {
		t.Errorf("Unexpected label diff (-want +got): %v", diff)
	}

	domain := strings.Join([]string{route.Name, route.Namespace, defaultDomainSuffix}, ".")
	expectedSpec := netv1alpha1.IngressSpec{
		TLS: []netv1alpha1.IngressTLS{},
		Rules: []netv1alpha1.IngressRule{{
			Hosts: []string{
				"test-route.test.svc.cluster.local",
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  "test-rev",
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
		}, {
			Hosts: []string{
				domain,
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  "test-rev",
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
		}},
	}
	if diff := cmp.Diff(expectedSpec, ci.Spec); diff != "" {
		t.Errorf("Unexpected rule spec diff (-want +got): %s", diff)
	}

	// Update ingress loadbalancer to trigger placeholder service creation.
	ci.Status = netv1alpha1.IngressStatus{
		LoadBalancer: &netv1alpha1.LoadBalancerStatus{
			Ingress: []netv1alpha1.LoadBalancerIngressStatus{{
				DomainInternal: "test-domain",
			}},
		},
	}
	fakeingressinformer.Get(ctx).Informer().GetIndexer().Update(ci)
	ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))

	// Look for the events. Events are delivered asynchronously so we need to use
	// hooks here. Each hook tests for a specific event.
	select {
	case got := <-fakeRecorder.Events:
		const want = `Normal Created Created placeholder service "test-route"`
		if got != want {
			t.Errorf("<-Events = %s, wanted %s", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Error("timed out waiting for expected events.")
	}
	select {
	case got := <-fakeRecorder.Events:
		const wantPrefix = `Normal Created Created Ingress`
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("<-Events = %s, wanted prefix %s", got, wantPrefix)
		}
	case <-time.After(3 * time.Second):
		t.Error("timed out waiting for expected events.")
	}
}

func TestCreateRouteWithMultipleTargets(t *testing.T) {
	ctx, informers, ctl, _, cf := newTestSetup(t)
	wicb, err := controller.RunInformers(ctx.Done(), informers...)
	if err != nil {
		t.Fatalf("Error starting informers: %v", err)
	}
	defer func() {
		cf()
		wicb()
	}()
	// A standalone revision
	rev := getTestRevision("test-rev")
	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(rev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(rev)

	// A configuration and associated revision. Normally the revision would be
	// created by the configuration reconciler.
	config := getTestConfiguration()
	cfgrev := getTestRevisionForConfig(config)
	config.Status.SetLatestCreatedRevisionName(cfgrev.Name)
	config.Status.SetLatestReadyRevisionName(cfgrev.Name)
	fakeservingclient.Get(ctx).ServingV1().Configurations(testNamespace).Create(config)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakecfginformer.Get(ctx).Informer().GetIndexer().Add(config)
	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(cfgrev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(cfgrev)

	// A route targeting both the config and standalone revision.
	route := getTestRouteWithTrafficTargets(WithSpecTraffic(
		v1.TrafficTarget{
			ConfigurationName: config.Name,
			Percent:           ptr.Int64(90),
		}, v1.TrafficTarget{
			RevisionName: rev.Name,
			Percent:      ptr.Int64(10),
		}))
	fakeservingclient.Get(ctx).ServingV1().Routes(testNamespace).Create(route)
	// Since Reconcile looks in the lister, we need to add it to the informer.
	fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)

	ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))

	ci := getRouteIngressFromClient(ctx, t, route)
	domain := strings.Join([]string{route.Name, route.Namespace, defaultDomainSuffix}, ".")
	expectedSpec := netv1alpha1.IngressSpec{
		TLS: []netv1alpha1.IngressTLS{},
		Rules: []netv1alpha1.IngressRule{{
			Hosts: []string{
				"test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 90,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 10,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				domain,
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 90,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 10,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}},
	}

	if diff := cmp.Diff(expectedSpec, ci.Spec); diff != "" {
		t.Errorf("Unexpected rule spec diff (-want +got): %v", diff)
	}
}

// Test one out of multiple target revisions is in Reserve serving state.
func TestCreateRouteWithOneTargetReserve(t *testing.T) {
	ctx, _, ctl, _, cf := newTestSetup(t)
	defer cf()
	// A standalone inactive revision
	rev := getTestRevision("test-rev")
	rev.Status.MarkActiveFalse("NoTraffic", "no message")

	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(rev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(rev)

	// A configuration and associated revision. Normally the revision would be
	// created by the configuration reconciler.
	config := getTestConfiguration()
	cfgrev := getTestRevisionForConfig(config)
	config.Status.SetLatestCreatedRevisionName(cfgrev.Name)
	config.Status.SetLatestReadyRevisionName(cfgrev.Name)
	fakeservingclient.Get(ctx).ServingV1().Configurations(testNamespace).Create(config)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakecfginformer.Get(ctx).Informer().GetIndexer().Add(config)
	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(cfgrev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(cfgrev)

	// A route targeting both the config and standalone revision
	route := getTestRouteWithTrafficTargets(WithSpecTraffic(
		v1.TrafficTarget{
			ConfigurationName: config.Name,
			Percent:           ptr.Int64(90),
		}, v1.TrafficTarget{
			RevisionName:      rev.Name,
			ConfigurationName: "test-config",
			Percent:           ptr.Int64(10),
		}))
	fakeservingclient.Get(ctx).ServingV1().Routes(testNamespace).Create(route)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)

	ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))

	ci := getRouteIngressFromClient(ctx, t, route)
	domain := strings.Join([]string{route.Name, route.Namespace, defaultDomainSuffix}, ".")
	expectedSpec := netv1alpha1.IngressSpec{
		TLS: []netv1alpha1.IngressTLS{},
		Rules: []netv1alpha1.IngressRule{{
			Hosts: []string{
				"test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 90,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 10,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				domain,
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 90,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Status.ServiceName,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 10,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}},
	}
	if diff := cmp.Diff(expectedSpec, ci.Spec); diff != "" {
		t.Errorf("Unexpected rule spec diff (-want +got): %v", diff)
	}
}

func TestCreateRouteWithDuplicateTargets(t *testing.T) {
	ctx, _, ctl, _, cf := newTestSetup(t)
	defer cf()

	// A standalone revision
	rev := getTestRevision("test-rev")
	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(rev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(rev)

	// A configuration and associated revision. Normally the revision would be
	// created by the configuration reconciler.
	config := getTestConfiguration()
	cfgrev := getTestRevisionForConfig(config)
	config.Status.SetLatestCreatedRevisionName(cfgrev.Name)
	config.Status.SetLatestReadyRevisionName(cfgrev.Name)
	fakeservingclient.Get(ctx).ServingV1().Configurations(testNamespace).Create(config)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakecfginformer.Get(ctx).Informer().GetIndexer().Add(config)
	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(cfgrev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(cfgrev)

	// A route with duplicate targets. These will be deduped.
	route := getTestRouteWithTrafficTargets(WithSpecTraffic(
		v1.TrafficTarget{
			ConfigurationName: "test-config",
			Percent:           ptr.Int64(30),
		}, v1.TrafficTarget{
			ConfigurationName: "test-config",
			Percent:           ptr.Int64(20),
		}, v1.TrafficTarget{
			RevisionName: "test-rev",
			Percent:      ptr.Int64(10),
		}, v1.TrafficTarget{
			RevisionName: "test-rev",
			Percent:      ptr.Int64(5),
		}, v1.TrafficTarget{
			Tag:          "test-revision-1",
			RevisionName: "test-rev",
			Percent:      ptr.Int64(10),
		}, v1.TrafficTarget{
			Tag:          "test-revision-1",
			RevisionName: "test-rev",
			Percent:      ptr.Int64(10),
		}, v1.TrafficTarget{
			Tag:          "test-revision-2",
			RevisionName: "test-rev",
			Percent:      ptr.Int64(15),
		}))
	fakeservingclient.Get(ctx).ServingV1().Routes(testNamespace).Create(route)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)

	ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))

	ci := getRouteIngressFromClient(ctx, t, route)
	domain := strings.Join([]string{route.Name, route.Namespace, defaultDomainSuffix}, ".")
	expectedSpec := netv1alpha1.IngressSpec{
		TLS: []netv1alpha1.IngressTLS{},
		Rules: []netv1alpha1.IngressRule{{
			Hosts: []string{
				"test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				domain,
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}, {
			Hosts: []string{
				"test-revision-1-test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      "test-rev",
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				"test-revision-1-test-route.test.test-domain.dev",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      "test-rev",
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}, {
			Hosts: []string{
				"test-revision-2-test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      "test-rev",
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				"test-revision-2-test-route.test.test-domain.dev",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      "test-rev",
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}},
	}

	if diff := cmp.Diff(expectedSpec, ci.Spec); diff != "" {
		fmt.Printf("%+v\n", ci.Spec)
		t.Errorf("Unexpected rule spec diff (-want +got): %v", diff)
	}
}

func TestCreateRouteWithNamedTargets(t *testing.T) {
	ctx, _, ctl, _, cf := newTestSetup(t)
	defer cf()
	// A standalone revision
	rev := getTestRevision("test-rev")
	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(rev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(rev)

	// A configuration and associated revision. Normally the revision would be
	// created by the configuration reconciler.
	config := getTestConfiguration()
	cfgrev := getTestRevisionForConfig(config)
	config.Status.SetLatestCreatedRevisionName(cfgrev.Name)
	config.Status.SetLatestReadyRevisionName(cfgrev.Name)
	fakeservingclient.Get(ctx).ServingV1().Configurations(testNamespace).Create(config)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakecfginformer.Get(ctx).Informer().GetIndexer().Add(config)
	fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Create(cfgrev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(cfgrev)

	// A route targeting both the config and standalone revision with named
	// targets
	route := getTestRouteWithTrafficTargets(WithSpecTraffic(
		v1.TrafficTarget{
			Tag:          "foo",
			RevisionName: "test-rev",
			Percent:      ptr.Int64(50),
		}, v1.TrafficTarget{
			Tag:               "bar",
			ConfigurationName: "test-config",
			Percent:           ptr.Int64(50),
		}))

	fakeservingclient.Get(ctx).ServingV1().Routes(testNamespace).Create(route)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)

	ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))

	ci := getRouteIngressFromClient(ctx, t, route)
	domain := strings.Join([]string{route.Name, route.Namespace, defaultDomainSuffix}, ".")
	expectedSpec := netv1alpha1.IngressSpec{
		TLS: []netv1alpha1.IngressTLS{},
		Rules: []netv1alpha1.IngressRule{{
			Hosts: []string{
				"test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				domain,
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}, {
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 50,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}, {
			Hosts: []string{
				"bar-test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				"bar-test-route.test.test-domain.dev",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      cfgrev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  cfgrev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}, {
			Hosts: []string{
				"foo-test-route.test.svc.cluster.local",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityClusterLocal,
		}, {
			Hosts: []string{
				"foo-test-route.test.test-domain.dev",
			},
			HTTP: &netv1alpha1.HTTPIngressRuleValue{
				Paths: []netv1alpha1.HTTPIngressPath{{
					Splits: []netv1alpha1.IngressBackendSplit{{
						IngressBackend: netv1alpha1.IngressBackend{
							ServiceNamespace: testNamespace,
							ServiceName:      rev.Name,
							ServicePort:      intstr.FromInt(80),
						},
						Percent: 100,
						AppendHeaders: map[string]string{
							"Knative-Serving-Revision":  rev.Name,
							"Knative-Serving-Namespace": testNamespace,
						},
					}},
				}},
			},
			Visibility: netv1alpha1.IngressVisibilityExternalIP,
		}},
	}

	if diff := cmp.Diff(expectedSpec, ci.Spec); diff != "" {
		fmt.Printf("%+v\n", ci.Spec)
		t.Errorf("Unexpected rule spec diff (-want +got): %v", diff)
	}
}

func TestUpdateDomainConfigMap(t *testing.T) {
	ctx, _, ctl, watcher, cf := newTestSetup(t)
	defer cf()
	route := getTestRouteWithTrafficTargets(WithSpecTraffic(v1.TrafficTarget{}))
	routeClient := fakeservingclient.Get(ctx).ServingV1().Routes(route.Namespace)

	// Create a route.
	fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)
	routeClient.Create(route)
	ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))
	addResourcesToInformers(t, ctx, route)

	route.ObjectMeta.Labels = map[string]string{"app": "prod"}

	// Test changes in domain config map. Routes should get updated appropriately.
	expectations := []struct {
		apply                func()
		expectedDomainSuffix string
	}{{
		expectedDomainSuffix: prodDomainSuffix,
		apply:                func() {},
	}, {
		expectedDomainSuffix: "mytestdomain.com",
		apply: func() {
			domainConfig := corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DomainConfigName,
					Namespace: system.Namespace(),
				},
				Data: map[string]string{
					defaultDomainSuffix: "",
					"mytestdomain.com":  "selector:\n  app: prod",
				},
			}
			watcher.OnChange(&domainConfig)
		},
	}, {
		expectedDomainSuffix: "newdefault.net",
		apply: func() {
			domainConfig := corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DomainConfigName,
					Namespace: system.Namespace(),
				},
				Data: map[string]string{
					"newdefault.net":   "",
					"mytestdomain.com": "selector:\n  app: prod",
				},
			}
			watcher.OnChange(&domainConfig)
			route.Labels = make(map[string]string)
		},
	}, {
		// When no domain with an open selector is specified, we fallback
		// on the default of example.com.
		expectedDomainSuffix: config.DefaultDomain,
		apply: func() {
			domainConfig := corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DomainConfigName,
					Namespace: system.Namespace(),
				},
				Data: map[string]string{
					"mytestdomain.com": "selector:\n  app: prod",
				},
			}
			watcher.OnChange(&domainConfig)
			route.Labels = make(map[string]string)
		},
	}}

	for _, expectation := range expectations {
		t.Run(expectation.expectedDomainSuffix, func(t *testing.T) {
			expectation.apply()
			fakerouteinformer.Get(ctx).Informer().GetIndexer().Add(route)
			routeClient.Update(route)
			ctl.Reconciler.Reconcile(context.Background(), KeyOrDie(route))
			addResourcesToInformers(t, ctx, route)

			route, _ = routeClient.Get(route.Name, metav1.GetOptions{})
			expectedDomain := fmt.Sprintf("%s.%s.%s", route.Name, route.Namespace, expectation.expectedDomainSuffix)
			if route.Status.URL.Host != expectedDomain {
				t.Errorf("Expected domain %q but saw %q", expectedDomain, route.Status.URL.Host)
			}
		})
	}
}

func TestGlobalResyncOnUpdateDomainConfigMap(t *testing.T) {
	// Test changes in domain config map. Routes should get updated appropriately.
	// We're expecting exactly one route modification per config-map change.
	tests := []struct {
		doThings             func(*configmap.ManualWatcher)
		expectedDomainSuffix string
	}{{
		expectedDomainSuffix: prodDomainSuffix,
		doThings:             func(*configmap.ManualWatcher) {}, // The update will still happen: status will be updated to match the route labels
	}, {
		expectedDomainSuffix: "mytestdomain.com",
		doThings: func(watcher *configmap.ManualWatcher) {
			domainConfig := corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DomainConfigName,
					Namespace: system.Namespace(),
				},
				Data: map[string]string{
					defaultDomainSuffix: "",
					"mytestdomain.com":  "selector:\n  app: prod",
				},
			}
			watcher.OnChange(&domainConfig)
		},
	}, {
		expectedDomainSuffix: "newprod.net",
		doThings: func(watcher *configmap.ManualWatcher) {
			domainConfig := corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DomainConfigName,
					Namespace: system.Namespace(),
				},
				Data: map[string]string{
					defaultDomainSuffix: "",
					"newprod.net":       "selector:\n  app: prod",
				},
			}
			watcher.OnChange(&domainConfig)
		},
	}, {
		expectedDomainSuffix: defaultDomainSuffix,
		doThings: func(watcher *configmap.ManualWatcher) {
			domainConfig := corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DomainConfigName,
					Namespace: system.Namespace(),
				},
				Data: map[string]string{
					defaultDomainSuffix: "",
				},
			}
			watcher.OnChange(&domainConfig)
		},
	}}

	for _, test := range tests {
		test := test
		t.Run(test.expectedDomainSuffix, func(t *testing.T) {
			ctx, informers, ctrl, watcher, cf := newTestSetup(t)

			grp := errgroup.Group{}

			servingClient := fakeservingclient.Get(ctx)
			h := NewHooks()

			// Check for Ingress created as a signal that syncHandler ran
			h.OnUpdate(&servingClient.Fake, "routes", func(obj runtime.Object) HookResult {
				rt := obj.(*v1.Route)
				t.Logf("route updated: %q", rt.Name)

				expectedDomain := fmt.Sprintf("%s.%s.%s", rt.Name, rt.Namespace, test.expectedDomainSuffix)
				if rt.Status.URL.Host != expectedDomain {
					t.Logf("Expected domain %q but saw %q", expectedDomain, rt.Status.URL.Host)
					return HookIncomplete
				}

				return HookComplete
			})

			waitInformers, err := controller.RunInformers(ctx.Done(), informers...)
			if err != nil {
				t.Fatalf("Failed to start informers: %v", err)
			}
			defer func() {
				cf()
				if err := grp.Wait(); err != nil {
					t.Errorf("Wait() = %v", err)
				}
				waitInformers()
			}()

			if err := watcher.Start(ctx.Done()); err != nil {
				t.Fatalf("failed to start configuration manager: %v", err)
			}

			grp.Go(func() error { return ctrl.Run(1, ctx.Done()) })

			// Create a route.
			route := getTestRouteWithTrafficTargets(WithSpecTraffic(v1.TrafficTarget{}))
			route.Labels = map[string]string{"app": "prod"}

			servingClient.ServingV1().Routes(route.Namespace).Create(route)

			test.doThings(watcher)

			if err := h.WaitForHooks(3 * time.Second); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRouteDomain(t *testing.T) {
	route := Route("default", "myapp", WithRouteLabel(map[string]string{"route": "myapp"}), WithRouteAnnotation(map[string]string{"sub": "mysub"}))
	ctx := context.Background()
	cfg := ReconcilerTestConfig(false)
	ctx = config.ToContext(ctx, cfg)

	tests := []struct {
		Name     string
		Template string
		Pass     bool
		Expected string
	}{{
		Name:     "Default",
		Template: "{{.Name}}.{{.Namespace}}.{{.Domain}}",
		Pass:     true,
		Expected: "myapp.default.example.com",
	}, {
		Name:     "Dash",
		Template: "{{.Name}}-{{.Namespace}}.{{.Domain}}",
		Pass:     true,
		Expected: "myapp-default.example.com",
	}, {
		Name:     "Short",
		Template: "{{.Name}}.{{.Domain}}",
		Pass:     true,
		Expected: "myapp.example.com",
	}, {
		Name:     "SuperShort",
		Template: "{{.Name}}",
		Pass:     true,
		Expected: "myapp",
	}, {
		Name:     "Annotations",
		Template: `{{.Name}}.{{ index .Annotations "sub"}}.{{.Domain}}`,
		Pass:     true,
		Expected: "myapp.mysub.example.com",
	}, {
		// This cannot get through our validation, but verify we handle errors.
		Name:     "BadVarName",
		Template: "{{.Name}}.{{.NNNamespace}}.{{.Domain}}",
		Pass:     false,
		Expected: "",
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			cfg.Network.DomainTemplate = test.Template

			res, err := domains.DomainNameFromTemplate(ctx, route.ObjectMeta, route.Name)

			if test.Pass != (err == nil) {
				t.Fatal("DomainNameFromTemplate supposed to fail but didn't")
			}
			if got, want := res, test.Expected; got != want {
				t.Errorf("DomainNameFromTemplate = %q, want: %q", res, test.Expected)
			}
		})
	}
}
