package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Gentleman-Programming/engram/v2/internal/cloud/autosync"
	"github.com/Gentleman-Programming/engram/v2/internal/server"
	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

// autosyncStatusProvider is the interface subset of autosync.Manager used by the adapter.
// This avoids importing the full Manager type and allows test fakes.
type autosyncStatusProvider interface {
	Status() autosync.Status
}

// autosyncStatusAdapter implements server.SyncStatusProvider by mapping
// autosync.Manager phases to server.SyncStatus. When mgr is nil (autosync
// disabled), it falls back to the storeSyncStatusProvider.
//
// REQ-209, REQ-cloud-sync-status.
type autosyncStatusAdapter struct {
	mgr      autosyncStatusProvider
	fallback server.SyncStatusProvider
	// defaultProject is the project an unscoped status request reports, so it
	// is answered by the manager that owns that project.
	defaultProject string
}

// Status implements server.SyncStatusProvider.
func (a *autosyncStatusAdapter) Status(project string) server.SyncStatus {
	if a.mgr == nil {
		if a.fallback != nil {
			return a.fallback.Status(project)
		}
		return server.SyncStatus{Phase: "idle"}
	}

	// A project no running manager syncs (routed to a remote that could not
	// start) reports the persisted store status.
	st, owned := a.statusFor(project)
	if !owned {
		if a.fallback != nil {
			return a.fallback.Status(project)
		}
		return server.SyncStatus{Phase: "idle"}
	}

	// Get upgrade-stage overlay from fallback (these come from the store, not autosync).
	var upgradeStage, upgradeCode, upgradeMsg string
	if a.fallback != nil {
		fb := a.fallback.Status(project)
		upgradeStage = fb.UpgradeStage
		upgradeCode = fb.UpgradeReasonCode
		upgradeMsg = fb.UpgradeReasonMessage
	}

	result := a.mapPhase(st)
	result.UpgradeStage = upgradeStage
	result.UpgradeReasonCode = upgradeCode
	result.UpgradeReasonMessage = upgradeMsg
	return result
}

// statusFor returns the status of the manager that owns project. A provider
// that does not route by project (a single legacy manager) owns every project.
func (a *autosyncStatusAdapter) statusFor(project string) (autosync.Status, bool) {
	if routed, ok := a.mgr.(projectAutosyncStatusProvider); ok {
		if strings.TrimSpace(project) == "" {
			project = a.defaultProject
		}
		return routed.StatusForProject(project)
	}
	return a.mgr.Status(), true
}

// mapPhase converts an autosync.Status to a server.SyncStatus.
// Phase mapping per REQ-209:
//   - PhaseHealthy → healthy
//   - PhasePushing / PhasePulling / PhaseIdle → running
//   - PhasePushFailed / PhasePullFailed / PhaseBackoff → degraded + transport_failed
//   - PhaseDisabled → degraded + upgrade_paused
func (a *autosyncStatusAdapter) mapPhase(st autosync.Status) server.SyncStatus {
	base := server.SyncStatus{
		Enabled:             true,
		LastError:           st.LastError,
		ConsecutiveFailures: st.ConsecutiveFailures,
		BackoffUntil:        st.BackoffUntil,
		LastSyncAt:          st.LastSyncAt,
		// Phase E: propagate deferred/dead counts from autosync.Status.
		DeferredCount: st.DeferredCount,
		DeadCount:     st.DeadCount,
	}

	switch st.Phase {
	case autosync.PhaseHealthy:
		base.Phase = "healthy"
		base.ReasonCode = ""

	case autosync.PhasePushing, autosync.PhasePulling, autosync.PhaseIdle:
		base.Phase = "running"

	case autosync.PhasePushFailed, autosync.PhasePullFailed, autosync.PhaseBackoff:
		base.Phase = "degraded"
		// BW5: Pass through specific reason codes (auth_required, policy_forbidden)
		// set by the manager; fall back to "transport_failed" for generic failures.
		if st.ReasonCode != "" {
			base.ReasonCode = st.ReasonCode
		} else {
			base.ReasonCode = "transport_failed"
		}
		if st.ReasonMessage != "" {
			base.ReasonMessage = st.ReasonMessage
		} else {
			base.ReasonMessage = st.LastError
		}

	case autosync.PhaseDisabled:
		base.Phase = "degraded"
		base.ReasonCode = "upgrade_paused"
		base.ReasonMessage = st.ReasonMessage

	default:
		base.Phase = st.Phase
		base.ReasonCode = st.ReasonCode
		base.ReasonMessage = st.ReasonMessage
	}

	return base
}

// projectAutosyncStatusProvider is implemented by providers that run one
// manager per cloud remote and can answer for the manager owning a project.
type projectAutosyncStatusProvider interface {
	StatusForProject(project string) (autosync.Status, bool)
}

type autosyncUpgradePauser interface {
	StopForUpgrade(project string) error
	ResumeAfterUpgrade(project string) error
}

type autosyncDirtyNotifier interface {
	NotifyDirty()
}

// autosyncGroup runs one autosync manager per cloud remote behind the single
// manager surface cmdServe and cmdMCP use. The global manager owns every
// project that is not routed to its own remote; routed projects belong to the
// manager of their remote, or to none when their remote could not start.
type autosyncGroup struct {
	global  startableAutosyncManager
	owners  map[string]startableAutosyncManager
	routed  map[string]struct{}
	members []startableAutosyncManager
}

func newAutosyncGroup() *autosyncGroup {
	return &autosyncGroup{
		owners: map[string]startableAutosyncManager{},
		routed: map[string]struct{}{},
	}
}

func (g *autosyncGroup) markRouted(project string) {
	g.routed[project] = struct{}{}
}

// routedProjects lists every project with a per-project override, sorted.
func (g *autosyncGroup) routedProjects() []string {
	out := make([]string, 0, len(g.routed))
	for project := range g.routed {
		out = append(out, project)
	}
	sort.Strings(out)
	return out
}

func (g *autosyncGroup) addGlobal(mgr startableAutosyncManager) {
	g.global = mgr
	g.members = append(g.members, mgr)
}

func (g *autosyncGroup) addRemote(mgr startableAutosyncManager, projects []string) {
	for _, project := range projects {
		g.owners[project] = mgr
	}
	g.members = append(g.members, mgr)
}

func (g *autosyncGroup) empty() bool {
	return len(g.members) == 0
}

// owner returns the manager that syncs project, or nil when none does.
func (g *autosyncGroup) owner(project string) startableAutosyncManager {
	project, _ = store.NormalizeProject(project)
	project = strings.TrimSpace(project)
	if mgr, ok := g.owners[project]; ok {
		return mgr
	}
	if _, routed := g.routed[project]; routed {
		return nil
	}
	return g.global
}

// start launches every manager. Startup handshake (CodeRabbit PR #1189): a
// manager supporting Start is launched through it so Stop always waits until
// its run loop registered; deterministic test fakes keep the goroutine launch.
func (g *autosyncGroup) start(ctx context.Context) {
	for _, mgr := range g.members {
		if starter, ok := mgr.(autosyncStartHandshake); ok {
			starter.Start(ctx)
		} else {
			go mgr.Run(ctx)
		}
	}
}

// Stop stops every manager so each releases its own sync lease.
func (g *autosyncGroup) Stop() {
	for _, mgr := range g.members {
		mgr.Stop()
	}
}

// Status reports the global manager, or the first remote manager when no
// global remote is configured.
func (g *autosyncGroup) Status() autosync.Status {
	if g.global != nil {
		return g.global.Status()
	}
	return g.members[0].Status()
}

// StatusForProject reports the manager owning project; false when no running
// manager syncs it.
func (g *autosyncGroup) StatusForProject(project string) (autosync.Status, bool) {
	mgr := g.owner(project)
	if mgr == nil {
		return autosync.Status{}, false
	}
	return mgr.Status(), true
}

// NotifyDirty wakes every manager; each only pushes its own projects.
func (g *autosyncGroup) NotifyDirty() {
	for _, mgr := range g.members {
		if notifier, ok := mgr.(autosyncDirtyNotifier); ok {
			notifier.NotifyDirty()
		}
	}
}

// StopForUpgrade pauses only the manager owning project.
func (g *autosyncGroup) StopForUpgrade(project string) error {
	pauser, err := g.upgradePauser(project)
	if err != nil {
		return err
	}
	return pauser.StopForUpgrade(project)
}

// ResumeAfterUpgrade resumes only the manager owning project.
func (g *autosyncGroup) ResumeAfterUpgrade(project string) error {
	pauser, err := g.upgradePauser(project)
	if err != nil {
		return err
	}
	return pauser.ResumeAfterUpgrade(project)
}

func (g *autosyncGroup) upgradePauser(project string) (autosyncUpgradePauser, error) {
	mgr := g.owner(project)
	if mgr == nil {
		return nil, fmt.Errorf("no autosync manager is running for project %q", project)
	}
	pauser, ok := mgr.(autosyncUpgradePauser)
	if !ok {
		return nil, fmt.Errorf("autosync manager for project %q does not support upgrade pause", project)
	}
	return pauser, nil
}
