package telemetry

import "testing"

func TestEventCommonDefaults(t *testing.T) {
	got := NewEventCommon(CommonInput{RequestID: "req-1", Operation: "chat.completions"})
	if got.ServiceName != "llmgate" || got.ServiceVersion != "dev" || got.Environment != "local" {
		t.Fatalf(
			"defaults = service_name=%q service_version=%q environment=%q",
			got.ServiceName,
			got.ServiceVersion,
			got.Environment,
		)
	}
}
