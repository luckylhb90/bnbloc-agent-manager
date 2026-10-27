// Package manager is the agent manager's engine: which agents exist (the
// signed catalog), what is on this computer, and the install, update and
// registration steps. Design record: docs/prd/2026-10-24-agent-manager.md in
// bnbloc.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Actions a member (or the page) asks for.
const (
	ActInstall = "install"
	// ActInstallSandbox puts the agent in the member's sandbox instead of on
	// this computer (sandbox.go).
	ActInstallSandbox = "install-sandbox"
	ActUpdate         = "update"
	ActRollback       = "rollback"
	ActRemove         = "remove"
	ActCheck          = "check"
	ActRegister       = "register"
	ActUnregister     = "unregister"
)

// Service owns the state of every agent this computer's member manages.
type Service struct {
	Catalog *Catalog
	Store   Store
	// SettingsFile is the platform's component settings file ("" = none).
	SettingsFile string
	// ACPHome is the ACP core's home ("" = nothing is registered).
	ACPHome string
	// MountRoot is the folder the desktop gives this app inside the workspaces
	// root (the one folder the sandbox machine shares), MountsFile the file the
	// bridge reads the sandbox mounts from and GuestBase the folder of the guest
	// the agents appear under. All three empty: the app is not under the desktop,
	// and no agent can be put in the sandbox.
	MountRoot, MountsFile, GuestBase string
	// RegistryURL is the npm registry (https only; "" = DefaultRegistry).
	RegistryURL string
	HTTP        *http.Client
	// Version is this component's version, shown on the page.
	Version string
	// StallWindow ends an operation with no activity (0 = DefaultStall).
	StallWindow time.Duration
	// CheckEvery is the look for newer versions (0 = six hours).
	CheckEvery time.Duration
	// Now is the clock (nil = time.Now).
	Now func() time.Time

	// Settings are the settings as last read.
	Settings Settings
	// eff is the catalog in effect: Catalog with the agents the settings add
	// (config.go). Read through cat().
	eff atomic.Pointer[Catalog]

	mu          sync.Mutex
	state       State
	det         map[string]Detected
	detectedAt  time.Time
	ops         map[string]*operation
	autoTried   map[string]string
	sizes       map[string]sizeEntry
	settingsErr string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Init loads the state, sweeps what an interrupted install left and reads the
// settings. A state file that cannot be read stops the start: it records what
// was installed and is never replaced by an empty one.
func (s *Service) Init() error {
	st, err := s.Store.Load()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.state = st
	s.det = map[string]Detected{}
	s.ops = map[string]*operation{}
	s.autoTried = map[string]string{}
	s.mu.Unlock()
	s.Store.SweepPartials()
	s.loadSettings()
	return nil
}

func (s *Service) loadSettings() {
	set, eff, err := ReadSettings(s.SettingsFile, s.Catalog)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.settingsErr = err.Error()
		return
	}
	s.settingsErr = ""
	s.Settings = set
	s.eff.Store(eff)
}

// cat is the catalog in effect: what the build carries, and the agents the
// settings added.
func (s *Service) cat() *Catalog {
	if c := s.eff.Load(); c != nil {
		return c
	}
	return s.Catalog
}

// agentState returns the record of an agent, made when absent. Caller holds
// the lock.
func (s *Service) agentState(id string) *AgentState {
	a := s.state.Agents[id]
	if a == nil {
		a = &AgentState{}
		s.state.Agents[id] = a
	}
	return a
}

// mutate changes the state under the lock and saves it.
func (s *Service) mutate(f func(st *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.state)
	return s.Store.Save(s.state)
}

// RefreshDetected looks at the computer again for every agent.
func (s *Service) RefreshDetected(ctx context.Context) {
	found := map[string]Detected{}
	for i := range s.cat().Agents {
		e := &s.cat().Agents[i]
		if e.RunsHere() {
			found[e.ID] = s.detect(ctx, e)
		}
	}
	s.mu.Lock()
	s.det = found
	s.detectedAt = s.now()
	s.mu.Unlock()
}

// Run keeps the picture current until ctx ends: a look at the computer every
// minute, a look for newer versions every CheckEvery, and, only when the
// member turned automatic updates on, the update of what the manager
// installed.
func (s *Service) Run(ctx context.Context) {
	s.RefreshDetected(ctx)
	_ = s.writeRegistration()
	every := s.CheckEvery
	if every <= 0 {
		every = 6 * time.Hour
	}
	detect := time.NewTicker(time.Minute)
	defer detect.Stop()
	check := time.NewTimer(3 * time.Second)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-detect.C:
			s.loadSettings()
			s.RefreshDetected(ctx)
		case <-check.C:
			s.CheckAll(ctx)
			s.gcAll() // versions a session held are released once it ends
			s.autoUpdate()
			check.Reset(every)
		}
	}
}

// CheckAll looks for a newer version of every agent that has a source.
func (s *Service) CheckAll(ctx context.Context) {
	for i := range s.cat().Agents {
		e := &s.cat().Agents[i]
		if e.RunsHere() && e.packageIn(s.checkPlacement(e)) != "" {
			if err := s.check(ctx, e); err != nil {
				log.Printf("agent-manager: check %s: %v", e.ID, err)
			}
		}
	}
}

// autoUpdate updates what the manager installed, when the member allows it
// and nothing stops it: never an agent whose update failed to the same target
// (no retry loop), a pinned one, or one the organization does not offer.
func (s *Service) autoUpdate() {
	s.mu.Lock()
	on := s.Settings.AutoUpdate
	s.mu.Unlock()
	if !on {
		return
	}
	for i := range s.cat().Agents {
		e := &s.cat().Agents[i]
		if s.placementOf(e.ID) == PlaceSandbox {
			continue // a conversation resumes with the new version: only on a click
		}
		v := s.agentView(e)
		if v.Managed == nil || !v.Can.Update || !v.UpdateAvailable || v.Op != nil && v.Op.Running {
			continue
		}
		s.mu.Lock()
		tried := s.autoTried[e.ID] == v.Target
		s.autoTried[e.ID] = v.Target
		s.mu.Unlock()
		if tried {
			continue
		}
		if err := s.Begin(e.ID, ActUpdate); err != nil {
			log.Printf("agent-manager: automatic update of %s: %v", e.ID, err)
		}
	}
}

// ErrBusy is returned when an agent already has an operation running.
var ErrBusy = errors.New("another operation is running for this agent")

// Begin starts an action in the background and returns at once; the page
// reads the outcome from the snapshot.
func (s *Service) Begin(id, action string) error {
	e, ok := s.cat().Entry(id)
	if !ok {
		return fmt.Errorf("no agent %q in the catalog", id)
	}
	ctx, op, err := s.begin(e, action)
	if err != nil {
		return err
	}
	go func() { s.finish(e, op, s.perform(ctx, op, e, action)) }()
	return nil
}

// Do runs an action to its end (the command line).
func (s *Service) Do(ctx context.Context, id, action string, onLine func(string)) error {
	e, ok := s.cat().Entry(id)
	if !ok {
		return fmt.Errorf("no agent %q in the catalog", id)
	}
	ctx, op, err := s.beginIn(ctx, e, action)
	if err != nil {
		return err
	}
	if onLine != nil {
		stop := make(chan struct{})
		defer close(stop)
		go followLog(op, onLine, stop)
	}
	err = s.perform(ctx, op, e, action)
	s.finish(e, op, err)
	return err
}

func followLog(op *operation, out func(string), stop <-chan struct{}) {
	sent := 0
	flush := func() {
		op.mu.Lock()
		lines := append([]string(nil), op.log[min(sent, len(op.log)):]...)
		sent = len(op.log)
		op.mu.Unlock()
		for _, l := range lines {
			out(l)
		}
	}
	for {
		select {
		case <-stop:
			flush()
			return
		case <-time.After(150 * time.Millisecond):
			flush()
		}
	}
}

func (s *Service) begin(e *Entry, action string) (context.Context, *operation, error) {
	return s.beginIn(context.Background(), e, action)
}

func (s *Service) beginIn(parent context.Context, e *Entry, action string) (context.Context, *operation, error) {
	if !slices.Contains([]string{ActInstall, ActInstallSandbox, ActUpdate, ActRollback, ActRemove, ActCheck, ActRegister, ActUnregister}, action) {
		return nil, nil, fmt.Errorf("unknown action %q", action)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.ops[e.ID]; old != nil && old.running {
		return nil, nil, ErrBusy
	}
	stall := s.StallWindow
	if stall <= 0 {
		stall = DefaultStall
	}
	ctx, cancel := context.WithCancel(parent)
	op := newOperation(action, stall, cancel, s.now())
	s.ops[e.ID] = op
	return ctx, op, nil
}

func (s *Service) finish(e *Entry, op *operation, err error) {
	op.finish(err, s.now())
	op.cancel()
	s.mu.Lock()
	delete(s.sizes, e.ID)
	s.mu.Unlock()
	if err != nil {
		log.Printf("agent-manager: %s %s: %v", op.kind, e.ID, err)
	}
	// What is installed may have changed: look again, and tell the ACP core.
	if op.kind != ActCheck {
		s.RefreshDetected(context.Background())
		if werr := s.writeRegistration(); werr != nil {
			log.Printf("agent-manager: registration: %v", werr)
		}
	}
}

// Cancel ends the agent's running operation (the member's button).
func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	op := s.ops[id]
	s.mu.Unlock()
	if op == nil || !op.running {
		return errors.New("nothing is running for this agent")
	}
	op.canceled.Store(true)
	op.cancel()
	return nil
}

// gate refuses an action on an agent that is not for this computer or this
// organization.
func (s *Service) gate(e *Entry) error {
	if !e.RunsHere() {
		return fmt.Errorf("%s does not run on this operating system", e.Name)
	}
	s.mu.Lock()
	ok := s.Settings.Allows(e.ID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("your organization does not offer %s", e.Name)
	}
	return nil
}

func (s *Service) perform(ctx context.Context, op *operation, e *Entry, action string) error {
	if action == ActCheck {
		if e.packageIn(s.checkPlacement(e)) == "" {
			return fmt.Errorf("%s has no place to look for a newer version", e.Name)
		}
		return s.check(ctx, e)
	}
	if action != ActRemove && action != ActUnregister {
		if err := s.gate(e); err != nil {
			return err
		}
	}
	switch action {
	case ActInstall:
		return s.doInstall(ctx, op, e)
	case ActInstallSandbox:
		return s.installInSandbox(ctx, op, e)
	case ActUpdate:
		return s.doUpdate(ctx, op, e)
	case ActRollback:
		return s.doRollback(op, e)
	case ActRemove:
		return s.doRemove(op, e)
	case ActRegister, ActUnregister:
		return s.doRegister(op, e, action == ActRegister)
	}
	return nil
}

// target is the version an install or update moves to: the organization's
// pin, else the newest the registry offers (after the hold); "" for an agent
// that has no registry and updates itself.
func (s *Service) target(ctx context.Context, e *Entry) (string, error) {
	return s.targetIn(ctx, e, s.checkPlacement(e))
}

// checkPlacement is the placement a look for a newer version is about: where
// the manager's install lives, else this computer (the sandbox only once chosen).
func (s *Service) checkPlacement(e *Entry) string {
	if p := s.placementOf(e.ID); p != "" {
		return p
	}
	if e.Package() == "" && e.Sandbox != nil {
		return PlaceSandbox
	}
	return PlaceHost
}

func (s *Service) targetIn(ctx context.Context, e *Entry, placement string) (string, error) {
	s.mu.Lock()
	pin, hold := s.Settings.Pinned[e.ID], s.Settings.HoldDays
	s.mu.Unlock()
	if pin != "" {
		return pin, nil
	}
	pkg := e.packageIn(placement)
	if pkg == "" {
		// An agent with no registry (it updates itself) has no target to name.
		return "", nil
	}
	v, err := s.latestFromRegistry(ctx, pkg, hold)
	if err != nil {
		return "", err
	}
	return v, nil
}

// check looks for the newest version and records it, or why it could not.
func (s *Service) check(ctx context.Context, e *Entry) error {
	v, err := s.target(ctx, e)
	_ = s.mutate(func(st *State) {
		a := s.agentStateIn(st, e.ID)
		a.CheckedAt = s.now()
		if err != nil {
			a.CheckErr = err.Error()
			return
		}
		a.CheckErr, a.Latest = "", v
	})
	return err
}

func (s *Service) agentStateIn(st *State, id string) *AgentState {
	a := st.Agents[id]
	if a == nil {
		a = &AgentState{}
		st.Agents[id] = a
	}
	return a
}

func (s *Service) managedVersion(id string) (cur, prev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.state.Agents[id]; a != nil {
		return a.Version, a.Previous
	}
	return "", ""
}

func (s *Service) doInstall(ctx context.Context, op *operation, e *Entry) error {
	if e.Install == nil && e.InstallRun != nil {
		return s.runInstaller(ctx, op, e)
	}
	if e.Install == nil {
		if e.InstallHint != "" {
			return errors.New("the manager does not install " + e.Name + ". " + e.InstallHint)
		}
		return errors.New("the manager does not install " + e.Name)
	}
	if cur, _ := s.managedVersion(e.ID); cur != "" {
		if s.placementOf(e.ID) == PlaceSandbox {
			return fmt.Errorf("%s %s is already installed in your sandbox; remove it first to put it on this computer", e.Name, cur)
		}
		return fmt.Errorf("%s %s is already installed; use update", e.Name, cur)
	}
	if s.sandboxOnly(e.ID) {
		return fmt.Errorf("your organization installs %s only in the sandbox", e.Name)
	}
	v, err := s.target(ctx, e)
	if err != nil {
		return err
	}
	op.logf("installing %s %s", e.Name, v)
	if _, err := s.npmInstall(ctx, op, e, v); err != nil {
		return err
	}
	return s.mutate(func(st *State) {
		a := s.agentStateIn(st, e.ID)
		a.Version, a.Previous, a.Method, a.InstalledAt = v, "", "npm", s.now()
		a.Registered = e.Register != nil
		a.Latest, a.CheckErr = v, ""
	})
}

// runInstaller runs a member's own install line for an agent not on their
// computer, then looks again: what it installed must be found.
func (s *Service) runInstaller(ctx context.Context, op *operation, e *Entry) error {
	path, err := exec.LookPath(e.InstallRun.Command)
	if err != nil {
		return fmt.Errorf("%s is needed to install %s and was not found", e.InstallRun.Command, e.Name)
	}
	op.logf("installing %s", e.Name)
	if err := s.run(ctx, op, path, e.InstallRun.Args...); err != nil {
		return err
	}
	d := s.detect(ctx, e)
	if d.Command == "" {
		return fmt.Errorf("the installer finished, but %s is not on the PATH this app sees: add its folder to your PATH (a new terminal is not enough) and restart the app", e.Detect.Command)
	}
	op.logf("%s is now at %s", e.Name, d.Path)
	return nil
}

func (s *Service) doUpdate(ctx context.Context, op *operation, e *Entry) error {
	cur, _ := s.managedVersion(e.ID)
	v, err := s.target(ctx, e)
	if err != nil {
		return err
	}
	if cur != "" {
		return s.updateManaged(ctx, op, e, cur, v)
	}
	return s.updateExternal(ctx, op, e, v)
}

// updateManaged installs the new version beside the old one, verifies it, and
// only then points the registration at it; the old one stays for a rollback.
// A running agent keeps running from the old directory: nothing is signalled.
func (s *Service) updateManaged(ctx context.Context, op *operation, e *Entry, cur, v string) error {
	if v == cur {
		op.logf("%s is already at %s", e.Name, cur)
		return nil
	}
	if s.placementOf(e.ID) == PlaceSandbox {
		return s.updateInSandbox(ctx, op, e, cur, v)
	}
	if e.Install == nil {
		return errors.New("the manager does not install " + e.Name)
	}
	op.logf("updating %s %s to %s", e.Name, cur, v)
	if _, err := s.npmInstall(ctx, op, e, v); err != nil {
		return err
	}
	if err := s.mutate(func(st *State) {
		a := s.agentStateIn(st, e.ID)
		a.Version, a.Previous, a.InstalledAt = v, cur, s.now()
		a.Latest, a.CheckErr = v, ""
	}); err != nil {
		return err
	}
	s.gc(e.ID)
	return nil
}

// updateExternal updates an install the member made, only by the method the
// catalog names for how it was found, and verifies the result.
func (s *Service) updateExternal(ctx context.Context, op *operation, e *Entry, target string) error {
	s.mu.Lock()
	d := s.det[e.ID]
	pin := s.Settings.Pinned[e.ID]
	s.mu.Unlock()
	if d.Command == "" {
		return fmt.Errorf("%s was not found on this computer", e.Name)
	}
	if d.Kind == KindNPMGlobal && e.Install != nil {
		op.logf("updating %s (installed with npm) to %s", e.Name, target)
		if err := s.npmGlobalUpdate(ctx, op, e, target); err != nil {
			return err
		}
		return s.expectVersion(ctx, op, e, target)
	}
	for _, u := range e.Update {
		if u.Kind != d.Kind {
			continue
		}
		args, ok := selfArgs(u, pin)
		if !ok {
			return fmt.Errorf("your organization pinned %s to %s, and this install cannot be set to a chosen version", e.Name, pin)
		}
		program := d.Command
		if u.Command != "" {
			path, err := exec.LookPath(u.Command)
			if err != nil {
				return fmt.Errorf("%s is needed to update %s and was not found", u.Command, e.Name)
			}
			program = path
		}
		op.logf("updating %s", e.Name)
		before := d.Version
		if err := s.run(ctx, op, program, args...); err != nil {
			return err
		}
		after := s.detect(ctx, e).Version
		switch {
		case after == "":
			op.logf("the updater finished; the version could not be read")
		case after == before:
			op.logf("the updater finished; the version is unchanged (%s)", after)
		default:
			op.logf("%s is now %s", e.Name, after)
		}
		return nil
	}
	if d.Owner != "" {
		return fmt.Errorf("this install of %s is updated by %s", e.Name, d.Owner)
	}
	return fmt.Errorf("this install of %s was not made by the manager and has no updater it can run", e.Name)
}

func (s *Service) expectVersion(ctx context.Context, op *operation, e *Entry, want string) error {
	if len(e.Detect.Args) == 0 {
		return nil
	}
	got := s.detect(ctx, e).Version
	if got != want {
		if got == "" {
			got = "none"
		}
		return fmt.Errorf("after the update the command reports %s, not %s", got, want)
	}
	op.logf("%s is now %s", e.Name, got)
	return nil
}

func (s *Service) doRollback(op *operation, e *Entry) error {
	cur, prev := s.managedVersion(e.ID)
	if cur == "" || prev == "" {
		return errors.New("there is no earlier version to go back to")
	}
	if _, err := os.Stat(s.storeOf(e).versionDir(e.ID, prev)); err != nil {
		return fmt.Errorf("the earlier version %s is gone from disk", prev)
	}
	op.logf("going back from %s to %s", cur, prev)
	return s.mutate(func(st *State) {
		a := s.agentStateIn(st, e.ID)
		a.Version, a.Previous = prev, cur
	})
}

func (s *Service) doRemove(op *operation, e *Entry) error {
	cur, _ := s.managedVersion(e.ID)
	if cur == "" {
		return errors.New("the manager did not install this agent and does not remove it")
	}
	if s.placementOf(e.ID) == PlaceSandbox {
		if s.heldInSandbox(e) {
			return fmt.Errorf("a room may still be running %s: removing it now would end that conversation. Remove it when no room uses it (the app restarting ends them)", e.Name)
		}
	} else if held := s.heldVersions(e.ID); len(held) > 0 {
		return fmt.Errorf("a room is still running %s %s: removing it now would end that conversation. A room keeps its agent until the app restarts, so remove it then", e.Name, strings.Join(held, ", "))
	}
	op.logf("removing %s", e.Name)
	store := s.storeOf(e) // before the record that says where it lives is cleared
	if err := s.mutate(func(st *State) {
		a := s.agentStateIn(st, e.ID)
		a.Version, a.Previous, a.Method, a.Registered = "", "", "", false
		a.InstalledAt = time.Time{}
	}); err != nil {
		return err
	}
	if err := os.RemoveAll(store.agentDir(e.ID)); err != nil {
		return fmt.Errorf("some files could not be removed (an agent may be running from them); try again later: %w", err)
	}
	return nil
}

func (s *Service) doRegister(op *operation, e *Entry, on bool) error {
	if e.Register == nil {
		return fmt.Errorf("%s has no ACP launch line, so rooms cannot run it", e.Name)
	}
	if on {
		s.mu.Lock()
		cur := ""
		if a := s.state.Agents[e.ID]; a != nil {
			cur = a.Version
		}
		have := cur != "" || s.det[e.ID].Command != ""
		s.mu.Unlock()
		if !have {
			return fmt.Errorf("%s is not installed", e.Name)
		}
	}
	op.logf("%s %s for rooms", map[bool]string{true: "enabling", false: "disabling"}[on], e.Name)
	return s.mutate(func(st *State) { s.agentStateIn(st, e.ID).Registered = on })
}

// commandEnv is the environment of a command the manager runs: the process's
// own (the desktop gives it the member's login-shell environment), with the
// npm update notifier off.
func (s *Service) commandEnv() []string {
	return append(os.Environ(), "npm_config_update_notifier=false", "NO_UPDATE_NOTIFIER=1")
}

// WriteRegistration brings the ACP core's drop-in up to date (the command
// line calls it after an action).
func (s *Service) WriteRegistration() error { return s.writeRegistration() }
