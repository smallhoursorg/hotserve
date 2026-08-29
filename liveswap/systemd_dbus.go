package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// userManagerClient talks to the systemd service manager of the user
// hotserve runs as, over that manager's private socket
// ($XDG_RUNTIME_DIR/systemd/private; /run/user/<uid> by default) — the
// same path `systemctl --user` takes when no session bus is around. No
// session bus, no polkit: the socket is owned by our uid and answers
// only to it, and it exists whenever user@<uid>.service is running
// (lingering keeps it running with nobody logged in).
//
// The connection is dialed lazily and shared process-wide: pooled
// managedApps outlive any one config, so it belongs to the process,
// not to an App. A transport failure drops it and the next call
// redials; callers see the error of the call that hit it.
type userManagerClient struct {
	mu   sync.Mutex
	conn *sddbus.Conn
}

// userManager is the process-wide client every systemdRunner shares.
var userManager = &userManagerClient{}

// probeUserManager is what Provision calls to fail loudly when the
// manager is unreachable; a variable so unit tests can stub it.
var probeUserManager = func() error { return userManager.probe() }

// userManagerSocket is the private socket of this uid's manager.
func userManagerSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	return filepath.Join(dir, "systemd", "private")
}

// probe proves the manager answers and reports a useful version.
func (c *userManagerClient) probe() error {
	conn, err := c.get()
	if err == nil {
		_, err = conn.GetManagerProperty("Version")
		c.dropIfDisconnected(conn, err)
	}
	if err != nil {
		return fmt.Errorf("liveswap needs the systemd user manager for uid %d (socket %s): %w — apps run as transient units under it; on a packaged install make sure `loginctl enable-linger hotserve` is in effect and user@%d.service is active",
			os.Getuid(), userManagerSocket(), err, os.Getuid())
	}
	return nil
}

func (c *userManagerClient) get() (*sddbus.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		if c.conn.Connected() {
			return c.conn, nil
		}
		c.conn.Close()
		c.conn = nil
	}
	sock := userManagerSocket()
	conn, err := sddbus.NewConnection(func() (*godbus.Conn, error) { return dialPrivate(sock) })
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

// dropIfDisconnected forgets a connection that a failed call has
// found dead, so the next call redials instead of failing forever.
func (c *userManagerClient) dropIfDisconnected(conn *sddbus.Conn, err error) {
	if err == nil || conn.Connected() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn.Close()
		c.conn = nil
	}
}

// dialPrivate mirrors go-systemd's own private-socket dial for
// /run/systemd/private: EXTERNAL auth with our uid and no Hello (the
// private socket is not a bus daemon). godbus binds a connection to
// the context it is dialed with, hence Background, not a call context.
func dialPrivate(socket string) (*godbus.Conn, error) {
	conn, err := godbus.Dial("unix:path="+socket, godbus.WithContext(context.Background()))
	if err != nil {
		return nil, err
	}
	if err := conn.Auth([]godbus.Auth{godbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// unitProperties renders a unitSpec as transient-unit properties. The
// hardening set is fixed here for every unit hotserve creates:
// Restart=no (the watchdog restarts), KillMode=control-group (stop
// means the whole tree), NoNewPrivileges (no setuid escalation), and
// both output streams to the journal under the app's identifier.
func unitProperties(u unitSpec) []sddbus.Property {
	typ := "simple"
	if u.Oneshot {
		typ = "oneshot"
	}
	env := u.Environment
	if env == nil {
		env = []string{}
	}
	return []sddbus.Property{
		sddbus.PropDescription(u.Description),
		sddbus.PropType(typ),
		sddbus.PropExecStart(u.ExecStart, false),
		{Name: "WorkingDirectory", Value: godbus.MakeVariant(u.WorkingDirectory)},
		{Name: "Environment", Value: godbus.MakeVariant(env)},
		{Name: "Restart", Value: godbus.MakeVariant("no")},
		{Name: "KillMode", Value: godbus.MakeVariant("control-group")},
		{Name: "NoNewPrivileges", Value: godbus.MakeVariant(true)},
		{Name: "TimeoutStopUSec", Value: godbus.MakeVariant(uint64(u.StopTimeout / time.Microsecond))}, //nolint:gosec // durations here are small positive config values
		{Name: "SyslogIdentifier", Value: godbus.MakeVariant(u.SyslogIdentifier)},
		{Name: "StandardOutput", Value: godbus.MakeVariant("journal")},
		{Name: "StandardError", Value: godbus.MakeVariant("journal")},
	}
}

func (c *userManagerClient) StartTransientUnit(ctx context.Context, u unitSpec) (string, error) {
	conn, err := c.get()
	if err != nil {
		return "", err
	}
	// go-systemd delivers the result with its listener lock held, so
	// the channel must never block: buffer it.
	ch := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, u.Name, "fail", unitProperties(u), ch); err != nil {
		c.dropIfDisconnected(conn, err)
		return "", err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *userManagerClient) StopUnit(ctx context.Context, name string) (string, error) {
	conn, err := c.get()
	if err != nil {
		return "", err
	}
	ch := make(chan string, 1)
	if _, err := conn.StopUnitContext(ctx, name, "replace", ch); err != nil {
		c.dropIfDisconnected(conn, err)
		return "", err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *userManagerClient) ResetFailedUnit(ctx context.Context, name string) error {
	conn, err := c.get()
	if err != nil {
		return err
	}
	err = conn.ResetFailedUnitContext(ctx, name)
	c.dropIfDisconnected(conn, err)
	return err
}

// UnitStatus reads every property of the unit object in one round
// trip (GetAll with an empty interface spans Unit and Service). A unit
// the manager does not know comes back as not-found, never an error.
func (c *userManagerClient) UnitStatus(ctx context.Context, name string) (unitStatus, error) {
	conn, err := c.get()
	if err != nil {
		return unitStatus{}, err
	}
	props, err := conn.GetAllPropertiesContext(ctx, name)
	if err != nil {
		var derr godbus.Error
		if errors.As(err, &derr) {
			switch derr.Name {
			case "org.freedesktop.systemd1.NoSuchUnit", "org.freedesktop.DBus.Error.UnknownObject":
				return unitStatus{LoadState: "not-found"}, nil
			}
		}
		c.dropIfDisconnected(conn, err)
		return unitStatus{}, err
	}
	return unitStatus{
		LoadState:      propString(props, "LoadState"),
		ActiveState:    propString(props, "ActiveState"),
		SubState:       propString(props, "SubState"),
		Result:         propString(props, "Result"),
		MainPID:        propInt(props, "MainPID"),
		ExecMainCode:   propInt(props, "ExecMainCode"),
		ExecMainStatus: propInt(props, "ExecMainStatus"),
	}, nil
}

// ListUnits returns the loaded units whose names match pattern, with
// only the fields the manager's listing carries (name and states).
func (c *userManagerClient) ListUnits(ctx context.Context, pattern string) ([]unitStatus, error) {
	conn, err := c.get()
	if err != nil {
		return nil, err
	}
	units, err := conn.ListUnitsByPatternsContext(ctx, nil, []string{pattern})
	if err != nil {
		c.dropIfDisconnected(conn, err)
		return nil, err
	}
	out := make([]unitStatus, 0, len(units))
	for _, u := range units {
		out = append(out, unitStatus{Name: u.Name, LoadState: u.LoadState, ActiveState: u.ActiveState, SubState: u.SubState})
	}
	return out, nil
}

func propString(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}

func propInt(props map[string]any, key string) int {
	switch v := props[key].(type) {
	case int32:
		return int(v)
	case uint32:
		return int(v)
	case int64:
		return int(v)
	case uint64:
		return int(v) //nolint:gosec // PIDs and exit codes fit comfortably
	case int:
		return v
	}
	return 0
}

var _ systemdConn = (*userManagerClient)(nil)
