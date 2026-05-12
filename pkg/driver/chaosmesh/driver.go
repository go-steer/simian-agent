// Package chaosmesh implements simian.ChaosDriver for the full Chaos Mesh
// CRD catalog via dynamic-client apply. Per requirements R-FAULT-01 / R-FAULT-07
// there are no per-fault-type Go wrappers — any chaos-mesh.org/v1alpha1
// resource installed in the cluster is addressable.
package chaosmesh

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// APIGroup is the Chaos Mesh CRD API group.
const APIGroup = "chaos-mesh.org"

// Driver implements simian.ChaosDriver for Chaos Mesh.
type Driver struct {
	dyn       dynamic.Interface
	disco     discovery.DiscoveryInterface
	mapper    *restmapper.DeferredDiscoveryRESTMapper
	namePrefix string
}

// New creates a Driver. namePrefix is the GenerateName prefix for created
// resources; defaults to "simian-".
func New(dyn dynamic.Interface, disco discovery.CachedDiscoveryInterface, namePrefix string) *Driver {
	if namePrefix == "" {
		namePrefix = "simian-"
	}
	return &Driver{
		dyn:        dyn,
		disco:      disco,
		mapper:     restmapper.NewDeferredDiscoveryRESTMapper(disco),
		namePrefix: namePrefix,
	}
}

// Engine implements ChaosDriver.
func (d *Driver) Engine() simian.Engine { return simian.EngineChaosMesh }

// Apply implements ChaosDriver. Builds an unstructured object from the
// manifest, injects spec.duration, and Creates via the dynamic client.
// Returns the engine UID encoded as "<namespace>/<name>".
func (d *Driver) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	if len(m.Targets) == 0 {
		return "", fmt.Errorf("chaos-mesh apply: manifest has no targets")
	}
	ns := m.Targets[0].Namespace
	if ns == "" {
		return "", fmt.Errorf("chaos-mesh apply: manifest target has no namespace")
	}

	gvk := schema.FromAPIVersionAndKind(m.APIVersion, m.ResourceKind)
	gvr, err := d.gvrFor(gvk)
	if err != nil {
		return "", fmt.Errorf("chaos-mesh apply: %w", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(m.APIVersion)
	obj.SetKind(m.ResourceKind)
	obj.SetGenerateName(d.namePrefix)
	obj.SetNamespace(ns)
	obj.SetLabels(map[string]string{
		"simian.chaos/managed":   "true",
		"simian.chaos/fault-uid": m.UID,
	})

	// Deep-copy the spec map into the unstructured object. Inject the duration
	// — every Chaos Mesh resource accepts a top-level spec.duration string.
	spec := deepCopyMap(m.Spec)
	if m.Duration > 0 {
		spec["duration"] = m.Duration.String()
	}
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		return "", fmt.Errorf("chaos-mesh apply: set spec: %w", err)
	}

	created, err := d.dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("chaos-mesh apply: create %s: %w", gvk.Kind, err)
	}
	return engineUID(created.GetNamespace(), created.GetName(), gvr), nil
}

// Clear implements ChaosDriver. Decodes the engineUID, deletes the resource.
// Idempotent — NotFound is treated as success.
func (d *Driver) Clear(ctx context.Context, engineUIDStr string) error {
	ns, name, gvr, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		return err
	}
	err = d.dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("chaos-mesh clear %s/%s: %w", ns, name, err)
	}
	return nil
}

// Catalog implements ChaosDriver. Enumerates installed CRDs in the chaos-mesh.org
// API group and emits one CatalogEntry per Kind.
func (d *Driver) Catalog(ctx context.Context) ([]simian.CatalogEntry, error) {
	groups, err := d.disco.ServerGroups()
	if err != nil {
		return nil, fmt.Errorf("chaos-mesh catalog: server groups: %w", err)
	}
	var preferred string
	for _, g := range groups.Groups {
		if g.Name == APIGroup {
			preferred = g.PreferredVersion.GroupVersion
			break
		}
	}
	if preferred == "" {
		// Chaos Mesh not installed; not an error — just an empty catalog.
		return nil, nil
	}
	resList, err := d.disco.ServerResourcesForGroupVersion(preferred)
	if err != nil {
		return nil, fmt.Errorf("chaos-mesh catalog: server resources for %s: %w", preferred, err)
	}
	out := make([]simian.CatalogEntry, 0, len(resList.APIResources))
	seen := map[string]bool{}
	for _, r := range resList.APIResources {
		// Filter subresources (status, scale, etc.)
		if strings.Contains(r.Name, "/") {
			continue
		}
		// Many Chaos Mesh CRDs end in "Chaos" — keep all but elide internal
		// helper resources whose Kind starts with lowercase or is utility-only.
		if r.Kind == "" || seen[r.Kind] {
			continue
		}
		if !catalog.IsUserFault(simian.EngineChaosMesh, r.Kind) {
			continue
		}
		seen[r.Kind] = true
		out = append(out, simian.CatalogEntry{
			Engine:          simian.EngineChaosMesh,
			APIVersion:      preferred,
			ResourceKind:    r.Kind,
			BlastRadiusTier: catalog.Classify(simian.EngineChaosMesh, r.Kind),
			Description:     r.Kind + " (chaos-mesh.org)",
		})
	}
	return out, nil
}

// gvrFor resolves a GVK to a GVR using the cached REST mapper.
func (d *Driver) gvrFor(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	mapping, err := d.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("rest mapping for %s: %w", gvk, err)
	}
	return mapping.Resource, nil
}

// engineUID encodes the resource location as "ns/name@group/version/resource".
func engineUID(namespace, name string, gvr schema.GroupVersionResource) string {
	return fmt.Sprintf("%s/%s@%s/%s/%s", namespace, name, gvr.Group, gvr.Version, gvr.Resource)
}

func decodeEngineUID(s string) (string, string, schema.GroupVersionResource, error) {
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 {
		return "", "", schema.GroupVersionResource{}, fmt.Errorf("invalid engineUID %q", s)
	}
	nn := strings.SplitN(parts[0], "/", 2)
	if len(nn) != 2 {
		return "", "", schema.GroupVersionResource{}, fmt.Errorf("invalid engineUID namespace/name %q", parts[0])
	}
	gvrParts := strings.Split(parts[1], "/")
	if len(gvrParts) != 3 {
		return "", "", schema.GroupVersionResource{}, fmt.Errorf("invalid engineUID gvr %q", parts[1])
	}
	return nn[0], nn[1], schema.GroupVersionResource{
		Group:    gvrParts[0],
		Version:  gvrParts[1],
		Resource: gvrParts[2],
	}, nil
}

// deepCopyMap is a shallow-friendly deep copy sufficient for JSON-shaped maps.
func deepCopyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	case time.Duration:
		return t.String()
	default:
		return v
	}
}
