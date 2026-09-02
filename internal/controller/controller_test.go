package controller

import (
	"log/slog"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/RamaHerbin/sluice/internal/router"
)

// TestNewDoesNotStart is a placeholder that also pins the fake clientset
// dependency in go.mod. The controller agent replaces it.
func TestNewDoesNotStart(t *testing.T) {
	c := New(fake.NewClientset(), router.NewStore(), slog.Default(), NopMetrics{}, Options{})
	if c == nil {
		t.Fatal("New returned nil")
	}
	select {
	case <-c.Synced():
		t.Fatal("Synced closed before Run")
	default:
	}
}

func TestEndpointSliceServiceKey(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{}
	slice.Namespace = "demo"
	slice.Name = "echo-abc"
	slice.Labels = map[string]string{discoveryv1.LabelServiceName: "echo"}
	keys, err := endpointSliceServiceKey(slice)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "demo/echo" {
		t.Fatalf("keys = %v, want [demo/echo]", keys)
	}
	if got := objectKey(slice); got != "demo/echo-abc" {
		t.Fatalf("objectKey = %q", got)
	}
	if _, err := endpointSliceServiceKey("nope"); err == nil {
		t.Fatal("expected error for wrong type")
	}
}
