package notifications

import (
	"reflect"
	"testing"
)

func TestNonNilResources(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil becomes a non-nil empty slice", nil, []string{}},
		{"already-empty non-nil slice is unchanged", []string{}, []string{}},
		{"non-empty slice is unchanged", []string{"estimate", "sales_order"}, []string{"estimate", "sales_order"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nonNilResources(tt.in)
			if got == nil {
				t.Fatal("got nil, want a non-nil slice")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}
