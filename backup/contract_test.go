package backup

import (
	"encoding/json"
	"testing"

	"github.com/smallhoursorg/hotserve/liveswap"
)

// This package deliberately does not import liveswap: it reads that
// module's config over the admin API, so the coupling is a JSON
// contract, not a compile-time one. The tests do import it, to prove
// the contract is what this package assumes — the check has to live
// somewhere, and a test is the one place where the import costs
// nothing at run time.

func TestStateEntryMatchesLiveswapJSON(t *testing.T) {
	encoded, err := json.Marshal([]liveswap.StateEntry{
		{Kind: liveswap.StateKindSQLite, Path: "app.db"},
		{Kind: liveswap.StateKindFiles, Path: "uploads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded []StateEntry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("liveswap's state entries no longer decode here: %v", err)
	}
	want := []StateEntry{{Kind: KindSQLite, Path: "app.db"}, {Kind: KindFiles, Path: "uploads"}}
	for i := range want {
		if decoded[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, decoded[i], want[i])
		}
	}
}

func TestKindNamesMatchLiveswap(t *testing.T) {
	if KindSQLite != liveswap.StateKindSQLite || KindFiles != liveswap.StateKindFiles {
		t.Fatalf("kinds drifted: %q/%q here, %q/%q in liveswap",
			KindSQLite, KindFiles, liveswap.StateKindSQLite, liveswap.StateKindFiles)
	}
}

// The default root is duplicated (see DefaultLiveswapRoot); if
// liveswap ever moves its data, backups must not keep reading the old
// path and reporting success.
func TestDefaultRootMatchesLiveswap(t *testing.T) {
	if DefaultLiveswapRoot != liveswap.DefaultRoot {
		t.Fatalf("default root drifted: %q here, %q in liveswap", DefaultLiveswapRoot, liveswap.DefaultRoot)
	}
}
