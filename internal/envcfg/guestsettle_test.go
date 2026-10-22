package envcfg_test

import (
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"

	"github.com/stretchr/testify/assert"
)

// GuestSettle: unset env -> 60s default; a valid Go duration -> that value;
// empty/unparseable/non-positive -> default.
func TestGuestSettle(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
	}{
		{name: "Unset falls back to 60s default", set: false, want: 60 * time.Second},
		{name: "Valid 90s override", set: true, value: "90s", want: 90 * time.Second},
		{name: "Empty falls back to default", set: true, value: "", want: 60 * time.Second},
		{name: "Unparseable falls back to default", set: true, value: "soon", want: 60 * time.Second},
		{name: "Zero falls back to default", set: true, value: "0s", want: 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("VZ_BUILD_GUEST_SETTLE", tt.value)
			} else {
				t.Setenv("VZ_BUILD_GUEST_SETTLE", "")
			}
			assert.Equal(t, tt.want, envcfg.GuestSettle())
		})
	}
}
