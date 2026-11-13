package usagehistory

import (
	"testing"
	"time"
)

func TestIncreasingRetentionDoesNotPromisePrunedHistory(t *testing.T) {
	store, clock, path := historyFixture(t, 1)
	clock.Add(int64(48 * time.Hour))
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	boundary := store.cutoff()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	longer, err := openStore(path, 30, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := longer.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	// An event arriving late must not imply that the already-pruned period is complete.
	if err = longer.Append(sampleEvent("late", "a", boundary.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	result, err := longer.Query(t.Context(), queryFixture(longer, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.AvailableSince.Equal(boundary) || result.TotalEvents != 0 {
		t.Fatalf("retention extension advertised deleted history: %+v", result)
	}
}
