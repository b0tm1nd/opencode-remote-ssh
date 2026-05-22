package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/opencode-ai/opencode-remote-stub/internal/state"
)

type Handler struct {
	token   string
	st      *state.State
	events  *eventBus
}

type eventBus struct {
	mu       sync.RWMutex
	listeners []chan []byte
}

func newEventBus() *eventBus {
	return &eventBus{}
}

func (e *eventBus) Publish(eventType string, props map[string]interface{}) {
	payload, _ := json.Marshal(map[string]interface{}{
		"type":       eventType,
		"properties": props,
	})
	data := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, payload))

	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, ch := range e.listeners {
		select {
		case ch <- data:
		default:
		}
	}
}

func (e *eventBus) Subscribe() <-chan []byte {
	ch := make(chan []byte, 10)
	e.mu.Lock()
	e.listeners = append(e.listeners, ch)
	e.mu.Unlock()
	return ch
}

func New(token string, st *state.State) *Handler {
	return &Handler{
		token:  token,
		st:     st,
		events: newEventBus(),
	}
}

func JSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func Error(w http.ResponseWriter, code int, errType, message string) {
	JSON(w, code, map[string]interface{}{
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

func (h *Handler) SyncHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		Error(w, 405, "method_not_allowed", "POST required")
		return
	}
	JSON(w, 200, []interface{}{})
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	platform := "linux"
	arch := "amd64"

	log.Printf("[Health] workspaces=%d sessions=%d", len(h.st.ListWorkspaces()), len(h.st.ListSessions()))

	JSON(w, 200, map[string]interface{}{
		"ok":       true,
		"version":  "0.1.0",
		"hostname": hostname,
		"platform": platform,
		"arch":     arch,
		"stub": map[string]interface{}{
			"pid":       os.Getpid(),
			"startedAt": time.Now().UnixMilli(),
		},
	})
}

func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()

	ch := h.events.Subscribe()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case data := <-ch:
			w.Write(data)
			flusher.Flush()
		}
	}
}

func (h *Handler) WorkspaceAdaptor(w http.ResponseWriter, r *http.Request) {
	JSON(w, 200, []map[string]string{
		{
			"type":        "ssh-provider",
			"name":        "SSH Provider",
			"description": "Remote Linux host over SSH-backed Go stub",
		},
	})
}

func (h *Handler) WorkspaceList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		list := h.st.ListWorkspaces()
		out := make([]map[string]interface{}, 0, len(list))
		for _, ws := range list {
			out = append(out, map[string]interface{}{
				"id":         ws.ID,
				"type":       ws.Type,
				"name":       ws.Name,
				"branch":     nil,
				"directory":  nil,
				"extra":      ws.Extra,
				"projectID":  ws.ProjectID,
			})
		}
		JSON(w, 200, out)
		return
	}

	if r.Method == http.MethodPost {
		var ws state.Workspace
		if err := json.NewDecoder(r.Body).Decode(&ws); err != nil {
			log.Printf("[WorkspaceList POST] body decode error: %v", err)
			Error(w, 400, "invalid_request", err.Error())
			return
		}
		log.Printf("[WorkspaceList POST] registering workspace: id=%q type=%q name=%q projectID=%q host=%q", ws.ID, ws.Type, ws.Name, ws.ProjectID, ws.Host)
		ws.Status = "ready"
		ws.CreatedAt = time.Now().UnixMilli()
		if ws.Extra == nil {
			ws.Extra = make(map[string]interface{})
		}
		if v, ok := ws.Extra["host"].(string); ok {
			ws.Host = v
		}
		if v, ok := ws.Extra["provider"].(string); ok {
			ws.Provider = v
		}

		if err := h.st.CreateWorkspace(&ws); err != nil {
			log.Printf("[WorkspaceList POST] create error: %v", err)
			Error(w, 500, "internal_error", err.Error())
			return
		}
		log.Printf("[WorkspaceList POST] workspace %q registered OK", ws.ID)

		h.events.Publish("workspace.ready", map[string]interface{}{
			"workspaceID": ws.ID,
			"status":      ws.Status,
		})

		JSON(w, 200, map[string]interface{}{
			"id":         ws.ID,
			"type":       ws.Type,
			"name":       ws.Name,
			"branch":     nil,
			"directory":  nil,
			"extra":      ws.Extra,
			"projectID":  ws.ProjectID,
		})
		return
	}

	Error(w, 405, "method_not_allowed", "only GET and POST supported")
}

func (h *Handler) WorkspaceStatus(w http.ResponseWriter, r *http.Request) {
	list := h.st.ListWorkspaces()
	out := make([]map[string]interface{}, 0, len(list))
	for _, ws := range list {
		out = append(out, map[string]interface{}{
			"workspaceID": ws.ID,
			"status":      ws.Status,
		})
	}
	JSON(w, 200, out)
}

func WithWorkspaceRoutes(mux *http.ServeMux, require func(http.HandlerFunc) http.HandlerFunc, h *Handler) {
	mux.HandleFunc("/experimental/workspace/", require(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/experimental/workspace/"):]
		if r.Method == http.MethodDelete {
			if err := h.st.DeleteWorkspace(id); err != nil {
				Error(w, 500, "internal_error", err.Error())
				return
			}
			JSON(w, 200, true)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/experimental/workspace/"+id+"/session-restore" {
			JSON(w, 200, true)
			return
		}
		Error(w, 405, "method_not_allowed", "only DELETE supported")
	}))
}

func (h *Handler) SessionCreate(w http.ResponseWriter, r *http.Request) {
	log.Printf("[SessionCreate] method=%s path=%s query=%s", r.Method, r.URL.Path, r.URL.RawQuery)

	if r.Method != http.MethodPost {
		Error(w, 405, "method_not_allowed", "POST required")
		return
	}

	var req struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		WorkspaceID string `json:"workspaceID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[SessionCreate] body decode error: %v", err)
		Error(w, 400, "invalid_request", err.Error())
		return
	}

	log.Printf("[SessionCreate] body decoded: id=%q title=%q workspaceID=%q", req.ID, req.Title, req.WorkspaceID)

	if req.WorkspaceID == "" {
		req.WorkspaceID = r.URL.Query().Get("workspace")
		log.Printf("[SessionCreate] fallback to query workspace=%q", req.WorkspaceID)
	}

	log.Printf("[SessionCreate] looking up workspace id=%q", req.WorkspaceID)
	log.Printf("[SessionCreate] known workspaces: %v", func() []string {
		var ids []string
		for _, ws := range h.st.ListWorkspaces() {
			ids = append(ids, ws.ID)
		}
		return ids
	}())

	var ws *state.Workspace
	if req.WorkspaceID != "" {
		ws, _ = h.st.GetWorkspace(req.WorkspaceID)
	}
	if ws == nil {
		all := h.st.ListWorkspaces()
		for _, w := range all {
			ws = w
		}
	}
	if ws == nil {
		log.Printf("[SessionCreate] WORKSPACE NOT FOUND: %q (no workspaces at all)", req.WorkspaceID)
		Error(w, 404, "not_found", "workspace not found")
		return
	}

	req.WorkspaceID = ws.ID
	log.Printf("[SessionCreate] resolved to workspace id=%q name=%q", ws.ID, ws.Name)

	log.Printf("[SessionCreate] workspace found: id=%q type=%q name=%q", ws.ID, ws.Type, ws.Name)

	home, _ := os.UserHomeDir()
	sessionID := req.ID
	if sessionID == "" {
		sessionID = fmt.Sprintf("sess_%d", time.Now().UnixMilli())
	}
	se := &state.Session{
		ID:          sessionID,
		WorkspaceID: req.WorkspaceID,
		Title:       req.Title,
		Directory:   home,
		Status:      state.SessionStatus{Type: "idle"},
		CreatedAt:   time.Now().UnixMilli(),
		UpdatedAt:   time.Now().UnixMilli(),
	}

	if err := h.st.CreateSession(se); err != nil {
		Error(w, 500, "internal_error", err.Error())
		return
	}

	h.events.Publish("session.created", map[string]interface{}{
		"sessionID":    se.ID,
		"workspaceID": se.WorkspaceID,
	})

	JSON(w, 200, map[string]interface{}{
		"id":         se.ID,
		"slug":       se.Title,
		"projectID":  ws.ProjectID,
		"workspaceID": se.WorkspaceID,
		"directory":  se.Directory,
		"title":      se.Title,
		"version":    "0.1.0",
		"time": map[string]interface{}{
			"created": se.CreatedAt,
			"updated": se.UpdatedAt,
		},
		"permission": []string{},
	})
}

func (h *Handler) SessionStatus(w http.ResponseWriter, r *http.Request) {
	list := h.st.ListSessions()
	out := make(map[string]interface{})
	for _, se := range list {
		out[se.ID] = map[string]string{"type": se.Status.Type}
	}
	JSON(w, 200, out)
}

func (h *Handler) SessionGet(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/session/"):]
	se := h.ensureSession(id)
	if se == nil {
		Error(w, 500, "internal_error", "failed to get or create session")
		return
	}

	ws, _ := h.st.GetWorkspace(se.WorkspaceID)
	projectID := "unknown"
	if ws != nil {
		projectID = ws.ProjectID
	}

	JSON(w, 200, map[string]interface{}{
		"id":         se.ID,
		"slug":       se.Title,
		"projectID":  projectID,
		"workspaceID": se.WorkspaceID,
		"directory":  se.Directory,
		"title":      se.Title,
		"version":    "0.1.0",
		"time": map[string]interface{}{
			"created": se.CreatedAt,
			"updated": se.UpdatedAt,
		},
		"permission": []string{},
	})
}

func (h *Handler) SessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/session/"):]
	if err := h.st.DeleteSession(id); err != nil {
		Error(w, 500, "internal_error", err.Error())
		return
	}
	h.events.Publish("session.deleted", map[string]interface{}{"sessionID": id})
	JSON(w, 200, true)
}

func (h *Handler) PermissionList(w http.ResponseWriter, r *http.Request) {
	list := h.st.ListPermissions()
	out := make([]map[string]interface{}, 0, len(list))
	for _, p := range list {
		always := make([]string, 0)
		if approval := h.st.CheckApproval(p.WorkspaceID, ""); approval != nil && approval.Mode == "always" {
			always = append(always, approval.Pattern)
		}
		out = append(out, map[string]interface{}{
			"id":         p.ID,
			"sessionID":  p.SessionID,
			"permission": p.Permission,
			"patterns":   p.Patterns,
			"metadata":   p.Metadata,
			"always":     always,
		})
	}
	JSON(w, 200, out)
}

func (h *Handler) PermissionReply(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/permission/"):]
	if idx := strings.Index(id, "/reply"); idx > 0 {
		id = id[:idx]
	}

	p, ok := h.st.GetPermission(id)
	if !ok {
		Error(w, 404, "not_found", "permission request not found")
		return
	}

	var req struct {
		Reply   string `json:"reply"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, 400, "invalid_request", err.Error())
		return
	}

	p.Status = req.Reply
	h.st.UpdatePermission(p)

	if req.Reply == "always" || req.Reply == "once" {
		log.Printf("Creating approval for workspace %s pattern %s", p.WorkspaceID, p.Patterns[0])
		approval := &state.Approval{
			ID:          fmt.Sprintf("apr_%d", time.Now().UnixMilli()),
			WorkspaceID: p.WorkspaceID,
			Host:        "default",
			Permission:  p.Permission,
			Pattern:     p.Patterns[0],
			Mode:        req.Reply,
			CreatedAt:   time.Now().UnixMilli(),
		}
		if err := h.st.CreateApproval(approval); err != nil {
			log.Printf("ERROR creating approval: %v", err)
		} else {
			log.Printf("Approval created successfully")
		}
	}

	h.events.Publish("permission.replied", map[string]interface{}{
		"requestID": p.ID,
		"reply":     req.Reply,
	})

	JSON(w, 200, true)
}

func (h *Handler) ensureSession(id string) *state.Session {
	se, ok := h.st.GetSession(id)
	if ok {
		return se
	}
	home, _ := os.UserHomeDir()
	wsList := h.st.ListWorkspaces()
	wsID := ""
	if len(wsList) > 0 {
		wsID = wsList[0].ID
	}
	se = &state.Session{
		ID:          id,
		WorkspaceID: wsID,
		Directory:   home,
		Status:      state.SessionStatus{Type: "idle"},
		CreatedAt:   time.Now().UnixMilli(),
		UpdatedAt:   time.Now().UnixMilli(),
	}
	if err := h.st.CreateSession(se); err != nil {
		return nil
	}
	return se
}

func (h *Handler) Shell(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/session/"):]
	id = id[:len(id)-len("/shell")]

	se := h.ensureSession(id)
	if se == nil {
		Error(w, 500, "internal_error", "failed to create session")
		return
	}

	var req struct {
		Command string            `json:"command"`
		Cwd    string            `json:"cwd"`
		Env    map[string]string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, 400, "invalid_request", err.Error())
		return
	}

	cwd := req.Cwd
	if cwd == "" {
		cwd = se.Directory
	}

	approval := h.st.CheckApproval(se.WorkspaceID, cwd)
	if approval == nil || approval.Mode != "always" {
		permID := fmt.Sprintf("perm_%d", time.Now().UnixMilli())
		perm := &state.PermissionRequest{
			ID:          permID,
			SessionID:   se.ID,
			WorkspaceID: se.WorkspaceID,
			Permission:  "path.access",
			Patterns:    []string{cwd + "/**"},
			Metadata: map[string]interface{}{
				"operation": "shell",
				"cwd":       cwd,
			},
			Status:    "pending",
			CreatedAt: time.Now().UnixMilli(),
		}
		h.st.CreatePermission(perm)

		h.events.Publish("permission.asked", map[string]interface{}{
			"id":         perm.ID,
			"sessionID":  se.ID,
			"permission": perm.Permission,
			"patterns":   perm.Patterns,
			"metadata":   perm.Metadata,
		})

		Error(w, 403, "permission_required", "access to "+cwd+" requires approval")
		return
	}

	start := time.Now()
	output, err := runCommand(req.Command, cwd, req.Env)
	duration := time.Since(start).Milliseconds()

	JSON(w, 200, map[string]interface{}{
		"title": req.Command,
		"output": output,
		"metadata": map[string]interface{}{
			"exitCode":  err,
			"durationMs": duration,
			"cwd":       cwd,
		},
	})
}

func runCommand(cmd, cwd string, env map[string]string) (string, int) {
	parts := []string{"sh", "-c", cmd}
	execCmd := exec.Command(parts[0], parts[1:]...)
	execCmd.Dir = cwd
	execCmd.Stdin = nil

	if len(env) > 0 {
		for k, v := range env {
			execCmd.Env = append(os.Environ(), k+"="+v)
		}
	}

	out, err := execCmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return string(out), exitCode
}

func (h *Handler) Command(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/session/"):]
	id = id[:len(id)-len("/command")]

	se := h.ensureSession(id)
	if se == nil {
		Error(w, 500, "internal_error", "failed to get or create session")
		return
	}

	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, 400, "invalid_request", err.Error())
		return
	}

	output, exitCode := runCommand(req.Command, se.Directory, nil)

	JSON(w, 200, map[string]interface{}{
		"title":   req.Command,
		"output":  output,
		"metadata": map[string]interface{}{
			"exitCode": exitCode,
			"cwd":      se.Directory,
		},
	})
}

func (h *Handler) Message(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/session/"):]
	id = id[:len(id)-len("/message")]

	_ = h.ensureSession(id)

	JSON(w, 200, map[string]interface{}{
		"id":    fmt.Sprintf("msg_%d", time.Now().UnixMilli()),
		"role":  "assistant",
		"parts": []interface{}{},
	})
}