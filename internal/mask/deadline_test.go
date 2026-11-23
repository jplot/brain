package mask

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExhaustedBudgetReturnsOnlyTheMarker(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	got := textWithin("avant "+secret+" apres", time.Nanosecond)
	if got != Marker {
		t.Fatalf("got %q, want the bare marker", got)
	}
}

func TestConcurrentCallsDoNotShareAnotherCallsTimeout(t *testing.T) {
	// Each long scan outlasts a 1 ms timeout plus the 5 ms check period, so a call that
	// inherits a short MatchTimeout from a concurrent one gets wiped.
	in := strings.Repeat("lorem ipsum dolor ", 10000) + "ghp_" + strings.Repeat("a", 36)
	want := strings.Repeat("lorem ipsum dolor ", 10000) + Marker

	stop := make(chan struct{})
	var hammer sync.WaitGroup
	for range 8 {
		hammer.Add(1)
		go func() {
			defer hammer.Done()
			for {
				select {
				case <-stop:
					return
				default:
					textWithin("", time.Millisecond)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	wiped := make(chan string, 64)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2 {
				if got := textWithin(in, time.Minute); got != want {
					wiped <- got[len(got)-min(len(got), 40):]
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	hammer.Wait()
	close(wiped)
	if n := len(wiped); n > 0 {
		t.Fatalf("%d calls were wiped by another call's timeout, first ends with %q", n, <-wiped)
	}
}

func TestBudgetGrowsWithTheInput(t *testing.T) {
	if got := budgetFor(""); got != time.Second {
		t.Fatalf("empty input: %v", got)
	}
	if got := budgetFor(strings.Repeat("A", 1<<20)); got < 3*time.Second {
		t.Fatalf("1 MiB: %v", got)
	}
}
