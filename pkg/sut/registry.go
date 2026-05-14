package sut

import (
	"fmt"
	"sort"
	"sync"
)

// MemoryRegistry is the default Registry implementation.
type MemoryRegistry struct {
	mu   sync.RWMutex
	suts map[string]SUT
}

// NewMemoryRegistry constructs an empty registry.
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{suts: map[string]SUT{}}
}

// MustRegister panics if a SUT with the same name is already registered;
// intended for init() of built-in SUT packages.
func (r *MemoryRegistry) MustRegister(s SUT) {
	if err := r.Register(s); err != nil {
		panic(err)
	}
}

// Register adds s to the registry. Errors if the name is already taken.
func (r *MemoryRegistry) Register(s SUT) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.suts[s.Name()]; exists {
		return fmt.Errorf("sut: %q already registered", s.Name())
	}
	r.suts[s.Name()] = s
	return nil
}

// Get implements Registry.
func (r *MemoryRegistry) Get(name string) (SUT, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.suts[name]
	return s, ok
}

// List implements Registry. Returns SUTs sorted by name for stable output.
func (r *MemoryRegistry) List() []SUT {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SUT, 0, len(r.suts))
	for _, s := range r.suts {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Default is the package-level registry that built-in SUT packages register
// themselves into via init().
var Default = NewMemoryRegistry()
