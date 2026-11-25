package agentapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// The same operations as plain JSON, for scripts and tools without MCP.

func (h *Handler) rest(w http.ResponseWriter, r *http.Request, fn func(c caller) (any, error)) {
	c, err := h.auth(r)
	if err != nil {
		h.unauthorized(w, err)
		return
	}
	v, err := fn(c)
	var ue userError
	switch {
	case errors.Is(err, errNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.As(err, &ue):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case err != nil:
		h.Log.Error("agent api", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errNotFound
	}
	return id, nil
}

func (h *Handler) restList(w http.ResponseWriter, r *http.Request) {
	h.rest(w, r, func(c caller) (any, error) {
		_, rows, err := h.listIssues(r.Context(), c, r.URL.Query().Get("include_fixed") == "true")
		return map[string]any{"issues": rows}, err
	})
}

func (h *Handler) restGet(w http.ResponseWriter, r *http.Request) {
	h.rest(w, r, func(c caller) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		_, v, err := h.getIssue(r.Context(), c, id)
		return v, err
	})
}

func (h *Handler) restReport(w http.ResponseWriter, r *http.Request) {
	h.rest(w, r, func(c caller) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		var a ReportArgs
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&a); err != nil {
			return nil, userError{"body must be JSON: {\"summary\": ..., \"files\": [...], \"release\": ...}"}
		}
		a.IssueID = id
		_, v, err := h.reportFix(r.Context(), c, a, h.now())
		return v, err
	})
}

func (h *Handler) restCheck(w http.ResponseWriter, r *http.Request) {
	h.rest(w, r, func(c caller) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		_, v, err := h.checkFix(r.Context(), c, id, r.URL.Query().Get("reproduced") == "true", h.now())
		return v, err
	})
}
