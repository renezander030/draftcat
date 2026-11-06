//go:build voice

package voice

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                 string          `json:"id"`
		CallerPhone        string          `json:"caller_phone"`
		Workflow           string          `json:"workflow"`
		StartedAt          int64           `json:"started_at_ms"`
		PreCallContextJSON json.RawMessage `json:"pre_call_context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.ID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if in.StartedAt == 0 {
		in.StartedAt = time.Now().UnixMilli()
	}
	// A refused call is not stored: it never appears as a completed call.
	if s.gate != nil {
		if err := s.gate.Admit(r.Context(), in.ID); err != nil {
			writeRefused(w, err)
			return
		}
	}
	if err := s.store.InsertSessionStart(SessionStart{
		ID:                 in.ID,
		CallerPhone:        in.CallerPhone,
		Workflow:           in.Workflow,
		StartedAt:          in.StartedAt,
		PreCallContextJSON: string(in.PreCallContextJSON),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeOK(w)
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string          `json:"session_id"`
		TS        int64           `json:"ts_ms"`
		Kind      string          `json:"kind"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.SessionID == "" || in.Kind == "" {
		http.Error(w, "session_id and kind required", http.StatusBadRequest)
		return
	}
	if in.TS == 0 {
		in.TS = time.Now().UnixMilli()
	}
	if err := s.store.InsertEvent(Event{
		SessionID:   in.SessionID,
		TS:          in.TS,
		Kind:        in.Kind,
		PayloadJSON: string(in.Payload),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeOK(w)
}

func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID    string          `json:"session_id"`
		EndedAt      int64           `json:"ended_at_ms"`
		Outcome      json.RawMessage `json:"outcome"`
		Transcript   json.RawMessage `json:"transcript"`
		RecordingURL string          `json:"recording_url"`
		CostCents    int64           `json:"cost_cents"`
		Metrics      json.RawMessage `json:"metrics"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.SessionID == "" || len(in.Outcome) == 0 {
		http.Error(w, "session_id and outcome required", http.StatusBadRequest)
		return
	}
	if in.EndedAt == 0 {
		in.EndedAt = time.Now().UnixMilli()
	}
	if err := s.store.InsertOutcome(Outcome{
		SessionID:      in.SessionID,
		OutcomeJSON:    string(in.Outcome),
		TranscriptJSON: string(in.Transcript),
		RecordingURL:   in.RecordingURL,
		CostCents:      in.CostCents,
		MetricsJSON:    string(in.Metrics),
		EndedAt:        in.EndedAt,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.gate != nil {
		if err := s.gate.End(r.Context(), in.SessionID, time.UnixMilli(in.EndedAt)); err != nil {
			// The outcome is stored; a retry of this webhook records the
			// usage without storing anything twice.
			log.Printf("voice: call usage for %s not recorded: %v", in.SessionID, err)
			http.Error(w, "call usage not recorded", http.StatusInternalServerError)
			return
		}
	}
	writeOK(w)
}

func (s *Server) handleHandoff(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID   string `json:"session_id"`
		RequestedAt int64  `json:"requested_at_ms"`
		Reason      string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if in.RequestedAt == 0 {
		in.RequestedAt = time.Now().UnixMilli()
	}
	id, err := s.store.InsertHandoff(Handoff{
		SessionID:   in.SessionID,
		RequestedAt: in.RequestedAt,
		Reason:      in.Reason,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "handoff_id": id, "target": "pending_review"})
}

func (s *Server) handleLearning(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID      string          `json:"session_id"`
		TS             int64           `json:"ts_ms"`
		Category       string          `json:"category"`
		Description    string          `json:"description"`
		Severity       string          `json:"severity"`
		ProposedChange json.RawMessage `json:"proposed_change"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.SessionID == "" || in.Description == "" {
		http.Error(w, "session_id and description required", http.StatusBadRequest)
		return
	}
	if in.TS == 0 {
		in.TS = time.Now().UnixMilli()
	}
	if err := s.store.InsertLearning(Learning{
		SessionID:          in.SessionID,
		TS:                 in.TS,
		Category:           in.Category,
		Description:        in.Description,
		Severity:           in.Severity,
		ProposedChangeJSON: string(in.ProposedChange),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeOK(w)
}

func (s *Server) handlePreCallContext(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CallerPhone string `json:"caller_phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.CallerPhone == "" {
		http.Error(w, "caller_phone required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout())
	defer cancel()
	// The one synchronous call before the greeting: refuse here once a daily
	// cap is reached, so an orchestrator can end the call before it runs.
	if s.gate != nil {
		if err := s.gate.Check(ctx); err != nil {
			writeRefused(w, err)
			return
		}
	}
	result := s.lookups.Run(ctx, in.CallerPhone)
	writeJSON(w, result)
}

// writeRefused answers 429 for a call refused by a cap and 503 when the
// caps could not be read: a call draftcat cannot decide on is not admitted.
func writeRefused(w http.ResponseWriter, err error) {
	status, reason := http.StatusServiceUnavailable, "call budget unavailable"
	if errors.Is(err, ErrCallRefused) {
		status, reason = http.StatusTooManyRequests, err.Error()
	} else {
		log.Printf("voice: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "refused": true, "reason": reason})
}

func writeOK(w http.ResponseWriter) {
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
