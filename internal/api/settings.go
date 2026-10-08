package api

import (
	"net/http"
	"runtime"

	"lakesync/internal/core/engine"
)

type generalSettings struct {
	DefaultSyncIntervalS int `json:"default_sync_interval_s"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	g := generalSettings{DefaultSyncIntervalS: s.cfg.DefaultSyncIntervalS}
	_, _ = s.st.GetSetting(r.Context(), "general", &g)
	m := engine.LoadMaintenanceSettings(r.Context(), s.st)
	ext, _ := s.q.Extensions(r.Context())
	s.writeJSON(w, 200, map[string]any{
		"general":     g,
		"maintenance": m,
		"limits": map[string]any{
			"min_sync_interval_s": s.cfg.MinSyncIntervalS,
			"query_row_limit":     s.cfg.QueryRowLimit,
			"query_timeout_s":     int(s.cfg.QueryTimeout.Seconds()),
		},
		"about": map[string]any{
			"version":         s.cfg.Version,
			"commit":          s.cfg.Commit,
			"go":              runtime.Version(),
			"duckdb":          s.q.Version(r.Context()),
			"extensions":      ext,
			"worker":          s.worker != nil,
			"egress_ip":       s.egress.get(),
			"private_allowed": s.cfg.AllowPrivateEndpoints,
		},
	})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		General     *generalSettings            `json:"general"`
		Maintenance *engine.MaintenanceSettings `json:"maintenance"`
	}
	if err := s.decode(r, &body); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	if body.General != nil {
		if body.General.DefaultSyncIntervalS < s.cfg.MinSyncIntervalS {
			s.badRequest(w, "The default sync interval is below the minimum.", "")
			return
		}
		if err := s.st.SetSetting(r.Context(), "general", body.General); err != nil {
			s.internal(w, err)
			return
		}
	}
	if body.Maintenance != nil {
		m := *body.Maintenance
		if m.SmallFileMB <= 0 || m.SmallFilesTrigger <= 0 || m.TargetFileMB <= 0 || m.DeleteFilesTrigger < 0 || m.SnapshotKeepDays <= 0 || m.SnapshotsKeepLast <= 0 || m.OrphanOlderDays <= 0 || m.CheckEveryMinutes <= 0 {
			s.badRequest(w, "Maintenance thresholds must be positive numbers.", "")
			return
		}
		if err := s.st.SetSetting(r.Context(), engine.SettingsKeyMaintenance, m); err != nil {
			s.internal(w, err)
			return
		}
	}
	s.getSettings(w, r)
}
