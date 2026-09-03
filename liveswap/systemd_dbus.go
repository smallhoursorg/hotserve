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
	"go.uber.org/zap"
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
	// generation counts successful dials, so a measurement can name the
	// connection it was taken on. Starts at 1: 0 means "never dialed",
	// which no cached measurement can match.
	generation atomic.Uint64
	// nofile is the manager's DefaultLimitNOFILE (its hard ceiling for
	// units), read once per connection; 0 = unknown, leave units alone.
	nofile atomic.Uint64

	// The sandbox capability is a property of the manager, not of a
	// config load, and measuring it costs a whole throwaway unit — far
	// too much to repeat on Caddy's critical path, which is what
	// App.Start used to do on every reload. It is measured at most once
	// per connection: a manager restart forces a redial, which is the
	// event the generation exists to catch.
	//
	// What the probe measures is the kernel and the LSM rather than
	// the manager, so a sysctl or an LSM policy reload can move the
	// answer with the connection still up. That is deliberately not
	// chased: the verdict is a start-time input (App.Start freezes a
	// tier into each app's spec and every later deploy reads that
	// field), so a measurement taken at the start that resolved it is
	// the honest one to hold. A host whose namespace policy changes
	// underneath a running hotserve is re-measured on the next dial —
	// a manager restart, or hotserve's own.
	//
	// Its own mutex, not mu: a measurement takes a unit start plus its
	// exit, and holding the mutex every D-Bus call needs for that long
	// would stall the runner.
	sandboxMu  sync.Mutex
	sandboxCap sandboxCapability
	sandboxGen uint64 // generation sandboxCap holds for; 0 = never measured
}

// userManager is the process-wide client every systemdRunner shares.
var userManager = &userManagerClient{}

// sandboxCapability reports what this manager can deliver, measuring
// at most once per connection (see the cache fields above). The
// measurement starts a real unit, so the first caller after a dial
// pays for it and every later config load reads the cache.
//
// probe() rather than get(): it proves the manager answers a real
// request, and its error names the uid, the socket and the lingering
// to enable. resolveSandboxTier puts that reason verbatim into the
// `sandbox require` refusal and the `auto` WARN, so a manager that
// went away between Start's probeManager and here must not be
// reported as a sandbox problem with no remedy attached.
func (c *userManagerClient) sandboxCapability(logger *zap.Logger) sandboxCapability {
	if err := c.probe(); err != nil {
		return sandboxCapability{tier: sandboxNone, reason: err.Error()}
	}
	return c.cachedSandboxCapability(func() sandboxCapability {
		r := newSystemdRunner(c, logger)
		defer r.cancel()
		return probeSandboxCapability(r)
	})
}

// cachedSandboxCapability returns the measurement held for the current
// connection, taking a fresh one via measure when the cache is empty
// or belongs to an older one. Separate from sandboxCapability so the
// caching rule is testable without a manager to dial.
//
// Only a capability the host actually delivered is cached. A `none`
// verdict is not a measurement of the host so much as the absence of
// one: probeSandboxCapability reports the same thing whether the
// namespaces are genuinely unavailable or the probe unit merely timed
// out under boot load, and caching that would pin every app
// unsandboxed — or, under `require`, refuse the server — for the life
// of a connection that nothing will drop. So a failed verdict is
// reported and re-measured next time, which is what this code did
// before the cache existed. The cost of re-measuring falls only on
// hosts that cannot sandbox at all; the supported one pays it once.
func (c *userManagerClient) cachedSandboxCapability(measure func() sandboxCapability) sandboxCapability {
	c.sandboxMu.Lock()
	defer c.sandboxMu.Unlock()
	// Read under the lock: sampled outside it, a redial between the
	// read and the lock would let a cache hit serve a measurement of a
	// manager that no longer exists.
	gen := c.generation.Load()
	if gen != 0 && c.sandboxGen == gen {
		return c.sandboxCap
	}
	got := measure()
	// Cache only against a real connection, and only if that
	// connection is still the current one: a redial while the probe
	// ran means it described a manager that is no longer live. The
	// caller that asked still gets what was measured.
	if got.tier != sandboxNone && gen != 0 && c.generation.Load() == gen {
		c.sandboxCap, c.sandboxGen = got, gen
	}
	return got
}

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
	var nofile uint64 // 0 = unknown: leave units' limits alone
	if v, err := conn.GetManagerProperty("DefaultLimitNOFILE"); err == nil {
		nofile = parseManagerUint(v)
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
		conn.Close() // another caller won the redial; its limit stands
		return c.conn, nil
	}
	c.conn = conn
	c.nofile.Store(nofile) // published with the connection it was read from
	c.generation.Add(1)    // ... as is the generation the sandbox cache keys on
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
	props := []sddbus.Property{
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
	return append(props, sandboxProperties(u.Sandbox)...)
}

// D-Bus shapes of the sandbox properties, as systemd's own
// bus-unit-util renders the unit-file syntax: list-valued path
// options are "(ssbt)" bind tuples or "a(ss)" (path, options) pairs,
// the allow-list options are "(bas)", and the flag/capability sets
// are uint64 masks.
type (
	bindMount struct {
		Source, Destination string
		IgnoreENOENT        bool
		Flags               uint64 // mount flags: mountRecursive for rbind
	}
	tmpfsMount struct{ Path, Options string }
	allowList  struct {
		AllowList bool
		Names     []string
	}
)

const (
	mountRecursive uint64 = 0x4000 // MS_REC: BindPaths=' default "rbind"
	errnoEPERM     int32  = 1      // SystemCallErrorNumber=EPERM, every Linux arch
)

// sandboxProperties renders a sandboxSpec (nil: nothing). The set is
// the one measured in issue #35; see sandbox.go for what it closes.
// There is one tier above none, so a spec that reaches past the guard
// below is a full one and gets every property unconditionally.
//
// The view is deny-by-default: TemporaryFileSystem=/ replaces the
// whole filesystem with an empty read-only tmpfs, and the binds below
// are the only things that exist inside the unit. There is no
// InaccessiblePaths= and no ProtectSystem= because there is nothing
// left for either to act on — an unnamed path is absent, not merely
// unreadable, which is a stronger statement than either option makes
// and one that cannot go stale. MountAPIVFS= is not set: /proc, /sys
// and /dev are mounted inside the tmpfs by PrivateDevices= and
// PrivatePIDs=, measured on 252/255/257/259.
func sandboxProperties(s *sandboxSpec) []sddbus.Property {
	if s == nil || s.tier == sandboxNone {
		return nil
	}
	writable := make([]bindMount, 0, len(s.writable))
	for _, b := range s.writable {
		src := b.source
		if src == "" {
			src = b.dest
		}
		writable = append(writable, bindMount{Source: src, Destination: b.dest, Flags: mountRecursive})
	}
	// The base view first: every entry optional. Debian 13 is merged-/usr
	// and has no /etc/pki, so several names are aliases or simply absent
	// — and /run/systemd/resolve exists only where resolved runs. A
	// missing entry must not fail the unit (see sandboxBaseView).
	readOnly := make([]bindMount, 0, len(sandboxBaseView))
	for _, p := range sandboxBaseView {
		readOnly = append(readOnly, bindMount{Source: p, Destination: p, IgnoreENOENT: true, Flags: mountRecursive})
	}
	props := []sddbus.Property{
		// Explicit on purpose: the mount options below do not imply it
		// (without it the unit fails to exec), and it is the piece that
		// closes /proc reads of processes outside the namespace.
		{Name: "PrivateUsers", Value: godbus.MakeVariant(true)},
		{Name: "PrivateTmp", Value: godbus.MakeVariant(true)},
		{Name: "PrivateDevices", Value: godbus.MakeVariant(true)},
		// Read-only cgroupfs: the delegated subtree is owned by the
		// app's own uid, so resource caps are otherwise rewritable.
		{Name: "ProtectControlGroups", Value: godbus.MakeVariant(true)},
		{Name: "ProtectKernelTunables", Value: godbus.MakeVariant(true)},
		{Name: "ProtectKernelModules", Value: godbus.MakeVariant(true)},
		{Name: "ProtectKernelLogs", Value: godbus.MakeVariant(true)},
		// RestrictNamespaces=yes: the allowed set is empty.
		{Name: "RestrictNamespaces", Value: godbus.MakeVariant(uint64(0))},
		{Name: "RestrictRealtime", Value: godbus.MakeVariant(true)},
		{Name: "RestrictSUIDSGID", Value: godbus.MakeVariant(true)},
		{Name: "LockPersonality", Value: godbus.MakeVariant(true)},
		// AF_NETLINK stays: getifaddrs() — Node's os.networkInterfaces(),
		// Go's net.Interfaces(), which frameworks call at startup — needs
		// a read-only netlink socket, and with an empty capability set
		// netlink cannot change anything.
		{Name: "RestrictAddressFamilies", Value: godbus.MakeVariant(allowList{true, []string{"AF_INET", "AF_INET6", "AF_UNIX", "AF_NETLINK"}})},
		{Name: "SystemCallFilter", Value: godbus.MakeVariant(allowList{true, []string{"@system-service"}})},
		{Name: "SystemCallErrorNumber", Value: godbus.MakeVariant(errnoEPERM)},
		{Name: "CapabilityBoundingSet", Value: godbus.MakeVariant(uint64(0))},
		// Start from nothing. The two bind properties appended below
		// put back the OS the app needs to run and the app's own
		// directories, and between them they are the whole view.
		{Name: "TemporaryFileSystem", Value: godbus.MakeVariant([]tmpfsMount{{"/", "ro"}})},
		// The manager hands every unit its own XDG_RUNTIME_DIR and bus
		// address; both point into a /run/user that is not in the view.
		{Name: "UnsetEnvironment", Value: godbus.MakeVariant([]string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"})},
	}
	// BindPaths= and BindReadOnlyPaths= are two views of ONE list in the
	// manager, and setting either to an empty array does not mean "add
	// nothing" — it resets the whole list, read-only entries included.
	// Emitting an empty BindPaths= after the base view would therefore
	// take /usr and /bin away again and every unit would fail
	// 203/EXEC — measured, and exactly what the capability probe (which
	// has no directories of its own) used to do. Omit what is empty.
	props = appendIfAny(props, "BindReadOnlyPaths", readOnly)
	props = appendIfAny(props, "BindPaths", writable)
	props = append(props, sddbus.Property{Name: "PrivatePIDs", Value: godbus.MakeVariant("yes")})
	return props
}

// appendIfAny adds a list-valued property only when it has entries;
// see the reset semantics noted in sandboxProperties.
func appendIfAny(props []sddbus.Property, name string, binds []bindMount) []sddbus.Property {
	if len(binds) == 0 {
		return props
	}
	return append(props, sddbus.Property{Name: name, Value: godbus.MakeVariant(binds)})
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
		StopTimeout:    usecDuration(propInt(props, "TimeoutStopUSec")),
	}
}

// usecDuration converts a systemd USec property; "infinity" (uint64
// max, negative once narrowed) and anything that would overflow a
// Duration read as unknown (0) rather than as a bogus budget.
func usecDuration(usec int) time.Duration {
	if usec <= 0 || usec > int(time.Duration(1<<62)/time.Microsecond) {
		return 0
	}
	return time.Duration(usec) * time.Microsecond
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
