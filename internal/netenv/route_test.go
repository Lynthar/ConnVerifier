package netenv

import (
	"context"
	"reflect"
	"testing"

	"github.com/Lynthar/ConnVerifier/internal/result"
)

func TestRouteToLoopback(t *testing.T) {
	r, err := RouteTo(context.Background(), "127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if r.Family != "ipv4" || r.Interface == "" || r.Kind != KindOther {
		t.Fatalf("RouteTo(loopback) = %+v; want ipv4 on a named interface of kind other", r)
	}
}

func TestHost(t *testing.T) {
	check := func(node string, s result.Status) result.Check {
		return result.Check{Status: s, Path: result.Path{Node: node}}
	}
	tests := []struct {
		name   string
		checks []result.Check
		want   *result.Host
	}{
		{"no checks", nil, nil},
		{"skipped only", []result.Check{check("127.0.0.1:9", result.Skip)}, nil},
		{"one per family", []result.Check{
			check("127.0.0.1:9", result.Pass),
			check("127.0.0.1:9", result.Warn),
			check("127.0.0.2:9", result.Error),
			check("192.0.2.10:7443", result.Skip),
		}, &result.Host{Interfaces: []result.Interface{{Family: "ipv4", Kind: KindOther}}}},
		{"no route found", []result.Check{check("no port", result.Error)},
			&result.Host{Interfaces: []result.Interface{{Family: "", Kind: KindUnknown}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Host(tt.checks); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Host = %+v, want %+v", got, tt.want)
			}
		})
	}
}
