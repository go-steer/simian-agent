// Copyright 2026 Google LLC
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

package kubestate

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
)

const testNS = "arena-1"

func manifest(kind string, spec map[string]any) simian.FaultManifest {
	return simian.FaultManifest{
		UID:          "01K4ZQ8XABCDEF",
		Source:       simian.SourceAutonomous,
		Engine:       simian.EngineKubeState,
		APIVersion:   APIVersion,
		ResourceKind: kind,
		Spec:         spec,
		Targets:      []simian.TargetRef{{Namespace: testNS}},
		Duration:     5 * time.Minute,
	}
}

func newTestDriver(objs ...runtime.Object) *Driver {
	cs := fake.NewSimpleClientset(objs...)
	standInDeploymentController(cs)
	d := New(cs)
	d.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	// Short, because the stand-in below reports availability on the first poll.
	// A test that wants the timeout path raises the bar instead — see
	// TestRolloutStuckRefusesToWedgeARevisionThatNeverCameUp.
	d.RolloutSettle = time.Second
	return d
}

// standInDeploymentController makes a fake clientset report a Deployment as
// fully rolled out.
//
// The fake runs no controllers, so status stays at the zero value forever and
// RolloutStuck's finisher — which waits for the first revision to be available
// before wedging the second — would time out against every test in this file.
// Stubbing the wait out instead would leave the one step this kind cannot be
// built without unexercised, so the wait runs for real and this supplies the
// only thing it reads.
//
// Deliberately unconditional: it says the Deployment is available, not that it
// stays available after the wedge. Nothing here simulates a rollout failing,
// because nothing here should — that the wedged revision never becomes ready is
// a fact about Kubernetes, and it is verified against a live cluster rather
// than asserted against a fake.
func standInDeploymentController(cs *fake.Clientset) {
	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		obj, err := cs.Tracker().Get(
			schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			get.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		dep, ok := obj.(*appsv1.Deployment)
		if !ok {
			return false, nil, nil
		}
		dep = dep.DeepCopy()
		want := int32(1)
		if dep.Spec.Replicas != nil {
			want = *dep.Spec.Replicas
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas = want
		dep.Status.ReadyReplicas = want
		dep.Status.UpdatedReplicas = want
		dep.Status.AvailableReplicas = want
		return true, dep, nil
	})
}

func getDeployment(t *testing.T, d *Driver, engineUIDStr string) *appsv1.Deployment {
	t.Helper()
	ns, name, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		t.Fatalf("decodeEngineUID(%q): %v", engineUIDStr, err)
	}
	dep, err := d.clientset.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	return dep
}

// getBundle returns the metadata of every object the fault created, found the
// way Clear finds them: by the bundle label, across every type this engine
// manages.
func getBundle(t *testing.T, d *Driver, engineUIDStr string) []metav1.ObjectMeta {
	t.Helper()
	ns, name, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		t.Fatalf("decodeEngineUID(%q): %v", engineUIDStr, err)
	}
	var out []metav1.ObjectMeta
	for _, r := range managedResources {
		objs, err := r.list(context.Background(), d.clientset, ns, BundleLabel+"="+name)
		if err != nil {
			t.Fatalf("list %s: %v", r.plural, err)
		}
		out = append(out, objs...)
	}
	return out
}

// podTemplateOf returns the pod template the bundle carries, wherever it lives.
// Most kinds put it on a Deployment; JobFailure puts it on a Job.
func podTemplateOf(t *testing.T, d *Driver, engineUIDStr string) corev1.PodSpec {
	t.Helper()
	ns, name, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		t.Fatalf("decodeEngineUID(%q): %v", engineUIDStr, err)
	}
	if dep, err := d.clientset.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
		return dep.Spec.Template.Spec
	}
	job, err := d.clientset.BatchV1().Jobs(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("bundle %s/%s carries no pod template: %v", ns, name, err)
	}
	return job.Spec.Template.Spec
}

func TestEngine(t *testing.T) {
	if got := newTestDriver().Engine(); got != simian.EngineKubeState {
		t.Errorf("Engine()=%q, want %q", got, simian.EngineKubeState)
	}
}

// Every kind must apply from an empty spec. The planner is an LLM; a kind that
// needs a field filled in correctly is a kind that will sometimes be injected
// wrong, and a fault that failed to inject is indistinguishable from one the
// SUT absorbed unless someone reads the audit log.
func TestApplyEveryKindWithEmptySpec(t *testing.T) {
	for _, kind := range Kinds() {
		t.Run(kind, func(t *testing.T) {
			d := newTestDriver()
			uid, err := d.Apply(context.Background(), manifest(kind, nil))
			if err != nil {
				t.Fatalf("Apply(%s, nil spec): %v", kind, err)
			}
			objs := getBundle(t, d, uid)
			if len(objs) == 0 {
				t.Fatalf("Apply(%s) created nothing", kind)
			}
			for _, obj := range objs {
				if got := obj.Labels[KindLabel]; got != kind {
					t.Errorf("%s: kind label = %q, want %q", obj.Name, got, kind)
				}
			}
			pod := podTemplateOf(t, d, uid)
			if len(pod.Containers) != 1 {
				t.Fatalf("want exactly one container, got %d", len(pod.Containers))
			}
			if pod.Containers[0].Name != containerName {
				t.Errorf("container name = %q, want %q", pod.Containers[0].Name, containerName)
			}
		})
	}
}

func TestApplyNamesWorkloadFromFaultUID(t *testing.T) {
	d := newTestDriver()
	m := manifest(KindImageUnresolvable, nil)
	uid, err := d.Apply(context.Background(), m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The gate computes the same name before Apply runs. If these ever
	// disagree the probe selects nothing, the Settle never passes, and a
	// landed fault is rolled back and reported as having done nothing.
	want := testNS + "/" + catalog.KubeStateWorkloadName(KindImageUnresolvable, "", m.UID)
	if uid != want {
		t.Errorf("engineUID = %q, want %q", uid, want)
	}
}

func TestApplyHonorsRequestedNameAndStillSuffixes(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindContainerExitLoop, map[string]any{"name": "checkout"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dep := getDeployment(t, d, uid)
	if !strings.HasPrefix(dep.Name, "checkout-") {
		t.Errorf("name %q does not start with the requested base", dep.Name)
	}
	// Two faults of the same kind in one namespace must not collide on the
	// second Create.
	if dep.Name == "checkout" {
		t.Errorf("name %q was not suffixed", dep.Name)
	}
}

// The identifying labels belong on the Deployment, where the reaper lists them,
// and nowhere near the pods, which are what a subject under evaluation reads.
func TestApplyKeepsSimianLabelsOffThePodTemplate(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindUnschedulable, nil))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dep := getDeployment(t, d, uid)
	for _, l := range []string{ManagedLabel, FaultUIDLabel, KindLabel} {
		if _, ok := dep.Labels[l]; !ok {
			t.Errorf("Deployment is missing label %q", l)
		}
		if _, ok := dep.Spec.Template.Labels[l]; ok {
			t.Errorf("pod template carries %q; a pod wearing Simian's name answers the question the rig is asking", l)
		}
	}
	if got := dep.Spec.Template.Labels["app"]; got != dep.Name {
		t.Errorf("pod label app=%q, want %q", got, dep.Name)
	}
	// The default gate selects on app=<name>; the Deployment's own selector
	// has to agree or the Deployment will not adopt the pods the gate reads.
	if got := dep.Spec.Selector.MatchLabels["app"]; got != dep.Name {
		t.Errorf("selector app=%q, want %q", got, dep.Name)
	}
}

func TestApplyStampsExpiryFromDuration(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindImageUnresolvable, nil))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dep := getDeployment(t, d, uid)
	got, ok := parseExpiry(dep.Annotations)
	if !ok {
		t.Fatalf("no expiry annotation on %s", dep.Name)
	}
	want := d.Now().Add(5 * time.Minute).UTC()
	if !got.Equal(want) {
		t.Errorf("expiry = %s, want %s", got, want)
	}
}

// A zero duration must stamp nothing. Stamping now+0 would make the very next
// reaper sweep delete a fault that is still live.
func TestApplyWithoutDurationStampsNoExpiry(t *testing.T) {
	d := newTestDriver()
	m := manifest(KindImageUnresolvable, nil)
	m.Duration = 0
	uid, err := d.Apply(context.Background(), m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := parseExpiry(getDeployment(t, d, uid).Annotations); ok {
		t.Error("stamped an expiry for a zero-duration fault")
	}
}

func TestApplyReplicas(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindContainerExitLoop, map[string]any{"replicas": float64(3)}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dep := getDeployment(t, d, uid)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want 3", dep.Spec.Replicas)
	}
}

func TestApplyRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*simian.FaultManifest)
		wantSub string
	}{
		{
			name:    "no targets",
			mutate:  func(m *simian.FaultManifest) { m.Targets = nil },
			wantSub: "no targets",
		},
		{
			name:    "target without namespace",
			mutate:  func(m *simian.FaultManifest) { m.Targets = []simian.TargetRef{{Name: "x"}} },
			wantSub: "no namespace",
		},
		{
			name:    "unsupported kind",
			mutate:  func(m *simian.FaultManifest) { m.ResourceKind = "NodeUnready" },
			wantSub: "unsupported kind",
		},
		{
			name:    "mutate mode not implemented",
			mutate:  func(m *simian.FaultManifest) { m.Spec = map[string]any{"mode": ModeMutate} },
			wantSub: "not implemented",
		},
		{
			name:    "unknown mode",
			mutate:  func(m *simian.FaultManifest) { m.Spec = map[string]any{"mode": "destroy"} },
			wantSub: "not recognised",
		},
		{
			name:    "zero replicas",
			mutate:  func(m *simian.FaultManifest) { m.Spec = map[string]any{"replicas": float64(0)} },
			wantSub: "between 1 and 20",
		},
		{
			// An LLM-chosen replica count is a blast radius nothing else in
			// the pipeline bounds, and anything past math.MaxInt32 also
			// overflows the int32 the API takes.
			name:    "absurd replica count",
			mutate:  func(m *simian.FaultManifest) { m.Spec = map[string]any{"replicas": float64(math.MaxInt32 + 1)} },
			wantSub: "between 1 and 20",
		},
		{
			name:    "name that is not a DNS label",
			mutate:  func(m *simian.FaultManifest) { m.Spec = map[string]any{"name": "Not A Name"} },
			wantSub: "valid workload name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver()
			m := manifest(KindImageUnresolvable, nil)
			tc.mutate(&m)
			_, err := d.Apply(context.Background(), m)
			if err == nil {
				t.Fatalf("Apply succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantSub)
			}
			// Nothing may be left behind by a rejected apply.
			list, err := d.clientset.AppsV1().Deployments(testNS).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(list.Items) != 0 {
				t.Errorf("rejected apply created %d deployment(s)", len(list.Items))
			}
		})
	}
}

func TestApplyExplicitSynthesizeModeIsAccepted(t *testing.T) {
	d := newTestDriver()
	if _, err := d.Apply(context.Background(), manifest(KindImageUnresolvable, map[string]any{"mode": ModeSynthesize})); err != nil {
		t.Fatalf("Apply with explicit mode=synthesize: %v", err)
	}
}

func TestApplyReturnsCreateError(t *testing.T) {
	d := newTestDriver()
	cs := d.clientset.(*fake.Clientset)
	cs.PrependReactor("create", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "x", errors.New("nope"))
	})
	_, err := d.Apply(context.Background(), manifest(KindImageUnresolvable, nil))
	if err == nil {
		t.Fatal("Apply succeeded despite a Forbidden create")
	}
	if !strings.Contains(err.Error(), "create deployment") {
		t.Errorf("error %q does not say what failed", err)
	}
}

func TestClearRoundTrip(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindMemoryLimitSqueeze, nil))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := d.Clear(context.Background(), uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	ns, name, _ := decodeEngineUID(uid)
	_, err = d.clientset.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("after Clear, Get returned %v, want NotFound", err)
	}
}

// Clear runs from the lease expiry path, the reaper and an explicit MCP call,
// and any two of those can race. A second Clear must not turn a recovered
// fault into an error the operator has to read.
func TestClearIsIdempotent(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindMemoryLimitSqueeze, nil))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for i := range 2 {
		if err := d.Clear(context.Background(), uid); err != nil {
			t.Fatalf("Clear #%d: %v", i+1, err)
		}
	}
}

func TestClearRejectsMalformedEngineUID(t *testing.T) {
	if err := newTestDriver().Clear(context.Background(), "no-slash-here"); err == nil {
		t.Fatal("Clear accepted a malformed engineUID")
	}
}

func managedDeployment(ns, name string, ann map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   ns,
		Labels:      map[string]string{ManagedLabel: "true"},
		Annotations: ann,
	}}
}

func TestReapExpired(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)

	d := newTestDriver(
		managedDeployment(testNS, "expired", map[string]string{ExpiryAnnotation: past}),
		managedDeployment(testNS, "live", map[string]string{ExpiryAnnotation: future}),
		managedDeployment(testNS, "unstamped", nil),
		managedDeployment(testNS, "garbled", map[string]string{ExpiryAnnotation: "the day before yesterday"}),
		// Not ours: no managed label, expired stamp. Must survive.
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "someone-elses", Namespace: testNS,
			Annotations: map[string]string{ExpiryAnnotation: past},
		}},
		// Ours and expired, but in a namespace nobody asked us to sweep.
		managedDeployment("other-ns", "out-of-scope", map[string]string{ExpiryAnnotation: past}),
	)

	cleared, err := d.ReapExpired(context.Background(), []string{testNS}, now)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if len(cleared) != 1 || cleared[0] != engineUID(testNS, "expired") {
		t.Fatalf("cleared = %v, want [%s]", cleared, engineUID(testNS, "expired"))
	}

	for _, want := range []struct{ ns, name string }{
		{testNS, "live"},
		// An unparseable or missing stamp is treated as absent: an object we
		// cannot prove is expired is left where an operator will see it.
		{testNS, "unstamped"},
		{testNS, "garbled"},
		{testNS, "someone-elses"},
		{"other-ns", "out-of-scope"},
	} {
		if _, err := d.clientset.AppsV1().Deployments(want.ns).Get(context.Background(), want.name, metav1.GetOptions{}); err != nil {
			t.Errorf("%s/%s was deleted: %v", want.ns, want.name, err)
		}
	}
	if _, err := d.clientset.AppsV1().Deployments(testNS).Get(context.Background(), "expired", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expired deployment survived the sweep: %v", err)
	}
}

// One bad namespace must not stop the sweep: the whole point of the reaper is
// that leftover chaos gets cleaned up even when something else is broken.
func TestReapExpiredKeepsGoingAfterAnError(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	d := newTestDriver(managedDeployment("good-ns", "expired", map[string]string{ExpiryAnnotation: past}))
	cs := d.clientset.(*fake.Clientset)
	cs.PrependReactor("list", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "bad-ns" {
			return true, nil, errors.New("boom")
		}
		return false, nil, nil
	})

	cleared, err := d.ReapExpired(context.Background(), []string{"bad-ns", "good-ns"}, now)
	if err == nil {
		t.Fatal("ReapExpired hid the list failure")
	}
	if len(cleared) != 1 || cleared[0] != engineUID("good-ns", "expired") {
		t.Errorf("cleared = %v, want [%s]", cleared, engineUID("good-ns", "expired"))
	}
}

func TestCatalog(t *testing.T) {
	entries, err := newTestDriver().Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != len(Kinds()) {
		t.Fatalf("Catalog returned %d entries, want %d", len(entries), len(Kinds()))
	}
	for _, e := range entries {
		if e.Engine != simian.EngineKubeState {
			t.Errorf("%s: engine = %q", e.ResourceKind, e.Engine)
		}
		if e.APIVersion != APIVersion {
			t.Errorf("%s: apiVersion = %q, want %q", e.ResourceKind, e.APIVersion, APIVersion)
		}
		if e.Description == "" {
			t.Errorf("%s: no description", e.ResourceKind)
		}
		if e.SpecTemplate == "" {
			t.Errorf("%s: no spec template", e.ResourceKind)
		}
		// The planner reads the template and nothing else. If the duration
		// advice is missing it will ask for 30s, the backoff state will not
		// have appeared by then, and the gate will roll back a fault that
		// landed.
		if !strings.Contains(e.SpecTemplate, "at least 3m") {
			t.Errorf("%s: spec template does not tell the planner how long to run it", e.ResourceKind)
		}
		if e.BlastRadiusTier != simian.TierNamespace {
			t.Errorf("%s: tier = %q, want %q", e.ResourceKind, e.BlastRadiusTier, simian.TierNamespace)
		}
		if e.EfficacyGate == "" {
			t.Errorf("%s: no efficacy gate advertised", e.ResourceKind)
		}
	}
}

// The driver's kind list, the constants pkg/catalog spells out by hand, and the
// tier table all have to agree. They cannot import each other — the driver
// imports the catalog for tier and gate lookup — so the only thing keeping them
// in sync is this test.
func TestKindsMatchCatalogConstants(t *testing.T) {
	inCatalog := []string{
		catalog.KubeStateBackendCrashLoop,
		catalog.KubeStateCertExpiry,
		catalog.KubeStateContainerExitLoop,
		catalog.KubeStateDependencyStall,
		catalog.KubeStateImageUnresolvable,
		catalog.KubeStateJobFailure,
		catalog.KubeStateMemoryLimitSqueeze,
		catalog.KubeStateNoOp,
		catalog.KubeStatePDBGridlock,
		catalog.KubeStateRolloutStuck,
		catalog.KubeStateSelectorDrift,
		catalog.KubeStateUnboundClaim,
		catalog.KubeStateUnschedulable,
	}
	kinds := Kinds()
	if len(kinds) != len(inCatalog) {
		t.Fatalf("driver has %d kinds, catalog names %d", len(kinds), len(inCatalog))
	}
	for i, k := range kinds {
		if k != inCatalog[i] {
			t.Errorf("kind[%d] = %q, catalog has %q", i, k, inCatalog[i])
		}
	}
	for _, k := range kinds {
		if !Supports(k) {
			t.Errorf("Supports(%q) = false", k)
		}
		if catalog.KubeStateWorkloadName(k, "", "abc") == "" {
			t.Errorf("catalog has no default workload name for %q", k)
		}
		if len(catalog.DefaultProbes(manifest(k, nil))) == 0 {
			t.Errorf("kind %q has no default efficacy gate; a fault nothing verifies can be reported as applied when it did nothing", k)
		}
	}
	if Supports("NodeUnready") {
		t.Error("Supports() claims a kind this driver does not build")
	}
}

// --- bundles ---

// Every object in a bundle has to carry the bundle label, because that label
// is the whole of Clear's memory. The driver records nothing: it finds the
// objects again from the engineUID and this label, so an object that misses it
// is an object that survives the fault it belongs to.
func TestEveryObjectInABundleIsLabelledWithIt(t *testing.T) {
	for _, kind := range Kinds() {
		t.Run(kind, func(t *testing.T) {
			d := newTestDriver()
			uid, err := d.Apply(context.Background(), manifest(kind, nil))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			_, name, _ := decodeEngineUID(uid)
			for _, obj := range getBundle(t, d, uid) {
				if got := obj.Labels[BundleLabel]; got != name {
					t.Errorf("%s: bundle label = %q, want %q", obj.Name, got, name)
				}
				if got := obj.Labels[ManagedLabel]; got != "true" {
					t.Errorf("%s: managed label = %q, want true", obj.Name, got)
				}
				if got := obj.Labels[FaultUIDLabel]; got != "01K4ZQ8XABCDEF" {
					t.Errorf("%s: fault-uid label = %q", obj.Name, got)
				}
				if _, ok := obj.Annotations[ExpiryAnnotation]; !ok {
					t.Errorf("%s: no expiry annotation; the reaper cannot clean it up after a restart", obj.Name)
				}
			}
		})
	}
}

func TestClearRemovesEveryObjectInTheBundle(t *testing.T) {
	// The two kinds that are more than one object, and the one that is more
	// than one *type* — a Clear that only knew about Deployments would leave
	// the Service and the claim behind, and the next scenario in that arena
	// would inherit them.
	for _, kind := range []string{KindSelectorDrift, KindUnboundClaim, KindJobFailure} {
		t.Run(kind, func(t *testing.T) {
			d := newTestDriver()
			uid, err := d.Apply(context.Background(), manifest(kind, nil))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if n := len(getBundle(t, d, uid)); n == 0 {
				t.Fatalf("Apply created nothing")
			}
			if err := d.Clear(context.Background(), uid); err != nil {
				t.Fatalf("Clear: %v", err)
			}
			if left := getBundle(t, d, uid); len(left) != 0 {
				t.Errorf("after Clear, %d object(s) remain: %v", len(left), left)
			}
		})
	}
}

// A bundle whose second object cannot be created must not leave the first one
// standing. A failed Apply is never leased, so nothing will ever call Clear for
// it: the rollback here is the only thing between a Forbidden create and a
// broken workload nobody is tracking.
func TestAPartiallyCreatedBundleIsRolledBack(t *testing.T) {
	d := newTestDriver()
	cs := d.clientset.(*fake.Clientset)
	cs.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "services"}, "x", errors.New("nope"))
	})
	_, err := d.Apply(context.Background(), manifest(KindSelectorDrift, nil))
	if err == nil {
		t.Fatal("Apply succeeded despite a Forbidden create")
	}
	if !strings.Contains(err.Error(), "create service") {
		t.Errorf("error %q does not name what failed", err)
	}
	deps, lerr := d.clientset.AppsV1().Deployments(testNS).List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("list deployments: %v", lerr)
	}
	if len(deps.Items) != 0 {
		t.Errorf("the Deployment created before the failure is still there: %v", deps.Items[0].Name)
	}
}

// The reaper clears leases, and a lease is per fault. A bundle of three
// expired objects is one fault that has ended, not three — reporting it three
// times would have the executor clear a lease twice against nothing.
func TestReapExpiredReportsOneBundleOnce(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindUnboundClaim, nil))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := len(getBundle(t, d, uid)); n < 2 {
		t.Fatalf("bundle has %d objects; this test needs more than one", n)
	}
	// An hour past the five-minute duration the manifest asked for.
	cleared, err := d.ReapExpired(context.Background(), []string{testNS}, time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if len(cleared) != 1 || cleared[0] != uid {
		t.Errorf("cleared = %v, want exactly [%s]", cleared, uid)
	}
	if left := getBundle(t, d, uid); len(left) != 0 {
		t.Errorf("%d object(s) survived the reap: %v", len(left), left)
	}
}

// The reaper reports one engineUID per bundle, and the bundle label is how it
// knows which objects belong to which. The two cases that pull in opposite
// directions: an object whose own name is not the bundle's, and an object with
// no bundle label at all — written by a Simian from before bundles existed,
// when every fault was one Deployment named after itself.
func TestReapExpiredNamesTheBundleAndNotTheObject(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute).Format(time.RFC3339)

	member := managedDeployment("arena-1", "sidecar-x", map[string]string{ExpiryAnnotation: past})
	member.Labels[BundleLabel] = "storefront-abc"
	legacy := managedDeployment("arena-1", "catalog-sync-old", map[string]string{ExpiryAnnotation: past})

	d := newTestDriver(member, legacy)
	cleared, err := d.ReapExpired(context.Background(), []string{"arena-1"}, now)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	slices.Sort(cleared)
	want := []string{engineUID("arena-1", "catalog-sync-old"), engineUID("arena-1", "storefront-abc")}
	if !slices.Equal(cleared, want) {
		t.Errorf("cleared = %v, want %v", cleared, want)
	}
}

// --- the finisher ---

// RolloutStuck is the only kind whose failure state Apply cannot create in one
// call, so it is the only one where Apply is not finished when the objects
// exist. What the finisher does — wait for the healthy revision, then wedge it
// — is the fault.
func TestApplyWedgesTheRolloutAfterTheHealthyRevisionIsUp(t *testing.T) {
	d := newTestDriver()
	uid, err := d.Apply(context.Background(), manifest(KindRolloutStuck, nil))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	c := getDeployment(t, d, uid).Spec.Template.Spec.Containers[0]
	if c.Image != defaultBrokenRolloutImage {
		t.Errorf("image = %q, want the wedged revision %q: Apply returned before the rollout was broken", c.Image, defaultBrokenRolloutImage)
	}
	if cmd := strings.Join(c.Command, " "); !strings.Contains(cmd, "exit 1") {
		t.Errorf("command = %q, want the failing one", cmd)
	}
	// The patch must merge on the container name rather than replacing the
	// array, or it would silently drop anything the healthy pod spec set.
	if c.Name != containerName {
		t.Errorf("container name = %q, want %q", c.Name, containerName)
	}
}

// A healthy revision that never comes up is a broken arena, not a landed
// fault. Wedging it anyway would produce a Deployment that never had a working
// revision — the gate's "previous revision still available" half would be
// false, and the fault would be a lie about what it was doing.
func TestRolloutStuckRefusesToWedgeARevisionThatNeverCameUp(t *testing.T) {
	cs := fake.NewSimpleClientset()
	// No stand-in controller: status stays at the zero value, which is what a
	// Deployment whose pods cannot start looks like.
	d := New(cs)
	d.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	d.RolloutSettle = 50 * time.Millisecond

	_, err := d.Apply(context.Background(), manifest(KindRolloutStuck, nil))
	if err == nil {
		t.Fatal("Apply succeeded against a Deployment that never became available")
	}
	if !strings.Contains(err.Error(), "did not become fully available") {
		t.Errorf("error = %v, want it to name the wait that failed", err)
	}
	// And the bundle is gone. A failed Apply is never leased, so nothing will
	// ever call Clear for it; a half-applied fault left in the arena is one
	// nobody is tracking and nobody will take out.
	deps, err := cs.AppsV1().Deployments(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deps.Items) != 0 {
		t.Errorf("Apply failed but left %d deployment(s) behind", len(deps.Items))
	}
}

// The RolloutStuck gate asserts that every replica of the previous revision is
// still available, and it computes that number from the manifest before Apply
// runs. If the driver's default and the catalog's ever diverge, the gate would
// look for a count the Deployment was never created with.
func TestApplyReplicaDefaultAgreesWithTheGate(t *testing.T) {
	for _, kind := range Kinds() {
		t.Run(kind, func(t *testing.T) {
			d := newTestDriver()
			uid, err := d.Apply(context.Background(), manifest(kind, nil))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if kind == KindJobFailure {
				// A Job has no replicas.
				return
			}
			dep := getDeployment(t, d, uid)
			if got, want := int(*dep.Spec.Replicas), catalog.KubeStateDefaultReplicas(kind); got != want {
				t.Errorf("created with %d replicas, the catalog tells the gate %d", got, want)
			}
		})
	}
}
