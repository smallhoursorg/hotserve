package unit

import (
	"context"
	"fmt"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
)

// Active is a unit the manager holds that is not at rest.
type Active struct {
	Name string
	// State is the manager's ActiveState. A oneshot is "activating" for
	// as long as its command runs, never "active".
	State string
	// Since is when the unit last left "inactive": when it was started.
	Since time.Time
}

// ListActive asks the system manager which units matching the patterns
// are running, starting or stopping, and since when. It reads, which
// the manager lets anyone do: no root, and nothing is started.
func ListActive(ctx context.Context, patterns ...string) ([]Active, error) {
	c, err := sddbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to the system manager: %w", err)
	}
	defer c.Close()
	units, err := c.ListUnitsByPatternsContext(ctx, []string{"active", "activating", "deactivating"}, patterns)
	if err != nil {
		return nil, fmt.Errorf("listing units: %w", err)
	}
	out := make([]Active, 0, len(units))
	for _, u := range units {
		a := Active{Name: u.Name, State: u.ActiveState}
		// A unit that ended between the two questions has no answer to
		// the second; it is said without a time rather than left out.
		if p, err := c.GetUnitPropertyContext(ctx, u.Name, "InactiveExitTimestamp"); err == nil {
			if usec, ok := p.Value.Value().(uint64); ok && usec > 0 {
				a.Since = time.UnixMicro(int64(usec)).UTC() //nolint:gosec // microseconds since 1970 fit for the next 290,000 years
			}
		}
		out = append(out, a)
	}
	return out, nil
}
