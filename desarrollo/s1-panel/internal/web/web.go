// Package web: handlers HTTP del panel s1 y plantillas embebidas.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"s1-panel/internal/auth"
	"s1-panel/internal/proxies"
	"s1-panel/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Server agrupa las dependencias de los handlers.
type Server struct {
	st       *store.Store
	auth     *auth.Manager
	user     string
	pass     string
	secure   bool
	tmpl     *template.Template
	s2URL    string // URL del endpoint de tareas de s2 ("" = no configurado)
	s2Notify string // URL de notificación de fin de subtarea a s2
	control  string // CONTROL_TOKEN; si no vacío, requerido en X-Control-Token
	httpCli  *http.Client
	dispatch func(ctx context.Context) (int, error)

	// loginLimits: rate-limit de login por IP (anti fuerza bruta, F1). En RAM
	// de proceso único (s1 corre 1 instancia; la DB guarda lo persistente).
	loginMu     sync.Mutex
	loginFailed map[string]loginFail // ip → intentos fallidos
	proxyMaxImport  int           // cuántos proxies importar del refresh (dev: pocos)
	proxyCheckBatch int           // cuántos verificar por refresh
	checkTimeout    time.Duration // timeout por verificación de proxy
	baseURL         string        // URL base pública para construir URLs TUI (SERVICES_BASE_URL + /servicio)
}

// loginFail registra intentos fallidos de una IP.
type loginFail struct {
	Count   int
	Blocked time.Time // hasta cuándo está bloqueada (0 = no bloqueada)
}

// SetBaseURL configura SERVICES_BASE_URL para la vista TUI del dashboard.
func (s *Server) SetBaseURL(u string) {
	s.baseURL = u
}

// SetProxyMax configure PROXY_MAX_IMPORT.
func (s *Server) SetProxyMax(n int) {
	if n > 0 {
		s.proxyMaxImport = n
	}
}

// SetProxyCheckBatch configura PROXY_CHECK_BATCH.
func (s *Server) SetProxyCheckBatch(n int) {
	if n > 0 {
		s.proxyCheckBatch = n
	}
}

// New crea el Server con templates cargadas.
func New(st *store.Store, am *auth.Manager, user, pass string, secure bool, s2URL, s2Notify, control string, dispatch func(ctx context.Context) (int, error)) (*Server, error) {
	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		st:              st,
		auth:            am,
		user:            user,
		pass:            pass,
		secure:          secure,
		tmpl:            tmpl,
		s2URL:           s2URL,
		s2Notify:        s2Notify,
		control:         control,
		httpCli:         &http.Client{Timeout: 15 * time.Second},
		dispatch:        dispatch,
		proxyMaxImport:  100, // spec: refresh = 100 más recientes y activos
		proxyCheckBatch: 10,
		checkTimeout:    6 * time.Second,
		loginFailed:     map[string]loginFail{},
	}, nil
}

// Routes registra todas las rutas HTTP del panel.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("/tareas", s.requireAuth(s.handleTareasPage))
	mux.HandleFunc("/api/keepalive", s.requireAuth(s.handleKeepAliveToggle))
	mux.HandleFunc("POST /api/tareas", s.requireAuth(s.handleTareaCreate))
	mux.HandleFunc("POST /api/tareas/{id}/enviar", s.requireAuth(s.handleTareaEnviar))

	// Fase 5: cola de subtareas (s2 y workers usan CONTROL_TOKEN)
	mux.HandleFunc("POST /api/subtareas", s.requireControl(s.handleSubtareasRecibir))
	mux.HandleFunc("GET /api/notificaciones/{worker_id}", s.requireControl(s.handleNotificacionPoll))
	mux.HandleFunc("POST /api/subtareas/{id}/fin", s.requireControl(s.handleSubtaskFin))
	mux.HandleFunc("POST /api/subtareas/{id}/progreso", s.requireControl(s.handleSubtaskProgreso))
	mux.HandleFunc("POST /api/subtareas/{id}/heartbeat", s.requireControl(s.handleSubtaskHeartbeat))
	mux.HandleFunc("GET /api/cola", s.requireAuth(s.handleColaJSON))

	// Fase 6: gestión de proxies (panel autenticado)
	mux.HandleFunc("/proxi", s.requireAuth(s.handleProxiPage))
	mux.HandleFunc("POST /api/proxy/refresh", s.requireAuth(s.handleProxyRefresh))
	mux.HandleFunc("POST /api/proxy/verificar", s.requireAuth(s.handleProxyVerificar))
	mux.HandleFunc("POST /api/proxy/asignar", s.requireAuth(s.handleProxyAsignar))
	mux.HandleFunc("POST /api/proxy/obtener", s.withCORS(s.withSessionOrControl(s.handleProxyObtener)))
	mux.HandleFunc("POST /api/proxy/retirar", s.requireAuth(s.handleProxyRetirar))
	mux.HandleFunc("POST /api/proxy/asignar-auto", s.requireAuth(s.handleProxyAsignarAuto))
	mux.HandleFunc("POST /api/proxy/asignar-nuevo", s.requireAuth(s.handleProxyAsignarNuevo))

	// Fase 11 (E1): tokens TUI — emisión (panel) y validación (servicios destino)
	mux.HandleFunc("POST /api/tui/token", s.requireAuth(s.handleTUITokenEmit))
	mux.HandleFunc("GET /api/tui/valida", s.requireControl(s.handleTUITokenValidate))

	// Fase 13 (F1): CORS controlado (API pública del panel)
	mux.HandleFunc("OPTIONS /api/proxy/obtener", s.handleCORS)
}

// requireControl exige CONTROL_TOKEN en cada request de worker/TUI.
// Nunca es un no-op: si s1 arrancó sin CONTROL_TOKEN, los endpoints de
// control responden 503 (falla cerrada) en vez de quedar abiertos.
func (s *Server) requireControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.control == "" {
			http.Error(w, "CONTROL_TOKEN no configurado en s1", http.StatusServiceUnavailable)
			return
		}
		tok := r.Header.Get("X-Control-Token")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		// Comparación en tiempo constante.
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.control)) != 1 {
			http.Error(w, "CONTROL_TOKEN inválido", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- Helpers de autenticación ------------------------------------------------

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth.UserFromCookie(r) == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) checkCredentials(user, pass string) bool {
	return subtle.ConstantTimeCompare([]byte(user), []byte(s.user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(s.pass)) == 1
}

// --- Handlers ----------------------------------------------------------------

// GET /health — health check del propio s1 (usado por keep-alive de otros).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"up","service":"s1","time":"` + time.Now().UTC().Format(time.RFC3339) + `"}`))
}

// GET /login — formulario; POST /login — autenticación.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if s.auth.UserFromCookie(r) != "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.tmpl.ExecuteTemplate(w, "login.html", map[string]string{"Error": ""})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
		return
	}
	// Rate-limit de login: 5 intentos fallidos en 10 min bloquea la IP 30 min.
	ip := clientIP(r)
	if msg := s.loginBlocked(ip); msg != "" {
		log.Printf("login bloqueado para IP %s: %s", ip, msg)
		w.WriteHeader(http.StatusTooManyRequests)
		s.tmpl.ExecuteTemplate(w, "login.html", map[string]string{"Error": msg})
		return
	}
	r.ParseForm()
	user := r.FormValue("user")
	pass := r.FormValue("pass")
	if !s.checkCredentials(user, pass) {
		log.Printf("login fallido para usuario %q (ip %s)", user, ip)
		s.loginFail(ip)
		w.WriteHeader(http.StatusUnauthorized)
		s.tmpl.ExecuteTemplate(w, "login.html", map[string]string{"Error": "Usuario o contraseña incorrectos"})
		return
	}
	s.loginOK(ip)
	if err := s.auth.SetCookie(w, user, s.secure); err != nil {
		http.Error(w, "error creando sesión", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// clientIP extrae la IP del cliente (host remoto; X-Forwarded-For si llega
// de un proxy en quien confiamos).
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}
	if host == "" || host == "127.0.0.1" || host == "::1" {
		// En Render detrás de proxy: usar X-Forwarded-For (primer hop).
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.Index(xff, ","); i != -1 {
				host = strings.TrimSpace(xff[:i])
			} else {
				host = strings.TrimSpace(xff)
			}
		}
	}
	return host
}

// loginBlocked devuelve el mensaje si la IP está bloqueada, o "" si puede
// intentar login.
func (s *Server) loginBlocked(ip string) string {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	f, ok := s.loginFailed[ip]
	if !ok {
		return ""
	}
	if !f.Blocked.IsZero() && time.Now().Before(f.Blocked) {
		return "Demasiados intentos: reintenta en unos minutos"
	}
	return ""
}

// loginFail registra un intento fallido; tras 5 en 10 min, bloquea 30 min.
func (s *Server) loginFail(ip string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	now := time.Now()
	f, ok := s.loginFailed[ip]
	// Ya bloqueada: no tocar (loginBlocked se encarga de rechazar).
	if ok && now.Before(f.Blocked) {
		return
	}
	// Sin historial, o bloqueo ya cumplido, o ventana de 10 min vencida:
	// empieza de cero.
	if !ok || now.Sub(f.Blocked) > 10*time.Minute {
		f = loginFail{Count: 1}
		s.loginFailed[ip] = f
		return
	}
	// Dentro de la ventana: cuenta otro fallo; al llegar a 5, bloquea 30 min.
	f.Count++
	if f.Count >= 5 {
		f.Blocked = now.Add(30 * time.Minute)
		f.Count = 0
	}
	s.loginFailed[ip] = f
}

// loginOK limpia el contador de una IP que autenticó correctamente.
func (s *Server) loginOK(ip string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.loginFailed, ip)
}

// GET /logout — cierra sesión.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	auth.ClearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// GET / — dashboard con servicios y toggle keep-alive.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	services, err := s.st.ListServices(ctx)
	if err != nil {
		log.Printf("dashboard: %v", err)
		http.Error(w, "error leyendo servicios", http.StatusInternalServerError)
		return
	}
	kaOn := s.st.KeepAliveState(ctx)
	user := s.auth.UserFromCookie(r)
	baseURL := s.baseURL
	if baseURL == "" {
		baseURL = r.Host
	}
	// Añadir esquema si falta (Host sin http://)
	if !strings.HasPrefix(baseURL, "http") {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		baseURL = scheme + "://" + baseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	data := map[string]interface{}{
		"User":        user,
		"Services":    services,
		"KeepAliveOn": kaOn,
		"BaseURL":     baseURL,
	}
	s.tmpl.ExecuteTemplate(w, "dashboard.html", data)
}

// POST /api/keepalive — toggle on/off persistido en settings.
func (s *Server) handleKeepAliveToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
		return
	}
	r.ParseForm()
	val := strings.TrimSpace(r.FormValue("value"))
	on := val == "on"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.st.SetKeepAlive(ctx, on); err != nil {
		http.Error(w, "error guardando toggle", http.StatusInternalServerError)
		return
	}
	log.Printf("keep-alive global → %v", on)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- Tareas (botón "Tarea") ---------------------------------------------------

// GET /tareas — vista de tareas del panel.
func (s *Server) handleTareasPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tareas, err := s.st.ListTareasPanel(ctx)
	if err != nil {
		log.Printf("tareas: %v", err)
		http.Error(w, "error leyendo tareas", http.StatusInternalServerError)
		return
	}
	data := map[string]interface{}{
		"User": s.auth.UserFromCookie(r),
		"Tareas": tareas,
		// flash: /tareas?ok=1 (enviada) o ?err=<motivo> (rechazada)
		"FlashOK":  r.URL.Query().Get("ok") == "1",
		"FlashErr": r.URL.Query().Get("err"),
	}
	s.tmpl.ExecuteTemplate(w, "tareas.html", data)
}

// POST /api/tareas — crear una o varias tareas (una por línea, spec: el botón
// Tarea acepta VARIAS tareas). Persistidas, nunca en RAM.
func (s *Server) handleTareaCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	raw := strings.TrimSpace(r.FormValue("prompt"))
	if raw == "" {
		http.Error(w, "prompt vacío", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	creadas := 0
	for _, linea := range strings.Split(raw, "\n") {
		prompt := strings.TrimSpace(linea)
		if prompt == "" {
			continue
		}
		if _, err := s.st.CreateTarea(ctx, prompt); err != nil {
			log.Printf("crear tarea: %v", err)
			http.Error(w, "error guardando tarea", http.StatusInternalServerError)
			return
		}
		creadas++
	}
	log.Printf("creadas %d tarea(s)", creadas)
	http.Redirect(w, r, "/tareas", http.StatusSeeOther)
}

// POST /api/tareas/{id}/enviar — envía la tarea a s2 y la marca como enviada.
// Regla: una vez enviada NO se puede cancelar ni corregir desde s1.
// Garantías: (a) marca atómica condicional (WHERE enviada_a_s2=false) — dos
// POST simultáneos no duplican el envío; (b) si no hay destino (S2_URL vacío)
// o s2 no responde, la tarea NO queda marcada como enviada (se puede reintentar).
func (s *Server) handleTareaEnviar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	tarea, err := s.st.GetTarea(ctx, id)
	if err != nil {
		http.Error(w, "tarea no encontrada", http.StatusNotFound)
		return
	}
	if tarea.EnviadaAS2 {
		http.Error(w, "tarea ya enviada a s2: no se puede cancelar ni corregir desde s1 (control en TUI de s2)", http.StatusBadRequest)
		return
	}
	if s.s2URL == "" {
		// Sin destino configurado, marcar como "enviada" sería mentirle al
		// usuario: s2 nunca la recibiría. Fallar cerrado para reintentar.
		log.Printf("enviar tarea %s: S2_URL no configurado, rechazado", id[:8])
		http.Error(w, "S2_URL no configurado: no se puede enviar la tarea a s2", http.StatusServiceUnavailable)
		return
	}

	// 1. Crear task global (estado inicial planificando)
	taskID, err := s.st.CreateTask(ctx, tarea.Prompt, "planificando")
	if err != nil {
		log.Printf("enviar tarea: crear task: %v", err)
		http.Error(w, "error creando task", http.StatusInternalServerError)
		return
	}

	// 2. Reserva atómica de la fila: solo un request puede ganar la transición
	// no-enviada → enviada. El perdedor descarta su task fantasma y responde 409.
	if err := s.st.MarkTareaEnviada(ctx, id, taskID); err != nil {
		_ = s.st.DeleteTask(ctx, taskID) // competidor ganó el envío
		if errors.Is(err, store.ErrTareaYaEnviada) {
			http.Error(w, "tarea ya enviada a s2: no se puede cancelar ni corregir desde s1 (control en TUI de s2)", http.StatusConflict)
			return
		}
		log.Printf("enviar tarea: marcar: %v", err)
		http.Error(w, "error marcando tarea enviada", http.StatusInternalServerError)
		return
	}

	// 3. Reenviar a s2 (obligatorio: si falla, se revierte la marca)
	body := []byte(`{"tarea_id":"` + id + `","task_id":"` + taskID + `","prompt":` + strconv.Quote(tarea.Prompt) + `}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.s2URL, bytes.NewReader(body))
	if err != nil {
		_ = s.st.UnmarkTareaEnviada(ctx, id)
		_ = s.st.DeleteTask(ctx, taskID)
		http.Error(w, "error preparando envío a s2", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.control != "" {
		req.Header.Set("X-Control-Token", s.control)
	}
	resp, err := s.httpCli.Do(req)
	if err != nil {
		log.Printf("enviar tarea a s2: %v", err)
		_ = s.st.UnmarkTareaEnviada(ctx, id)
		_ = s.st.DeleteTask(ctx, taskID)
		http.Error(w, "no se pudo contactar a s2: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		log.Printf("enviar tarea a s2: HTTP %d", resp.StatusCode)
		_ = s.st.UnmarkTareaEnviada(ctx, id)
		_ = s.st.DeleteTask(ctx, taskID)
		http.Error(w, "s2 rechazó la tarea (HTTP "+strconv.Itoa(resp.StatusCode)+")", http.StatusBadGateway)
		return
	}
	log.Printf("tarea %s enviada a s2 (task %s)", id[:8], taskID[:8])
	http.Redirect(w, r, "/tareas?ok=1", http.StatusSeeOther)
}

// --- Cola de subtareas (Fase 5) ----------------------------------------------

// POST /api/subtareas — s2 envía la lista de subtareas del plan dividido.
func (s *Server) handleSubtareasRecibir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID    string            `json:"task_id"`
		Subtareas []store.SubtaskIn `json:"subtareas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TaskID == "" || len(req.Subtareas) == 0 {
		http.Error(w, "task_id y subtareas son obligatorios", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	n, err := s.st.EnqueueSubtareas(ctx, req.TaskID, req.Subtareas)
	if err != nil {
		log.Printf("recibir subtareas: %v", err)
		http.Error(w, "error encolando subtareas", http.StatusInternalServerError)
		return
	}
	log.Printf("s2 envió %d subtareas para task %s", n, req.TaskID[:8])

	asignadas := 0
	if s.dispatch != nil {
		asignadas, _ = s.dispatch(ctx)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true, "enqueued": n, "asignadas": asignadas, "task_id": req.TaskID,
	})
}

// GET /api/notificaciones/{worker_id} — long-polling ligero del worker.
// Espera hasta 30s; responde la subtarea asignada o 204 si expira.
func (s *Server) handleNotificacionPoll(w http.ResponseWriter, r *http.Request) {
	workerID := r.PathValue("worker_id")
	if !validWorker(workerID) {
		http.Error(w, "worker inválido (w6..w10)", http.StatusBadRequest)
		return
	}
	pollCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	sub, err := s.st.PollNotification(pollCtx, workerID, 28*time.Second)
	if err != nil {
		log.Printf("poll %s: %v", workerID, err)
		http.Error(w, "error en long-poll", http.StatusInternalServerError)
		return
	}
	if sub == nil {
		w.WriteHeader(http.StatusNoContent) // 204: sin novedad, el worker repite
		return
	}
	log.Printf("notificación entregada a %s: subtarea %s", workerID, sub.ID[:8])
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sub)
}

// POST /api/subtareas/{id}/fin — el worker reporta el resultado.
func (s *Server) handleSubtaskFin(w http.ResponseWriter, r *http.Request) {
	subID := r.PathValue("id")
	var req struct {
		Estado    string          `json:"estado"` // completada | fallida
		Resultado json.RawMessage `json:"resultado"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if req.Estado != "completada" && req.Estado != "fallida" {
		http.Error(w, "estado debe ser completada|fallida", http.StatusBadRequest)
		return
	}
	if len(req.Resultado) == 0 {
		req.Resultado = json.RawMessage("{}")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	sub, err := s.st.GetSubtask(ctx, subID)
	if err != nil {
		http.Error(w, "subtarea no encontrada", http.StatusNotFound)
		return
	}
	if err := s.st.FinishSubtask(ctx, subID, req.Estado, string(req.Resultado)); err != nil {
		log.Printf("fin subtarea: %v", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	log.Printf("subtarea %s (%s) → %s por worker %s", subID[:8], sub.TaskID[:8], req.Estado, sub.WorkerID)

	// alertar al dispatcher: hay un worker libre
	if s.dispatch != nil {
		s.dispatch(ctx)
	}

	// progreso del task (antes de notificar a s2: s2 debe ver el estado final)
	st, _ := s.st.GetTaskStats(ctx, sub.TaskID)
	if st != nil && st.Activas == 0 && st.Total > 0 {
		fin := "completada"
		if st.Fallidas > 0 {
			fin = "fallida"
		}
		if err := s.st.SetTaskFinal(ctx, sub.TaskID, fin); err != nil {
			log.Printf("marcar task final: %v", err)
		} else {
			log.Printf("task %s → %s (%d subtareas)", sub.TaskID[:8], fin, st.Total)
		}
	}

	// avisar a s2 (leerá el resultado desde Supabase). Si falla, queda
	// pendiente en BD y StartS2Notifier la reintenta. Si tiene éxito, se
	// marca ya notificada para que el notifier no la reenvíe (duplicado).
	if err := s.notifyS2(ctx, sub.TaskID, subID, sub.WorkerID, req.Estado); err != nil {
		log.Printf("avisar a s2: %v (queda pendiente de reintento)", err)
	} else if err := s.st.MarkS2Notified(ctx, subID); err != nil {
		log.Printf("marcar s2 notificado: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// POST /api/subtareas/{id}/progreso — el worker informa que empezó a trabajar
// (asignada → en_progreso). Sin esto la transición quedaba huérfana en la
// spec de la cola: los estados se ven en Supabase, nunca en RAM.
func (s *Server) handleSubtaskProgreso(w http.ResponseWriter, r *http.Request) {
	subID := r.PathValue("id")
	if subID == "" {
		http.Error(w, `{"error":"id requerido"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.st.MarkEnProgreso(ctx, subID); err != nil {
		log.Printf("progreso %s: %v", subID[:8], err)
		http.Error(w, `{"error":"no se pudo marcar en_progreso"}`, http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "estado": "en_progreso"})
}

// POST /api/subtareas/{id}/heartbeat — señal de vida del worker mientras
// procesa la subtarea. Si la subtarea ya no es suya (preempción la reasignó),
// la respuesta incluye restart=true: el worker reinicia su OpenCode.
func (s *Server) handleSubtaskHeartbeat(w http.ResponseWriter, r *http.Request) {
	subID := r.PathValue("id")
	if subID == "" {
		http.Error(w, `{"error":"id requerido"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	restart, err := s.st.Heartbeat(ctx, subID)
	if err != nil {
		log.Printf("heartbeat %s: %v", subID[:8], err)
		http.Error(w, `{"error":"heartbeat no registrado"}`, http.StatusInternalServerError)
		return
	}
	if restart {
		log.Printf("heartbeat %s: subtarea ya reasignada → restart al worker", subID[:8])
	}
	defer func() {
		if s.dispatch != nil {
			s.dispatch(ctx)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "restart": restart})
}

// GET /api/cola — estado de la cola (debug/verificación).
func (s *Server) handleColaJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.RawQuery(r.Context(), `
		SELECT id, task_id, coalesce(worker_id,''), estado,
		       coalesce(file_path,''), created_at
		FROM subtask_queue ORDER BY created_at`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rows)
}

// notifyS2 avisa a s2 del fin de una subtarea (s2 consulta Supabase por el detalle).
// Devuelve error si no hay destino o si s2 no lo acepta; la cola de reintentos
// (StartS2Notifier) la reenvía hasta entregarla.
func (s *Server) notifyS2(ctx context.Context, taskID, subID, workerID, estado string) error {
	if s.s2Notify == "" {
		return fmt.Errorf("S2_NOTIFY_URL no configurado")
	}
	body := []byte(`{"task_id":"` + taskID + `","subtarea_id":"` + subID +
		`","worker_id":"` + workerID + `","estado":"` + estado + `"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.s2Notify, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.control != "" {
		req.Header.Set("X-Control-Token", s.control)
	}
	resp, err := s.httpCli.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("s2 respondió HTTP %d", resp.StatusCode)
	}
	log.Printf("s2 notificado: fin %s (%s)", subID[:8], estado)
	return nil
}

// StartS2Notifier reintenta periódicamente las notificaciones de fin de
// subtarea pendientes (cola en BD, nunca en RAM). Si no hay S2_NOTIFY_URL,
// las deja pendientes y vuelve a intentar.
func (s *Server) StartS2Notifier(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Println("notifier s2: detenido")
				return
			case <-ticker.C:
				if s.s2Notify == "" {
					continue
				}
				pend, err := s.st.PendingS2Notifications(ctx, 10)
				if err != nil {
					log.Printf("notifier s2: pendientes: %v", err)
					continue
				}
				for _, n := range pend {
					if err := s.notifyS2(ctx, n.TaskID, n.SubID, n.WorkerID, n.Estado); err == nil {
						if err := s.st.MarkS2Notified(ctx, n.SubID); err != nil {
							log.Printf("notifier s2: marcar %s: %v", n.SubID[:8], err)
						} else {
							log.Printf("notifier s2: %s entregada (reintento)", n.SubID[:8])
						}
					} else {
						log.Printf("notifier s2: reintento %s: %v", n.SubID[:8], err)
						break // si el destino está caído, parar la tanda
					}
				}
			}
		}
	}()
	log.Println("notifier s2: loop cada 15s")
}

// validWorker valida el id de un worker.
func validWorker(id string) bool {
	switch id {
	case "w6", "w7", "w8", "w9", "w10":
		return true
	}
	return false
}

// --- Gestión de proxies (Fase 6 / B5) ---------------------------------------

// proxiPageData alimenta la plantilla proxi.html.
type proxyView struct {
	ProxyURL      string
	Tipo          string
	Clasificacion string
	IPVista       string
	OcultaS       string // "sí" | "no" | "—"
	LatenciaMS    *int64
	LastChecked   string
}

type proxiPageData struct {
	Pool        []proxyView
	Assignments []store.AssignmentRow
	Destinos    []string
	LastRefresh string
	ProxyCount  int
}

// GET /proxi — sub-vista de gestión de proxies.
func (s *Server) handleProxiPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	pool, err := s.st.ListProxies(ctx, 40)
	if err != nil {
		log.Printf("proxi: pool: %v", err)
		pool = []store.ProxyRow{}
	}
	views := make([]proxyView, 0, len(pool))
	for _, p := range pool {
		oc := "—"
		if p.Oculta != nil {
			if *p.Oculta {
				oc = "sí"
			} else {
				oc = "no"
			}
		}
		lc := ""
		if p.LastChecked != nil {
			lc = *p.LastChecked
		}
		views = append(views, proxyView{
			ProxyURL: p.ProxyURL, Tipo: p.Tipo, Clasificacion: p.Clasificacion,
			IPVista: p.IPVista, OcultaS: oc, LatenciaMS: p.LatenciaMS,
			LastChecked: lc,
		})
	}
	assignments, err := s.st.ListAssignments(ctx)
	if err != nil {
		log.Printf("proxi: assignments: %v", err)
		assignments = []store.AssignmentRow{}
	}
	destinos, err := s.st.ProxyDestinations(ctx)
	if err != nil {
		log.Printf("proxi: destinos: %v", err)
		destinos = []string{}
	}
	last, _ := s.st.LastProxyRefresh(ctx)
	count, _ := s.st.ProxyCount(ctx)

	s.tmpl.ExecuteTemplate(w, "proxi.html", proxiPageData{
		Pool: views, Assignments: assignments, Destinos: destinos,
		LastRefresh: last, ProxyCount: count,
	})
}

// or devuelve a si no está vacío, si no b.
func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// POST /api/proxy/refresh — descarga la lista, verifica un lote y rota proxies.
func (s *Server) handleProxyRefresh(w http.ResponseWriter, r *http.Request) {
	defer http.Redirect(w, r, "/proxi", http.StatusSeeOther)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	entradas, err := proxies.FetchList(ctx, s.proxyMaxImport)
	if err != nil {
		log.Printf("proxy refresh: %v", err)
		return
	}
	nuevos, existentes, err := s.st.UpsertProxies(ctx, entradas)
	if err != nil {
		log.Printf("proxy refresh upsert: %v", err)
		return
	}
	verificados, validados := s.verifyBatch(ctx, entradas)
	if err := s.st.MarkProxyRefresh(ctx, time.Now().Format("2006-01-02 15:04:05")); err != nil {
		log.Printf("marcar refresh: %v", err)
	}
	s.rotateAll(ctx)
	log.Printf("proxy refresh: %d nuevas, %d existentes, %d verificados (%d válidos)",
		nuevos, existentes, verificados, validados)
}

// verifyBatch verifica hasta proxyCheckBatch proxies http/https nuevos.
func (s *Server) verifyBatch(ctx context.Context, entradas []proxies.Entry) (verificados, validados int) {
	n := 0
	for _, e := range entradas {
		if e.Tipo != "http" && e.Tipo != "https" {
			continue // socks4/5 → Fase 13
		}
		if n >= s.proxyCheckBatch {
			break
		}
		s.verificar(ctx, proxies.ProxyURL(e.URL))
		n++
	}
	validados, _ = s.st.CountValidated(ctx)
	return n, validados
}

// verificar corre el checker real sobre un proxy y guarda la clasificación.
func (s *Server) verificar(ctx context.Context, proxyURL string) {
	res := proxies.CheckIP(ctx, proxyURL, s.checkTimeout)
	if res.Err != nil {
		if err := s.st.SetProxyChecked(ctx, proxyURL, "descartado", "", res.Err.Error(), nil, nil); err != nil {
			log.Printf("guardar descarte %s: %v", proxyURL, err)
		}
		log.Printf("proxy %s → descartado: %v", proxyURL, res.Err)
		return
	}
	lat := res.Latency
	ocultaBool := false
	oculta := &ocultaBool
	if ipDirecta, err := proxies.DirectIP(ctx, 8*time.Second); err == nil {
		ocultaBool = res.IP != ipDirecta
		log.Printf("proxy %s → validado (IP vista %s, oculta=%v, %dms)", proxyURL, res.IP, ocultaBool, res.Latency)
	} else {
		log.Printf("proxy %s → validado (sin línea base: %v)", proxyURL, err)
	}
	if err := s.st.SetProxyChecked(ctx, proxyURL, "validado", res.IP, "", oculta, &lat); err != nil {
		log.Printf("guardar validado %s: %v", proxyURL, err)
	}
}

// assignAuto asigna el mejor proxy validado a destinos sin proxy activo.
func (s *Server) assignAuto(ctx context.Context) {
	destinos, err := s.st.ProxyDestinations(ctx)
	if err != nil {
		log.Printf("assignAuto destinos: %v", err)
		return
	}
	for _, d := range destinos {
		if a, err := s.st.ActiveProxyOf(ctx, d); err == nil && a != nil {
			continue // ya tiene proxy activo (aunque no confirmado: reserva)
		}
		best, err := s.st.BestFreeProxy(ctx, "http")
		if err != nil || best == nil {
			break // sin más proxies validados libres
		}
		if err := s.st.AssignProxy(ctx, d, *best, "auto"); err != nil {
			log.Printf("assignAuto %s: %v", d, err)
			continue
		}
		log.Printf("assignAuto: %s → %s (auto)", d, *best)
	}
}

// rotateAll rota el proxy de CADA uno de los 10 servicios (spec: "cada hora a
// las 00 min asigna 1 proxy a cada uno de los 10 servicios"). AssignProxy ya
// retira el anterior, así que asignar siempre rota. Si no hay proxies
// validados libres para un servicio, conserva el actual.
func (s *Server) rotateAll(ctx context.Context) {
	destinos, err := s.st.ProxyDestinations(ctx)
	if err != nil {
		log.Printf("rotateAll destinos: %v", err)
		return
	}
	for _, d := range destinos {
		best, err := s.st.BestFreeProxy(ctx, "http")
		if err != nil {
			log.Printf("rotateAll %s: %v", d, err)
			break
		}
		if best == nil {
			log.Printf("rotateAll: sin proxy libre validado para %s (conserva el actual)", d)
			continue
		}
		if err := s.st.AssignProxy(ctx, d, *best, "rotacion"); err != nil {
			log.Printf("rotateAll %s: %v", d, err)
			continue
		}
		log.Printf("rotateAll: %s → %s (rotación horaria)", d, *best)
	}
}

// POST /api/proxy/verificar — verifica un proxy concreto del pool.
func (s *Server) handleProxyVerificar(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if req.ProxyURL == "" {
		http.Error(w, "proxy_url obligatorio", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.st.UpsertProxy(ctx, proxies.ProxyURL(req.ProxyURL), "http"); err != nil {
		log.Printf("upsert proxy: %v", err)
	}
	s.verificar(ctx, proxies.ProxyURL(req.ProxyURL))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ok": "verificado"})
}

// POST /api/proxy/asignar — asignación manual.
func (s *Server) handleProxyAsignar(w http.ResponseWriter, r *http.Request) {
	defer http.Redirect(w, r, "/proxi", http.StatusSeeOther)
	var req struct {
		Servicio string `json:"servicio"`
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if req.Servicio == "" || req.ProxyURL == "" {
		return
	}
	if err := s.st.AssignProxy(r.Context(), req.Servicio, proxies.ProxyURL(req.ProxyURL), "manual"); err != nil {
		log.Printf("asignar proxy: %v", err)
	}
	log.Printf("proxy asignado manualmente: %s → %s", req.Servicio, req.ProxyURL)
}

// POST /api/proxy/obtener — pide la IP pública A TRAVÉS del proxy asignado
// al servicio y, si responde, marca la asignación como confirmada (OK).
func (s *Server) handleProxyObtener(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Servicio string `json:"servicio"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	asig, err := s.st.ActiveProxyOf(r.Context(), req.Servicio)
	if err != nil || asig == nil {
		http.Error(w, "el servicio no tiene proxy activo", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res := proxies.CheckIP(ctx, asig.ProxyURL, s.checkTimeout)
	if res.Err != nil {
		http.Error(w, "no se pudo obtener IP vía proxy: "+res.Err.Error(), http.StatusBadGateway)
		return
	}
	oculta := false
	if ipD, err := proxies.DirectIP(ctx, 8*time.Second); err == nil {
		oculta = res.IP != ipD
	}
	lat := res.Latency
	ocultaP := &oculta
	s.st.SetProxyChecked(ctx, asig.ProxyURL, "validado", res.IP, "", ocultaP, &lat)
	s.st.ConfirmAssignment(ctx, req.Servicio) // el target "confirma" que le funciona
	log.Printf("obtener: %s vía %s → IP pública %s (oculta=%v)", req.Servicio, asig.ProxyURL, res.IP, oculta)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"servicio": req.Servicio, "proxy": asig.ProxyURL,
		"ip_vista": res.IP, "oculta": oculta, "latencia_ms": res.Latency,
		"estado": "OK",
	})
}

// POST /api/proxy/retirar — retira la asignación activa (queda liberada).
func (s *Server) handleProxyRetirar(w http.ResponseWriter, r *http.Request) {
	defer http.Redirect(w, r, "/proxi", http.StatusSeeOther)
	var req struct {
		Servicio string `json:"servicio"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	if err := s.st.RetireAssignment(r.Context(), req.Servicio); err != nil {
		log.Printf("retirar proxy: %v", err)
	}
	log.Printf("proxy retirado de %s (liberada)", req.Servicio)
}

// POST /api/proxy/asignar-auto — fuerza la asignación automática.
func (s *Server) handleProxyAsignarAuto(w http.ResponseWriter, r *http.Request) {
	defer http.Redirect(w, r, "/proxi", http.StatusSeeOther)
	s.assignAuto(r.Context())
}

// POST /api/proxy/asignar-nuevo — asigna un proxy NUEVO a UN servicio (⋮ del
// panel). Retira el anterior (AssignProxy lo hace) y reserva el mejor proxy
// validado libre disponible. 404 si no hay ninguno.
func (s *Server) handleProxyAsignarNuevo(w http.ResponseWriter, r *http.Request) {
	defer http.Redirect(w, r, "/proxi", http.StatusSeeOther)
	var req struct {
		Servicio string `json:"servicio"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	servicio := store.CleanServiceID(req.Servicio)
	if servicio == "" {
		http.Error(w, "servicio inválido (s1..s5, w6..w10)", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	best, err := s.st.BestFreeProxy(ctx, "http")
	if err != nil {
		log.Printf("asignar-nuevo %s: %v", servicio, err)
		http.Error(w, "error consultando pool", http.StatusInternalServerError)
		return
	}
	if best == nil {
		http.Error(w, "no hay proxies validados libres: pulsa Refresh/verificar", http.StatusNotFound)
		return
	}
	if err := s.st.AssignProxy(ctx, servicio, *best, "renovacion"); err != nil {
		log.Printf("asignar-nuevo %s: %v", servicio, err)
		http.Error(w, "error asignando proxy", http.StatusInternalServerError)
		return
	}
	log.Printf("⋮ %s → %s (nuevo proxy)", servicio, *best)
}

// RefreshProxies es la rutina completa del cron (cada hora al minuto 00).
func (s *Server) RefreshProxies(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	entradas, err := proxies.FetchList(ctx, s.proxyMaxImport)
	if err != nil {
		log.Printf("cron proxy refresh: %v", err)
		return
	}
	if _, _, err := s.st.UpsertProxies(ctx, entradas); err != nil {
		log.Printf("cron upsert: %v", err)
		return
	}
	s.verifyBatch(ctx, entradas)
	s.st.MarkProxyRefresh(ctx, time.Now().Format("2006-01-02 15:04:05"))
	s.rotateAll(ctx)
	log.Println("cron proxy refresh completado")
}

// --- Tokens TUI (Fase 11 / E1) ----------------------------------------------

// POST /api/tui/token — emite un token TUI de corta duración para un servicio.
// El token crudo solo viaja aquí; s1 guarda su SHA-256 en tui_access_tokens.
func (s *Server) handleTUITokenEmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Servicio string `json:"servicio"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	servicio := store.CleanServiceID(req.Servicio)
	if servicio == "" {
		http.Error(w, "servicio inválido (s1..s5, w6..w10)", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user := s.auth.UserFromCookie(r)
	if user == "" {
		user = "admin"
	}
	token, err := s.st.EmitTUIToken(ctx, servicio, user, 10*time.Minute)
	if err != nil {
		log.Printf("emitir token TUI: %v", err)
		http.Error(w, "error emitiendo token", http.StatusInternalServerError)
		return
	}
	log.Printf("token TUI emitido para %s (usuario %s)", servicio, user)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"servicio": servicio,
		"token":    token,
		"ttl":      "10m",
	})
}

// GET /api/tui/valida — valida y consume un token TUI (lo llama el servicio destino).
// Protegido por CONTROL_TOKEN (requireControl).
func (s *Server) handleTUITokenValidate(w http.ResponseWriter, r *http.Request) {
	servicio := store.CleanServiceID(r.URL.Query().Get("servicio"))
	token := r.URL.Query().Get("token")
	if servicio == "" || token == "" {
		http.Error(w, "servicio y token son obligatorios", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	usuario, err := s.st.ValidateTUIToken(ctx, servicio, token)
	if err != nil {
		log.Printf("validar token TUI: %v", err)
		http.Error(w, "error validando token", http.StatusInternalServerError)
		return
	}
	if usuario == "" {
		http.Error(w, "token TUI inválido o expirado", http.StatusUnauthorized)
		return
	}
	log.Printf("token TUI validado para %s (usuario %s), marcado usado", servicio, usuario)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ok": "true", "servicio": servicio, "usuario": usuario})
}

// withSessionOrControl admite el request si tiene sesión de panel válida
// (cookie) o CONTROL_TOKEN correcto (servicios destino). Para endpoints que
// sirven a ambos, como /api/proxy/obtener.
func (s *Server) withSessionOrControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth.UserFromCookie(r) != "" {
			next(w, r)
			return
		}
		if s.control != "" {
			tok := r.Header.Get("X-Control-Token")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.control)) == 1 {
				next(w, r)
				return
			}
		}
		http.Error(w, "no autorizado: requiere sesión de panel o CONTROL_TOKEN", http.StatusUnauthorized)
	}
}

// handleCORS responde preflight OPTIONS con cabeceras CORS controladas (F1).
// En s1 solo se aplica al endpoint público /api/proxy/obtener.
func (s *Server) handleCORS(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// allowedOrigin devuelve el origen si está en la allowlist CORS (F1).
func (s *Server) allowedOrigin(r *http.Request) string {
	allowlist := map[string]bool{
		"http://localhost:8080": true,
		"http://localhost:9002": true,
		"http://localhost:9003": true,
		"http://localhost:9004": true,
		"http://localhost:9005": true,
		"http://localhost:9006": true,
		"http://localhost:9007": true,
		"http://localhost:9008": true,
		"http://localhost:9009": true,
		"http://localhost:9010": true,
		"https://s1-panel.onrender.com":      true,
		"https://s2-carlos-code.onrender.com": true,
	}
	origin := r.Header.Get("Origin")
	if allowlist[origin] {
		return origin
	}
	return ""
}

// setCORSHeaders añade cabeceras CORS a una RESPUESTA real (no solo preflight):
// solo si el Origin está en la allowlist.
func (s *Server) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	if origin := s.allowedOrigin(r); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}

// withCORS envuelve un handler para que sus respuestas reales lleven CORS.
func (s *Server) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.setCORSHeaders(w, r)
		next(w, r)
	}
}
