package backend

import (
	"context"
	"testing"

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// stubProvisioner is a do-nothing backend.Provisioner used to exercise the
// registry without a real isolation backend.
type stubProvisioner struct{ opts Options }

// Provision is a no-op that satisfies box.Provisioner.
//
// @return box.Instance Always nil.
// @error error Always nil.
//
// @testcase TestRegisterAndNew builds a stubProvisioner through the registry.
func (s *stubProvisioner) Provision(context.Context, sandbox.CreateOptions) (box.Instance, error) {
	return nil, nil
}

// List is a no-op that satisfies box.Provisioner.
//
// @return []box.Instance Always nil.
// @error error Always nil.
//
// @testcase TestRegisterAndNew builds a stubProvisioner through the registry.
func (s *stubProvisioner) List(context.Context) ([]box.Instance, error) { return nil, nil }

// Find is a no-op that satisfies box.Provisioner.
//
// @return box.Instance Always nil.
// @error error Always nil.
//
// @testcase TestRegisterAndNew builds a stubProvisioner through the registry.
func (s *stubProvisioner) Find(context.Context, string) (box.Instance, error) { return nil, nil }

// Close is a no-op that satisfies backend.Provisioner.
//
// @error error Always nil.
//
// @testcase TestRegisterAndNew builds a stubProvisioner through the registry.
func (s *stubProvisioner) Close() error { return nil }

var _ Provisioner = (*stubProvisioner)(nil)

// TestRegisterAndNew registers a backend and builds it back through New, and
// checks Names reports it.
func TestRegisterAndNew(t *testing.T) {
	var got Options
	Register("stub", func(o Options) (Provisioner, error) {
		got = o
		return &stubProvisioner{opts: o}, nil
	})

	want := Options{DefaultImage: "img", Namespace: "ns"}
	p, err := New("stub", want)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("New returned nil provisioner")
	}
	if got.DefaultImage != "img" || got.Namespace != "ns" {
		t.Fatalf("factory received %+v, want image/ns propagated", got)
	}
	var found bool
	for _, n := range Names() {
		if n == "stub" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() = %v, want it to include stub", Names())
	}
}

// TestRegisterPanicsOnDuplicate checks registering the same name twice panics.
func TestRegisterPanicsOnDuplicate(t *testing.T) {
	Register("dup", func(Options) (Provisioner, error) { return &stubProvisioner{}, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate name should panic")
		}
	}()
	Register("dup", func(Options) (Provisioner, error) { return &stubProvisioner{}, nil })
}

// TestNewUnknownBackend checks New errors on an unregistered name.
func TestNewUnknownBackend(t *testing.T) {
	if _, err := New("does-not-exist", Options{}); err == nil {
		t.Fatal("New with an unknown backend should fail")
	}
}

// TestNewEmptyNameUsesDefault checks an empty name selects DefaultName.
func TestNewEmptyNameUsesDefault(t *testing.T) {
	var built bool
	Register(DefaultName, func(Options) (Provisioner, error) {
		built = true
		return &stubProvisioner{}, nil
	})
	if _, err := New("", Options{}); err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if !built {
		t.Fatal("New(\"\") should build the default backend")
	}
}
