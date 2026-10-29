package deliveries

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDelivery_JSON_ProviderStatus(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	t.Run("included when set", func(t *testing.T) {
		out, err := json.Marshal(Delivery{ID: "d1", ProviderStatus: "bounced", ProviderStatusAt: &at})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(out)
		if !strings.Contains(s, `"providerStatus":"bounced"`) || !strings.Contains(s, `"providerStatusAt":"2026-10-01T12:00:00Z"`) {
			t.Fatalf("json = %s, want providerStatus and providerStatusAt", s)
		}
	})

	t.Run("omitted when unset", func(t *testing.T) {
		out, err := json.Marshal(Delivery{ID: "d1"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(out), "providerStatus") {
			t.Fatalf("json = %s, want no providerStatus fields", out)
		}
	})
}
