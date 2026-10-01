package cloudstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// dashboardDeploymentScope is the set of projects the dashboard may expose for
// this deployment, before any per-principal grant filtering. all=true means no
// deployment-level restriction.
type dashboardDeploymentScope struct {
	all      bool
	projects map[string]struct{}
}

// effectiveDashboardScope computes the deployment scope as the env allowlist
// (ENGRAM_CLOUD_ALLOWED_PROJECTS) united with the projects an admin registered
// in cloud_project_controls. Registration never introduces a wildcard: only a
// literal "*" allowlist (envAll) lifts the restriction. An empty allowlist keeps
// the historical "no deployment filter" behavior unchanged.
func effectiveDashboardScope(envAll bool, envProjects map[string]struct{}, registered []string) dashboardDeploymentScope {
	if envAll || len(envProjects) == 0 {
		return dashboardDeploymentScope{all: true}
	}
	projects := make(map[string]struct{}, len(envProjects)+len(registered))
	for project := range envProjects {
		if name := strings.TrimSpace(project); name != "" {
			projects[name] = struct{}{}
		}
	}
	for _, project := range registered {
		if name := strings.TrimSpace(project); name != "" && name != "*" {
			projects[name] = struct{}{}
		}
	}
	return dashboardDeploymentScope{projects: projects}
}

func (s dashboardDeploymentScope) allows(project string) bool {
	if s.all {
		return true
	}
	_, ok := s.projects[strings.TrimSpace(project)]
	return ok
}

func (s dashboardDeploymentScope) sortedProjects() []string {
	names := make([]string, 0, len(s.projects))
	for name := range s.projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// dashboardScope returns the effective deployment scope for the supplied set of
// registered projects, combining it with the env allowlist held by cs.
func (cs *CloudStore) dashboardScope(registered []string) dashboardDeploymentScope {
	return effectiveDashboardScope(cs.dashboardAllowedAll, cs.dashboardAllowedScopes, registered)
}

// loadRegisteredDashboardProjects returns the projects registered by an admin
// in cloud_project_controls. Those names are stored in canonical
// NormalizeProjectGrant form by CreateProjectWithGrantAndAudit.
func (cs *CloudStore) loadRegisteredDashboardProjects() ([]string, error) {
	if cs == nil || cs.db == nil {
		return nil, fmt.Errorf("cloudstore: not initialized")
	}
	rows, err := cs.db.QueryContext(context.Background(), `SELECT project FROM cloud_project_controls ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("cloudstore: dashboard query registered projects: %w", err)
	}
	defer rows.Close()
	projects := make([]string, 0)
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return nil, fmt.Errorf("cloudstore: dashboard scan registered project: %w", err)
		}
		if project = strings.TrimSpace(project); project != "" {
			projects = append(projects, project)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cloudstore: dashboard iterate registered projects: %w", err)
	}
	return projects, nil
}

// withRegisteredProjects adds a zero-count project row and an empty detail for
// every registered project that has no synced data yet, so admin-created
// projects are listed before their first push. Projects that already have
// rows are left untouched. Callers apply the deployment scope afterwards.
func (m dashboardReadModel) withRegisteredProjects(registered []string) dashboardReadModel {
	added := false
	for _, project := range registered {
		project = strings.TrimSpace(project)
		if project == "" {
			continue
		}
		if _, exists := m.projectDetails[project]; exists {
			continue
		}
		if !added {
			m.projects = append([]DashboardProjectRow(nil), m.projects...)
			details := make(map[string]DashboardProjectDetail, len(m.projectDetails)+len(registered))
			for name, detail := range m.projectDetails {
				details[name] = detail
			}
			m.projectDetails = details
			added = true
		}
		row := DashboardProjectRow{Project: project}
		m.projects = append(m.projects, row)
		m.projectDetails[project] = DashboardProjectDetail{
			Project:      project,
			Stats:        row,
			Contributors: []DashboardContributorRow{},
			Sessions:     []DashboardSessionRow{},
			Observations: []DashboardObservationRow{},
			Prompts:      []DashboardPromptRow{},
		}
	}
	if !added {
		return m
	}
	sort.Slice(m.projects, func(i, j int) bool { return m.projects[i].Project < m.projects[j].Project })
	m.admin.Projects = len(m.projects)
	return m
}
