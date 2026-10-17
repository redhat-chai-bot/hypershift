package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidPublishingHostname(t *testing.T) {
	for _, hostname := range []string{"localhost", "api", "api.example.com", "API.Example.COM", strings.Repeat("a", 63) + ".example.com", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)} {
		t.Run("When hostname is valid "+hostname+", it should accept it", func(t *testing.T) {
			require.True(t, validPublishingHostname(hostname))
		})
	}
	for _, hostname := range []string{"", "-api.example.com", "api-.example.com", strings.Repeat("a", 64) + ".example.com", "api..example.com", "api.example.com.", strings.Repeat("a", 254)} {
		t.Run("When hostname is invalid "+hostname+", it should reject it", func(t *testing.T) {
			require.False(t, validPublishingHostname(hostname))
		})
	}
}

func TestValidPublishingAddress(t *testing.T) {
	for _, hostname := range []string{"localhost", "api", "api.example.com", "API.Example.COM", strings.Repeat("a", 63) + ".example.com", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)} {
		t.Run("When hostname is valid "+hostname+", it should accept it", func(t *testing.T) {
			require.True(t, validPublishingAddress(hostname))
		})
	}
	for _, hostname := range []string{"", "-api.example.com", "api-.example.com", strings.Repeat("a", 64) + ".example.com", "api..example.com", "api.example.com.", strings.Repeat("a", 254)} {
		t.Run("When hostname is invalid "+hostname+", it should reject it", func(t *testing.T) {
			require.False(t, validPublishingAddress(hostname))
		})
	}
	for _, address := range []string{"0.0.0.0", "255.255.255.255", "::", "2001:db8::1", "::ffff:192.0.2.10", "2001:db8:0:0:0:0:192.0.2.10"} {
		t.Run("When address is valid "+address+", it should accept it", func(t *testing.T) { require.True(t, validPublishingAddress(address)) })
	}
	for _, address := range []string{"999.999.999.999", "256.0.0.1", "192.0.2.01", "2001:db8:::1", "::ffff:999.0.0.1"} {
		t.Run("When address is invalid "+address+", it should reject it", func(t *testing.T) { require.False(t, validPublishingAddress(address)) })
	}
}
