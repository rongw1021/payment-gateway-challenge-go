package scenarios

import "testing"

// Run executes identical HTTP scenarios against integration and deployed APIs.
func Run(t *testing.T, address string, keys Credentials) {
	t.Helper()
	c := newClient(t, address, keys)
	if *httpVerbose {
		t.Log("Development-only HTTP output is enabled: headers and bodies are unredacted.")
	}
	for _, group := range []struct {
		name, description string
		run               func(*testing.T)
	}{
		{"AuthenticationAndAuthorization", "Enforce credentials, endpoint scopes, and merchant identity.", c.authenticationAndAuthorization},
		{"CardTokenization", "Validate card input and issue unique, merchant-owned, single-use tokens.", c.cardTokenization},
		{"PaymentIntentCreation", "Validate intent details, creation idempotency, and concurrent creation.", c.paymentIntentCreation},
		{"PaymentIntentConfirmation", "Exercise confirmation inputs, state transitions, conflicts, replays, and concurrency.", c.paymentIntentConfirmation},
		{"PaymentIntentRetrieval", "Retrieve owned intents and complete masked history in every externally reachable stable state.", c.paymentIntentRetrieval},
		{"RateLimit", "Verify HTTP throttling, retry guidance, health-check availability, and recovery.", c.rateLimit},
	} {
		runCase(t, group.name, group.description, group.run)
	}
}

func runCase(t *testing.T, name, description string, run func(*testing.T)) bool {
	t.Helper()
	return t.Run(name, func(t *testing.T) {
		if *httpVerbose {
			t.Logf("\n%s\nBEGIN CASE: %s\nDescription: %s\n%s",
				divider, t.Name(), description, divider)
			t.Cleanup(func() {
				outcome := "PASS"
				if t.Failed() {
					outcome = "FAIL"
				}
				t.Logf("\n%s\nEND CASE: %s [%s]\n%s", divider, t.Name(), outcome, divider)
			})
		}
		run(t)
	})
}
