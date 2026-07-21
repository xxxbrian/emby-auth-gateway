package routeclass

import (
	"sync"
	"testing"
)

func TestInventoryReturnsDeepCopy(t *testing.T) {
	first := Inventory()
	if len(first) == 0 {
		t.Fatal("empty inventory")
	}
	// Snapshot originals before mutation.
	origLen := len(first)
	origTemplate := first[0].PathTemplate
	origMethods := append([]string(nil), first[0].Methods...)
	origAuth := first[0].AuthMode

	// Mutate returned slice header and nested Methods / string fields.
	first[0].PathTemplate = "/Mutated/Path"
	first[0].AuthMode = "mutated-auth"
	first[0].Ownership = Unclassified
	first[0].Operation = OperationUnclassified
	if len(first[0].Methods) > 0 {
		first[0].Methods[0] = "MUTATED"
		first[0].Methods = append(first[0].Methods, "EXTRA")
	}
	first = append(first, InventoryRule{
		Methods:      []string{"TRACE"},
		PathTemplate: "/Evil",
		Ownership:    Unclassified,
		Operation:    OperationUnclassified,
	})

	second := Inventory()
	if len(second) != origLen {
		t.Fatalf("Inventory length after mutation = %d, want %d (live table mutated)", len(second), origLen)
	}
	if second[0].PathTemplate != origTemplate {
		t.Fatalf("PathTemplate leaked mutation: %q want %q", second[0].PathTemplate, origTemplate)
	}
	if second[0].AuthMode != origAuth {
		t.Fatalf("AuthMode leaked mutation: %q want %q", second[0].AuthMode, origAuth)
	}
	if second[0].Ownership == Unclassified || second[0].Operation == OperationUnclassified {
		t.Fatalf("Ownership/Operation leaked mutation: %+v", second[0])
	}
	if len(second[0].Methods) != len(origMethods) {
		t.Fatalf("Methods length leaked: %v want %v", second[0].Methods, origMethods)
	}
	for i := range origMethods {
		if second[0].Methods[i] != origMethods[i] {
			t.Fatalf("Methods[%d] leaked: %q want %q", i, second[0].Methods[i], origMethods[i])
		}
	}

	// Classify must still honor the unmutated inventory.
	d := Classify("GET", "/System/Info/Public")
	if d.Ownership != LocalPublic || !d.MethodAllowed {
		t.Fatalf("Classify after Inventory mutation: %+v", d)
	}
	d = Classify("TRACE", "/Evil")
	if d.Ownership != Unclassified {
		t.Fatalf("mutated inventory must not authorize /Evil: %+v", d)
	}
}

func TestInventoryDeepCopyConcurrent(t *testing.T) {
	// Race-safe: concurrent Inventory copies + Classify must not observe cross-talk.
	const workers = 8
	const iters = 50
	var wg sync.WaitGroup
	errCh := make(chan string, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				inv := Inventory()
				if len(inv) == 0 {
					errCh <- "empty inventory"
					return
				}
				inv[0].Methods[0] = "X"
				inv[0].PathTemplate = "/X"
				inv[0].AuthMode = "x"
				d := Classify("GET", "/System/Info/Public")
				if d.Ownership != LocalPublic || !d.MethodAllowed {
					errCh <- "classify broken under concurrent Inventory mutation"
					return
				}
				// Fresh copy must not show peer mutations.
				fresh := Inventory()
				if fresh[0].PathTemplate == "/X" || fresh[0].AuthMode == "x" || fresh[0].Methods[0] == "X" {
					errCh <- "concurrent mutation visible in fresh Inventory"
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
}

func TestImageInventoryAuthModeAnonymousOrCookieOrSession(t *testing.T) {
	const want = "anonymous-or-resource-cookie-or-session"
	found := 0
	for _, rule := range Inventory() {
		if rule.PathTemplate == "/Items/{ItemId}/Images/{ImageType}" ||
			rule.PathTemplate == "/Items/{ItemId}/Images/{ImageType}/{Index}" {
			found++
			if rule.AuthMode != want {
				t.Fatalf("%s authMode = %q, want %q", rule.PathTemplate, rule.AuthMode, want)
			}
			// Methods/templates/ownership unchanged.
			if rule.Ownership != MediaProxy || rule.Operation != OperationMediaProxy {
				t.Fatalf("image rule ownership/op changed: %+v", rule)
			}
			if len(rule.Methods) != 2 || rule.Methods[0] != "GET" || rule.Methods[1] != "HEAD" {
				t.Fatalf("image methods changed: %v", rule.Methods)
			}
		}
	}
	if found != 2 {
		t.Fatalf("expected 2 image inventory templates, found %d", found)
	}
}
