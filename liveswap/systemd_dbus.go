package liveswap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// nofile is the manager's DefaultLimitNOFILE (its hard ceiling for
	// units), read once per connection; 0 = unknown, leave units alone.
	nofile atomic.Uint64
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

// probeTimeout bounds the Provision-time round trip to the manager.
const probeTimeout = 10 * time.Second

// probe proves the manager answers a real request within a deadline.
func (c *userManagerClient) probe() error {
	conn, err := c.get()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		_, err = conn.ListUnitsByPatternsContext(ctx, nil, []string{unitPrefix + "*.service"})
		cancel()
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
	if c.conn != nil {
		if c.conn.Connected() {
			conn := c.conn
			c.mu.Unlock()
			return conn, nil
		}
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()

	// Dial outside the lock: a slow or wedged manager must not make
	// every caller queue behind one redial past their own deadlines.
	// The socket deadlines set in dialPrivate stay in force until
	// NewConnection has finished (it dials twice and then installs its
	// signal match), so the whole establishment is bounded.
	sock := userManagerSocket()
	var raws []net.Conn
	conn, err := sddbus.NewConnection(func() (*godbus.Conn, error) {
		dc, nc, err := dialPrivate(sock)
		if err == nil {
			raws = append(raws, nc)
		}
		return dc, err
	})
	if err != nil {
		return nil, err
	}
	// systemd gives services soft NOFILE 1024 by default however high
	// the hard limit is; the exec runner's children inherited
	// hotserve.service's soft=hard. Units are created with both set to
	// the manager's hard ceiling (which the package raises via a
	// user@<uid>.service.d drop-in), so a setrlimit can never exceed
	// what the manager permits. Read while the handshake deadlines are
	// still armed, so a manager that stops answering here cannot hang
	// every caller; unknown just means "leave the limits alone".
	if v, err := conn.GetManagerProperty("DefaultLimitNOFILE"); err == nil {
		c.nofile.Store(parseManagerUint(v))
	} else if !conn.Connected() {
		conn.Close()
		return nil, err
	}
	for _, nc := range raws {
		if derr := nc.SetDeadline(time.Time{}); derr != nil {
			conn.Close()
			return nil, derr
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.conn.Connected() {
		conn.Close() // another caller won the redial
		return c.conn, nil
	}
	c.conn = conn
	return conn, nil
}

// parseManagerUint reads go-systemd's string rendering of a uint64
// manager property ("@t 524288"); 0 for anything unparseable or
// unlimited, which callers treat as "don't set".
func parseManagerUint(s string) uint64 {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@t"))
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == ^uint64(0) {
		return 0
	}
	return n
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

// dialTimeout bounds connecting to and authenticating with the private
// socket, so a manager that accepts the connection but never finishes
// the handshake cannot hang a call that advertises a deadline.
const dialTimeout = 10 * time.Second

// dialPrivate mirrors go-systemd's own private-socket dial for
// /run/systemd/private: EXTERNAL auth with our uid and no Hello (the
// private socket is not a bus daemon). The returned net.Conn carries a
// deadline the caller clears once the connection is fully set up; the
// established connection then lives for the process (godbus would
// close it with a call-scoped context, hence Background).
func dialPrivate(socket string) (*godbus.Conn, net.Conn, error) {
	nc, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return nil, nil, err
	}
	if err := nc.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		_ = nc.Close()
		return nil, nil, err
	}
	conn, err := godbus.NewConn(nc, godbus.WithContext(context.Background()))
	if err != nil {
		_ = nc.Close()
		return nil, nil, err
	}
	if err := conn.Auth([]godbus.Auth{godbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, nc, nil
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
	// TimeoutStopUSec=0 means "never SIGKILL"; a sub-microsecond grace
	// must not truncate into that.
	stopUSec := uint64(u.StopTimeout / time.Microsecond) //nolint:gosec // durations here are small positive config values
	if stopUSec == 0 {
		stopUSec = 1
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
		{Name: "TimeoutStopUSec", Value: godbus.MakeVariant(stopUSec)},
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
	props := unitProperties(u)
	if n := c.nofile.Load(); n > 0 {
		props = append(props,
			sddbus.Property{Name: "LimitNOFILE", Value: godbus.MakeVariant(n)},
			sddbus.Property{Name: "LimitNOFILESoft", Value: godbus.MakeVariant(n)},
		)
	}
	// go-systemd delivers the result with its listener lock held, so
	// the channel must never block: buffer it.
	ch := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, u.Name, "fail", props, ch); err != nil {
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
		if isNoSuchUnit(err) {
			return "done", nil // already unloaded: stopped by definition
		}
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
	if isNoSuchUnit(err) {
		return nil // already unloaded: nothing left to reset
	}
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
		if isNoSuchUnit(err) {
			return unitStatus{Name: name, LoadState: "not-found"}, nil
		}
		c.dropIfDisconnected(conn, err)
		return unitStatus{}, err
	}
	st := statusFromProps(props)
	st.Name = name
	return st, nil
}

// isNoSuchUnit reports the manager saying it does not know the unit —
// for our purposes "not loaded", never a transport failure.
func isNoSuchUnit(err error) bool {
	var derr godbus.Error
	if !errors.As(err, &derr) {
		return false
	}
	switch derr.Name {
	case "org.freedesktop.systemd1.NoSuchUnit", "org.freedesktop.DBus.Error.UnknownObject":
		return true
	}
	return false
}

// statusFromProps reads the fields the runner needs out of a unit's
// full property map (Unit + Service interfaces).
func statusFromProps(props map[string]any) unitStatus {
	return unitStatus{
		LoadState:      propString(props, "LoadState"),
		ActiveState:    propString(props, "ActiveState"),
		SubState:       propString(props, "SubState"),
		Result:         propString(props, "Result"),
		MainPID:        propInt(props, "MainPID"),
		ExecMainCode:   propInt(props, "ExecMainCode"),
		ExecMainStatus: propInt(props, "ExecMainStatus"),
		StopTimeout:    time.Duration(propInt(props, "TimeoutStopUSec")) * time.Microsecond,
	}
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
