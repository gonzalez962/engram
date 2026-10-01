package cloudstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrProjectAlreadyExists is returned by CreateProjectWithGrantAndAudit when
// a row already exists in cloud_project_controls for the requested (normalized)
// project. Callers must map this to a 409 Conflict at the API boundary.
var ErrProjectAlreadyExists = errors.New("cloudstore: project already exists")

// ProjectSyncControl holds the per-project sync enable/pause record.
// The backing table is cloud_project_controls (Postgres, added in migrate()).
type ProjectSyncControl struct {
	Project      string
	SyncEnabled  bool
	PausedReason *string
	UpdatedAt    string
	UpdatedBy    *string
}

// IsProjectSyncEnabled returns whether sync is enabled for the project.
// An absent row defaults to enabled=true (safe default).
func (cs *CloudStore) IsProjectSyncEnabled(project string) (bool, error) {
	if cs == nil || cs.db == nil {
		return true, nil
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return true, nil
	}
	var enabled bool
	err := cs.db.QueryRowContext(
		context.Background(),
		`SELECT sync_enabled FROM cloud_project_controls WHERE project = $1`,
		project,
	).Scan(&enabled)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		// BW8: Fail closed — return (false, error) so callers that ignore the error
		// do not silently permit mutations for projects with unknown sync state.
		return false, fmt.Errorf("cloudstore: IsProjectSyncEnabled: %w", err)
	}
	return enabled, nil
}

// SetProjectSyncEnabled upserts the project sync control record.
func (cs *CloudStore) SetProjectSyncEnabled(project string, enabled bool, updatedBy, reason string) error {
	if cs == nil || cs.db == nil {
		return fmt.Errorf("cloudstore: not initialized")
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("cloudstore: SetProjectSyncEnabled: project is empty")
	}
	var reasonPtr *string
	if r := strings.TrimSpace(reason); r != "" {
		reasonPtr = &r
	}
	var updatedByPtr *string
	if u := strings.TrimSpace(updatedBy); u != "" {
		updatedByPtr = &u
	}
	_, err := cs.db.ExecContext(
		context.Background(),
		`INSERT INTO cloud_project_controls (project, sync_enabled, paused_reason, updated_by, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (project) DO UPDATE SET
		     sync_enabled  = EXCLUDED.sync_enabled,
		     paused_reason = EXCLUDED.paused_reason,
		     updated_by    = EXCLUDED.updated_by,
		     updated_at    = EXCLUDED.updated_at`,
		project, enabled, reasonPtr, updatedByPtr, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("cloudstore: SetProjectSyncEnabled: %w", err)
	}
	// W4: invalidate the dashboard read model cache so SystemHealth / paused_projects
	// reflect the change immediately without waiting for the next chunk write.
	cs.invalidateDashboardReadModel()
	return nil
}

// GetProjectSyncControl returns the control record for a project, or nil if absent.
func (cs *CloudStore) GetProjectSyncControl(project string) (*ProjectSyncControl, error) {
	if cs == nil || cs.db == nil {
		return nil, fmt.Errorf("cloudstore: not initialized")
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, nil
	}
	var ctrl ProjectSyncControl
	var updatedAt time.Time
	err := cs.db.QueryRowContext(
		context.Background(),
		`SELECT project, sync_enabled, paused_reason, updated_by, updated_at
		 FROM cloud_project_controls WHERE project = $1`,
		project,
	).Scan(&ctrl.Project, &ctrl.SyncEnabled, &ctrl.PausedReason, &ctrl.UpdatedBy, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cloudstore: GetProjectSyncControl: %w", err)
	}
	ctrl.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return &ctrl, nil
}

// ListProjectSyncControls returns all project controls UNION DISTINCT projects
// known from cloud_chunks (projects with no explicit control row default to enabled).
func (cs *CloudStore) ListProjectSyncControls() ([]ProjectSyncControl, error) {
	if cs == nil || cs.db == nil {
		return nil, fmt.Errorf("cloudstore: not initialized")
	}
	// UNION ensures projects that have synced chunks appear even without a control row.
	rows, err := cs.db.QueryContext(context.Background(), `
		SELECT
		    p.project,
		    COALESCE(c.sync_enabled, TRUE)   AS sync_enabled,
		    c.paused_reason,
		    c.updated_by,
		    c.updated_at
		FROM (
		    SELECT DISTINCT project_name AS project FROM cloud_chunks
		    UNION
		    SELECT project FROM cloud_project_controls
		) p
		LEFT JOIN cloud_project_controls c ON c.project = p.project
		ORDER BY p.project
	`)
	if err != nil {
		return nil, fmt.Errorf("cloudstore: ListProjectSyncControls: %w", err)
	}
	defer rows.Close()

	var result []ProjectSyncControl
	for rows.Next() {
		var ctrl ProjectSyncControl
		var updatedAt *time.Time
		if err := rows.Scan(
			&ctrl.Project,
			&ctrl.SyncEnabled,
			&ctrl.PausedReason,
			&ctrl.UpdatedBy,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("cloudstore: ListProjectSyncControls scan: %w", err)
		}
		if updatedAt != nil {
			ctrl.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		}
		result = append(result, ctrl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cloudstore: ListProjectSyncControls iterate: %w", err)
	}
	return result, nil
}

// CreateProjectWithGrantAndAudit persists a brand-new project together with
// its initial actor grant and the auth-audit event in a single Postgres
// transaction, mirroring the CreatePrincipalTokenWithAudit atomicity contract
// so callers never see a project row without its grant or its audit event.
//
// Failure modes:
//   - ErrProjectAlreadyExists when cloud_project_controls already has a row for
//     the normalized project (detected via ON CONFLICT DO NOTHING + zero rows
//     affected; the transaction is rolled back so no grant/audit rows are left
//     dangling).
//   - ErrAuthAuditInsertFailed when the audit insert fails (the project row and
//     the grant row are rolled back together with the audit insert).
//
// The project name is normalized via the same NormalizeProjectGrant pipeline
// used by CreateProjectGrant, so a caller passing "My_Project" receives a row
// keyed under "my_project" and the grant is stored under the same canonical
// form. Initial sync_enabled defaults to TRUE — pausing a freshly created
// project is a deliberate, separate admin action.
func (cs *CloudStore) CreateProjectWithGrantAndAudit(ctx context.Context, params CreateProjectGrantParams, audit AuthAuditEvent) error {
	if cs == nil || cs.db == nil {
		return fmt.Errorf("cloudstore: not initialized")
	}
	principalID := strings.TrimSpace(params.PrincipalID)
	if principalID == "" {
		return fmt.Errorf("cloudstore: principal id is required")
	}
	grantedBy := strings.TrimSpace(params.GrantedByPrincipalID)
	if grantedBy == "" {
		return fmt.Errorf("cloudstore: granted by principal id is required")
	}
	project := normalizeCloudProjectGrant(params.Project)
	if project == "" {
		return fmt.Errorf("cloudstore: project is required")
	}

	tx, err := cs.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cloudstore: begin project create tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Insert the project control row. ON CONFLICT DO NOTHING lets us detect
	// duplicates via RowsAffected() == 0 without exposing the underlying
	// pq/23505 unique-violation text to callers.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO cloud_project_controls (project, sync_enabled, paused_reason, updated_by, updated_at)
		VALUES ($1, TRUE, NULL, $2, $3)
		ON CONFLICT (project) DO NOTHING`,
		project, grantedBy, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("cloudstore: insert project control: %w", err)
	}
	affected, raErr := res.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("cloudstore: project control rows affected: %w", raErr)
	}
	if affected == 0 {
		// Roll back explicitly so any earlier tx activity (currently none,
		// but future-proof) is reverted before we surface the conflict.
		_ = tx.Rollback()
		return ErrProjectAlreadyExists
	}

	// Initial grant for the actor. ON CONFLICT keeps the call idempotent on
	// the grant side (re-running with the same principal+project pair is a
	// no-op), but the surrounding tx is gated on the project-create above so
	// this branch is only reachable on a successful first insert.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO cloud_project_grants (principal_id, project, granted_by_principal_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (principal_id, project) DO UPDATE SET granted_by_principal_id = EXCLUDED.granted_by_principal_id`,
		principalID, project, grantedBy,
	); err != nil {
		return fmt.Errorf("cloudstore: insert project grant: %w", err)
	}

	// Auth audit event. We project the normalized project name into the row
	// so the audit log is self-describing (the request payload's name may
	// differ from the canonical form when the caller passed an unnormalized
	// one).
	auditEvent := audit
	auditEvent.Project = project
	if err := insertAuthAuditEvent(ctx, tx, auditEvent); err != nil {
		return fmt.Errorf("%w: %w", ErrAuthAuditInsertFailed, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cloudstore: commit project create tx: %w", err)
	}
	return nil
}
