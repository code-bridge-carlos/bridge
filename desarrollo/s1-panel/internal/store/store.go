// Package store: capa de datos de s1. Stateless — toda la persistencia en
// PostgreSQL (Supabase en producción). Nunca acumula estado en RAM.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"s1-panel/internal/auth"
	"s1-panel/internal/proxies"
)

// ServiceStatus es una fila de services_status.
type ServiceStatus struct {
	ID         string    `json:"id_servicio"`
	RAMMB      int       `json:"ram_mb"`
	Estado     string    `json:"estado"`
	URLBase    string    `json:"url_base"` // base para health-check (keep-alive)
	URLTUI     string    `json:"url_tui"`  // solo servicios con terminal de agente
	LatenciaMS int       `json:"latencia_ms"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Store agrupa las operaciones de BD que usa s1.
type Store struct {
	db *sql.DB
}

// Open conecta a PostgreSQL usando DATABASE_URL.
func Open(databaseURL string) (*Store, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL vacía")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("abrir conexión: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping a base de datos: %w", err)
	}
	return &Store{db: db}, nil
}

// Close cierra la conexión.
func (s *Store) Close() error { return s.db.Close() }

// ListServices devuelve el estado de los 10 servicios.
func (s *Store) ListServices(ctx context.Context) ([]ServiceStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id_servicio, coalesce(ram_mb,0), estado, coalesce(url_base,''),
		       coalesce(url_tui,''), coalesce(latencia_ms,0), updated_at
		FROM services_status
		ORDER BY id_servicio`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceStatus{}
	for rows.Next() {
		var sv ServiceStatus
		if err := rows.Scan(&sv.ID, &sv.RAMMB, &sv.Estado, &sv.URLBase, &sv.URLTUI, &sv.LatenciaMS, &sv.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// UpdateServiceStatus escribe el resultado de un ping keep-alive.
func (s *Store) UpdateServiceStatus(ctx context.Context, id string, estado string, latenciaMS int, ramMB int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE services_status
		SET estado=$2, latencia_ms=$3, ram_mb=$4, updated_at=now()
		WHERE id_servicio=$1`, id, estado, latenciaMS, ramMB)
	return err
}

// GetSetting lee un valor de settings (ej: keep_alive).
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key=$1`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting escribe un valor de settings.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ($1,$2)
		ON CONFLICT (key) DO UPDATE SET value=$2, updated_at=now()`, key, value)
	return err
}

// KeepAliveState devuelve si el keep-alive global está activo.
func (s *Store) KeepAliveState(ctx context.Context) bool {
	v, _ := s.GetSetting(ctx, "keep_alive")
	return v == "on"
}

// SetKeepAlive activa/desactiva el keep-alive global.
func (s *Store) SetKeepAlive(ctx context.Context, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	return s.SetSetting(ctx, "keep_alive", v)
}

// --- Tareas del panel (botón "Tarea") ----------------------------------------

// TareaPanel es una fila de tareas_panel combinada con su task global.
type TareaPanel struct {
	ID           string    `json:"id"`
	Prompt       string    `json:"prompt"`
	CreadaAt     time.Time `json:"creada_at"`
	EnviadaAS2   bool      `json:"enviada_a_s2"`
	SubtaskCount int       `json:"subtask_count"`
	TaskID       *string   `json:"task_id"`
	TaskEstado   string    `json:"task_estado"` // estado general (de tasks) o ""
}

// CreateTarea guarda una tarea escrita en el panel (nunca en RAM).
func (s *Store) CreateTarea(ctx context.Context, prompt string) (*TareaPanel, error) {
	var t TareaPanel
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO tareas_panel (prompt) VALUES ($1)
		RETURNING id, prompt, creada_at, enviada_a_s2, subtask_count, task_id`,
		prompt).Scan(&t.ID, &t.Prompt, &t.CreadaAt, &t.EnviadaAS2, &t.SubtaskCount, &t.TaskID)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTareasPanel devuelve todas las tareas del panel con su estado general.
func (s *Store) ListTareasPanel(ctx context.Context) ([]TareaPanel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.prompt, p.creada_at, p.enviada_a_s2, p.subtask_count,
		       p.task_id, coalesce(t.estado,'')
		FROM tareas_panel p
		LEFT JOIN tasks t ON t.id = p.task_id
		ORDER BY p.creada_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TareaPanel{}
	for rows.Next() {
		var t TareaPanel
		if err := rows.Scan(&t.ID, &t.Prompt, &t.CreadaAt, &t.EnviadaAS2,
			&t.SubtaskCount, &t.TaskID, &t.TaskEstado); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTarea devuelve una tarea del panel por id.
func (s *Store) GetTarea(ctx context.Context, id string) (*TareaPanel, error) {
	var t TareaPanel
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.prompt, p.creada_at, p.enviada_a_s2, p.subtask_count,
		       p.task_id, coalesce(t.estado,'')
		FROM tareas_panel p
		LEFT JOIN tasks t ON t.id = p.task_id
		WHERE p.id=$1`, id).Scan(&t.ID, &t.Prompt, &t.CreadaAt, &t.EnviadaAS2,
		&t.SubtaskCount, &t.TaskID, &t.TaskEstado)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ErrTareaYaEnviada: la tarea ya fue enviada a s2 (o está enviándose).
var ErrTareaYaEnviada = errors.New("tarea ya enviada a s2")

// MarkTareaEnviada marca atómicamente la tarea como enviada SOLO si aún no lo
// estaba (UPDATE condicional WHERE enviada_a_s2=false). Cierra el race TOCTOU:
// dos POST simultáneos no pueden duplicar el envío; el perdedor recibe
// ErrTareaYaEnviada.
func (s *Store) MarkTareaEnviada(ctx context.Context, tareaID, taskID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tareas_panel
		SET enviada_a_s2=true, task_id=$2
		WHERE id=$1 AND enviada_a_s2=false`, tareaID, taskID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrTareaYaEnviada
	}
	return nil
}

// UnmarkTareaEnviada revierte la marca si el envío a s2 falló, dejando la
// tarea disponible para reintentar.
func (s *Store) UnmarkTareaEnviada(ctx context.Context, tareaID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tareas_panel SET enviada_a_s2=false, task_id=NULL WHERE id=$1`, tareaID)
	return err
}

// SubTaskCountActualizado actualiza el nº de subtareas de una tarea (B4).
func (s *Store) SubTaskCountActualizado(ctx context.Context, taskID string, n int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tareas_panel SET subtask_count=$2
		WHERE task_id=$1`, taskID, n)
	return err
}

// --- Tareas globales (tabla tasks, gestionada por s2) -------------------------

// CreateTask crea una tarea global con estado inicial.
func (s *Store) CreateTask(ctx context.Context, prompt string, estado string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO tasks (prompt, estado) VALUES ($1,$2) RETURNING id`,
		prompt, estado).Scan(&id)
	return id, err
}

// DeleteTask elimina un task global (solo para deshacer un envío fallido a s2;
// las subtareas se borran en cascada — no deberían existir todavía).
func (s *Store) DeleteTask(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id=$1`, taskID)
	return err
}

// --- Cola de subtareas (Fase 5) ----------------------------------------------

// SubtaskIn es una subtarea entrante desde s2.
type SubtaskIn struct {
	FilePath string `json:"file_path"`
	Prompt   string `json:"prompt"`
	Contexto string `json:"contexto"`
}

// Subtask es una fila de subtask_queue.
type Subtask struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	WorkerID  string    `json:"worker_id"`
	FilePath  string    `json:"file_path"`
	Prompt    string    `json:"prompt"`
	Contexto  string    `json:"contexto"`
	Estado    string    `json:"estado"`
	Resultado string    `json:"resultado"`
	CreatedAt time.Time `json:"created_at"`
}

// EnqueueSubtareas encola subtareas de un task y lo marca como ejecutando.
func (s *Store) EnqueueSubtareas(ctx context.Context, taskID string, subs []SubtaskIn) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, sub := range subs {
		if sub.Prompt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subtask_queue (task_id, file_path, prompt, contexto, estado)
			VALUES ($1,$2,$3,$4,'pendiente')`,
			taskID, sub.FilePath, sub.Prompt, sub.Contexto); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		// Plan vacío: la tarea se cierra de inmediato (nada que ejecutar).
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET estado='completada', finished_at=now() WHERE id=$1`, taskID); err != nil {
			return n, err
		}
	} else {
		// task pasa a ejecutando + subtask_count del panel
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET estado='ejecutando' WHERE id=$1`, taskID); err != nil {
			return n, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tareas_panel SET subtask_count=$2 WHERE task_id=$1`, taskID, n); err != nil {
		return n, err
	}
	return n, tx.Commit()
}

// PendingSubtaskIDs devuelve las subtareas pendientes más antiguas.
func (s *Store) PendingSubtaskIDs(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM subtask_queue WHERE estado='pendiente'
		ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FreeWorkers devuelve los workers (w6-w10) sin subtarea activa.
func (s *Store) FreeWorkers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id FROM (VALUES ('w6'),('w7'),('w8'),('w9'),('w10')) AS w(id)
		LEFT JOIN subtask_queue sq
		  ON sq.worker_id = w.id AND sq.estado IN ('asignada','en_progreso')
		WHERE sq.id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AssignSubtask asigna una subtarea a un worker (pendiente → asignada).
func (s *Store) AssignSubtask(ctx context.Context, subID, workerID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE subtask_queue SET worker_id=$2, estado='asignada', updated_at=now()
		WHERE id=$1 AND estado='pendiente'`, subID, workerID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("subtarea %s ya no está pendiente", subID[:8])
	}
	return nil
}

// GetSubtask devuelve una subtarea por id.
func (s *Store) GetSubtask(ctx context.Context, subID string) (*Subtask, error) {
	var st Subtask
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, coalesce(worker_id,''), coalesce(file_path,''),
		       prompt, coalesce(contexto,''), estado, coalesce(resultado::text,''), created_at
		FROM subtask_queue WHERE id=$1`, subID).Scan(
		&st.ID, &st.TaskID, &st.WorkerID, &st.FilePath, &st.Prompt,
		&st.Contexto, &st.Estado, &st.Resultado, &st.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// PollNotification long-polla una subtarea asignada y no notificada para un worker.
// Devuelve (nil, nil) si expira el tiempo sin novedad.
func (s *Store) PollNotification(ctx context.Context, workerID string, maxWait time.Duration) (*Subtask, error) {
	deadline := time.Now().Add(maxWait)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		var st Subtask
		err := s.db.QueryRowContext(ctx, `
			SELECT id, task_id, worker_id, coalesce(file_path,''), prompt,
			       coalesce(contexto,''), estado, coalesce(resultado::text,''), created_at
			FROM subtask_queue
			WHERE worker_id=$1 AND estado='asignada' AND notificada_at IS NULL
			ORDER BY created_at LIMIT 1`,
			workerID).Scan(&st.ID, &st.TaskID, &st.WorkerID, &st.FilePath,
			&st.Prompt, &st.Contexto, &st.Estado, &st.Resultado, &st.CreatedAt)
		if err == nil {
			// entregar la notificación (marcar para evitar re-entregas)
			if _, err := s.db.ExecContext(ctx, `
				UPDATE subtask_queue SET notificada_at=now()
				WHERE id=$1 AND notificada_at IS NULL`, st.ID); err != nil {
				return nil, err
			}
			return &st, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, nil // timeout: el worker repite el poll
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// FinishSubtask marca el fin de una subtarea (resultado del worker).
func (s *Store) FinishSubtask(ctx context.Context, subID, estado string, resultado string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE subtask_queue SET estado=$2, resultado=$3::jsonb, updated_at=now()
		WHERE id=$1 AND estado IN ('asignada','en_progreso')`, subID, estado, resultado)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("subtarea %s no está activa (asignada/en_progreso)", subID[:8])
	}
	return nil
}

// MarkEnProgreso transiciona asignada → en_progreso (el worker empezó a
// trabajar sobre el archivo). Solo si aún está asignada a alguien.
func (s *Store) MarkEnProgreso(ctx context.Context, subID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE subtask_queue SET estado='en_progreso', updated_at=now()
		WHERE id=$1 AND estado='asignada'`, subID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("subtarea %s no está asignada (no se puede marcar en_progreso)", subID[:8])
	}
	return nil
}

// Heartbeat registra la señal de vida de un worker en su subtarea activa.
// Devuelve restart=true si la subtarea ya no está activa (fue reasignada por
// preempción): el worker debe reiniciar su sesión de OpenCode.
func (s *Store) Heartbeat(ctx context.Context, subID string) (restart bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE subtask_queue SET heartbeat_at=now()
		WHERE id=$1 AND estado IN ('asignada','en_progreso')`, subID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 0, nil // 0 filas → ya no es suya → restart
}

// StaleSubtareas devuelve subtareas activas sin señal de vida reciente
// (worker colgado/muerto). Con workers antiguos (heartbeat NULL) usa el
// momento de asignación como última señal.
func (s *Store) StaleSubtareas(ctx context.Context, sinSeñal time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM subtask_queue
		WHERE estado IN ('asignada','en_progreso')
		  AND COALESCE(heartbeat_at, updated_at) < now() - make_interval(secs => $1)
		ORDER BY created_at LIMIT 10`, float64(sinSeñal.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LongRunningSubtareas devuelve subtareas activas desde hace >=dur
// (candidatas a handoff por preempción por idle).
func (s *Store) LongRunningSubtareas(ctx context.Context, dur time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM subtask_queue
		WHERE estado IN ('asignada','en_progreso')
		  AND COALESCE(heartbeat_at, updated_at) < now() - make_interval(secs => $1)
		ORDER BY updated_at LIMIT 1`, float64(dur.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// WorkerIdle es un worker libre con su último momento de actividad.
type WorkerIdle struct {
	ID       string
	LibreDesde time.Time // última actividad; cero = nunca trabajó (libre desde siempre)
}

// FreeWorkersIdle devuelve workers libres desde hace >=idle (para handoff:
// la spec exige un worker libre que lleve >=300s sin actividad).
func (s *Store) FreeWorkersIdle(ctx context.Context, idle time.Duration) ([]WorkerIdle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id, coalesce(max(sq.updated_at), 'epoch'::timestamptz)
		FROM (VALUES ('w6'),('w7'),('w8'),('w9'),('w10')) AS w(id)
		LEFT JOIN subtask_queue sq
		  ON sq.worker_id = w.id AND sq.estado IN ('asignada','en_progreso')
		LEFT JOIN subtask_queue hist ON hist.worker_id = w.id
		WHERE sq.id IS NULL
		GROUP BY w.id
		HAVING max(hist.updated_at) IS NULL
		    OR max(hist.updated_at) < now() - make_interval(secs => $1)`,
		float64(idle.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerIdle{}
	for rows.Next() {
		var w WorkerIdle
		if err := rows.Scan(&w.ID, &w.LibreDesde); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// PreemptResult describe una preempción aplicada.
type PreemptResult struct {
	OriginalID string // subtarea reasignada (queda 'reasignada' con motivo)
	NuevaID    string // copia pendiente desde cero
	Worker     string // worker que tomó el trabajo (si se asignó ya)
}

// PreemptSubtarea reasigna desde cero una subtarea (preempción por idle o por
// worker colgado). Atómico en una transacción:
//  1. la original pasa a 'reasignada' con el motivo (resultado jsonb);
//  2. se copia como nueva fila 'pendiente' (mismo archivo/prompt/contexto);
//  3. opcionalmente se asigna directamente al worker indicado ('asignada').
func (s *Store) PreemptSubtarea(ctx context.Context, subID, motivo string, aWorker string) (*PreemptResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// leer la original
	var (
		taskID, filePath, prompt, contexto string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT task_id, coalesce(file_path,''), prompt, coalesce(contexto,'')
		FROM subtask_queue WHERE id=$1`, subID).
		Scan(&taskID, &filePath, &prompt, &contexto); err != nil {
		return nil, err
	}
	// solo reasignamos subtareas aún activas
	res, err := tx.ExecContext(ctx, `
UPDATE subtask_queue
		SET estado='reasignada', resultado=jsonb_build_object(
			'motivo', $2::text, 'antes', coalesce(resultado,'{}'::jsonb))
	WHERE id=$1 AND estado IN ('asignada','en_progreso')`, subID, motivo)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, fmt.Errorf("preempción: %s ya no está activa", subID[:8])
	}
	// copia desde cero
	estado := "pendiente"
	worker := ""
	if aWorker != "" {
		estado = "asignada"
		worker = aWorker
	}
	var nuevoID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO subtask_queue (task_id, worker_id, file_path, prompt, contexto, estado,
		                           heartbeat_at)
		VALUES ($1,$2,$3,$4,$5,$6, now())
		RETURNING id`,
		taskID, worker, filePath, prompt, contexto, estado).Scan(&nuevoID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	log.Printf("store: preempción %s (%s) → %s [%s]", subID[:8], motivo, nuevoID[:8], aWorker)
	return &PreemptResult{OriginalID: subID, NuevaID: nuevoID, Worker: aWorker}, nil
}

// TaskStats calcula el progreso de un task.
type TaskStats struct {
	Total       int `json:"total"`
	Completadas int `json:"completadas"`
	Fallidas    int `json:"fallidas"`
	Activas     int `json:"activas"`
}

// GetTaskStats calcula el progreso de un task.
func (s *Store) GetTaskStats(ctx context.Context, taskID string) (*TaskStats, error) {
	var st TaskStats
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE estado='completada'),
		       count(*) FILTER (WHERE estado='fallida'),
		       count(*) FILTER (WHERE estado IN ('pendiente','asignada','en_progreso'))
		FROM subtask_queue WHERE task_id=$1`, taskID).
		Scan(&st.Total, &st.Completadas, &st.Fallidas, &st.Activas)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// SetTaskFinal marca el task como completada o fallida (cuando ya no quedan activas).
func (s *Store) SetTaskFinal(ctx context.Context, taskID, estado string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET estado=$2, finished_at=now() WHERE id=$1`, taskID, estado)
	return err
}

// NotifPendiente es una subtarea finalizada pendiente de notificar a s2.
type NotifPendiente struct {
	SubID    string
	TaskID   string
	WorkerID string
	Estado   string
}

// PendingS2Notifications devuelve subtareas finalizadas sin notificar a s2
// (cola de reintentos en BD; s1 reintenta hasta entregar).
func (s *Store) PendingS2Notifications(ctx context.Context, limit int) ([]NotifPendiente, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, coalesce(worker_id,''), estado
		FROM subtask_queue
		WHERE estado IN ('completada','fallida') AND s2_notified_at IS NULL
		ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotifPendiente{}
	for rows.Next() {
		var n NotifPendiente
		if err := rows.Scan(&n.SubID, &n.TaskID, &n.WorkerID, &n.Estado); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkS2Notified registra que la notificación de fin llegó a s2.
func (s *Store) MarkS2Notified(ctx context.Context, subID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE subtask_queue SET s2_notified_at=now() WHERE id=$1`, subID)
	return err
}

// RawQuery ejecuta una consulta de solo lectura y devuelve filas como mapas
// (usado por endpoints de debug/verificación como /api/cola).
func (s *Store) RawQuery(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, c := range cols {
			switch v := vals[i].(type) {
			case []byte:
				m[c] = string(v)
			case time.Time:
				m[c] = v.Format(time.RFC3339)
			default:
				m[c] = v
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- Gestión de proxies (Fase 6 / B5) ---------------------------------------

// ProxyRow es una fila de proxy_pool.
type ProxyRow struct {
	ID            string  `json:"id"`
	ProxyURL      string  `json:"proxy_url"`
	Status        string  `json:"status"`
	Tipo          string  `json:"tipo"`
	Clasificacion string  `json:"clasificacion"`
	IPVista       string  `json:"ip_vista"`
	Oculta        *bool   `json:"oculta"`
	LatenciaMS    *int64  `json:"latencia_ms"`
	Motivo        string  `json:"motivo"`
	LastChecked   *string `json:"last_checked"`
}

// UpsertProxies inserta la lista descargada (únicos por proxy_url).
func (s *Store) UpsertProxies(ctx context.Context, entries []proxies.Entry) (nuevos, existentes int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	for _, e := range entries {
		proxyURL := proxies.ProxyURL(e.URL)
		res, err := tx.ExecContext(ctx, `
			INSERT INTO proxy_pool (proxy_url, tipo, status, source) VALUES ($1,$2,'libre','proxmint/free-proxy-list')
			ON CONFLICT (proxy_url) DO NOTHING`, proxyURL, e.Tipo)
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			nuevos++
		} else {
			existentes++
		}
	}
	return nuevos, existentes, tx.Commit()
}

// UpsertProxy inserta un proxy individual (no encontrado deja tipo por defecto).
func (s *Store) UpsertProxy(ctx context.Context, proxyURL string, tipo string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO proxy_pool (proxy_url, tipo, status, source) VALUES ($1,$2,'libre','manual')
		ON CONFLICT (proxy_url) DO UPDATE SET tipo=EXCLUDED.tipo`,
		proxyURL, tipo)
	return err
}

// ListProxies devuelve el pool (verificados y recientes primero).
func (s *Store) ListProxies(ctx context.Context, limit int) ([]ProxyRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, proxy_url, status, tipo, clasificacion,
		       coalesce(ip_vista,''), oculta, latencia_ms, coalesce(motivo,''),
		       to_char(last_checked, 'YYYY-MM-DD"T"HH24:MI:SS')
		FROM proxy_pool ORDER BY last_checked DESC NULLS LAST LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxyRow{}
	for rows.Next() {
		var p ProxyRow
		if err := rows.Scan(&p.ID, &p.ProxyURL, &p.Status, &p.Tipo, &p.Clasificacion,
			&p.IPVista, &p.Oculta, &p.LatenciaMS, &p.Motivo, &p.LastChecked); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProxyChecked guarda el resultado de una verificación (validado/descartado).
func (s *Store) SetProxyChecked(ctx context.Context, proxyURL, clasificacion, ipVista, motivo string, oculta *bool, latencia *int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE proxy_pool SET clasificacion=$2, ip_vista=$3, motivo=$4, oculta=$5,
			latencia_ms=$6, last_checked=now()
		WHERE proxy_url=$1`, proxyURL, clasificacion, ipVista, motivo, oculta, latencia)
	return err
}

// ProxyCount devuelve el total del pool.
func (s *Store) ProxyCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM proxy_pool`).Scan(&n)
	return n, err
}

// CountValidated devuelve cuántos proxies están clasificados como validados.
func (s *Store) CountValidated(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM proxy_pool WHERE clasificacion='validado'`).Scan(&n)
	return n, err
}

// AssignmentRow es una fila de proxy_assignments con el nombre del servicio.
type AssignmentRow struct {
	ID           string  `json:"id"`
	Servicio     string  `json:"servicio"`
	ProxyURL     string  `json:"proxy_url"`
	AsignadoAt   string  `json:"asignado_at"`
	Origen       string  `json:"origen"`
	Activo       bool    `json:"activo"`
	Confirmado   bool    `json:"confirmado"`
	ConfirmadoAt *string `json:"confirmado_at"`
	RetiradoAt   *string `json:"retirado_at"`
	Nombre       string  `json:"nombre"`
}

// ListAssignments devuelve todas las asignaciones (activas primero).
func (s *Store) ListAssignments(ctx context.Context) ([]AssignmentRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.servicio, a.proxy_url, to_char(a.asignado_at,'YYYY-MM-DD"T"HH24:MI:SS'),
			a.origen, a.activo, a.confirmado,
			to_char(a.confirmado_at,'YYYY-MM-DD"T"HH24:MI:SS'),
			to_char(a.retirado_at,'YYYY-MM-DD"T"HH24:MI:SS'),
			coalesce(ss.url_base,'')
		FROM proxy_assignments a
		LEFT JOIN services_status ss ON ss.id_servicio = a.servicio
		ORDER BY a.activo DESC, a.asignado_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssignmentRow{}
	for rows.Next() {
		var a AssignmentRow
		if err := rows.Scan(&a.ID, &a.Servicio, &a.ProxyURL, &a.AsignadoAt, &a.Origen,
			&a.Activo, &a.Confirmado, &a.ConfirmadoAt, &a.RetiradoAt, &a.Nombre); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AssignProxy asigna manualmente un proxy a un servicio (retira el anterior).
func (s *Store) AssignProxy(ctx context.Context, servicio, proxyURL, origen string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// retirar activas anteriores del servicio
	if _, err := tx.ExecContext(ctx, `
		UPDATE proxy_assignments SET activo=false, retirado_at=now()
		WHERE servicio=$1 AND activo`, servicio); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO proxy_assignments (servicio, proxy_url, origen) VALUES ($1,$2,$3)`,
		servicio, proxyURL, origen); err != nil {
		return err
	}
	// el pool queda asignado
	if _, err := tx.ExecContext(ctx, `
		UPDATE proxy_pool SET status='asignado' WHERE proxy_url=$1`, proxyURL); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmAssignment marca la asignación activa de un servicio como confirmada.
func (s *Store) ConfirmAssignment(ctx context.Context, servicio string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE proxy_assignments SET confirmado=true, confirmado_at=now()
		WHERE servicio=$1 AND activo`, servicio)
	return err
}

// RetireAssignment retira la asignación activa de un servicio (queda liberada).
func (s *Store) RetireAssignment(ctx context.Context, servicio string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE proxy_assignments SET activo=false, retirado_at=now()
		WHERE servicio=$1 AND activo`, servicio)
	return err
}

// BestFreeProxy devuelve el mejor proxy validado no asignado.
func (s *Store) BestFreeProxy(ctx context.Context, tipo string) (*string, error) {
	var url string
	err := s.db.QueryRowContext(ctx, `
		SELECT proxy_url FROM proxy_pool
		WHERE clasificacion='validado' AND status='libre' AND tipo=$1
		ORDER BY last_checked DESC NULLS LAST LIMIT 1`, tipo).Scan(&url)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &url, nil
}

// ProxyDestinations devuelve los destinos que admiten proxy: los 10 servicios
// del sistema (s1..s5, w6..w10). La spec asigna 1 proxy a CADA uno por hora.
func (s *Store) ProxyDestinations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id_servicio FROM services_status ORDER BY id_servicio`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ActiveProxyOf devuelve el proxy activo (y confirmado) de un servicio.
func (s *Store) ActiveProxyOf(ctx context.Context, servicio string) (*AssignmentRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, servicio, proxy_url, to_char(asignado_at,'YYYY-MM-DD"T"HH24:MI:SS'),
			origen, activo, confirmado, null, null, ''
		FROM proxy_assignments WHERE servicio=$1 AND activo
		ORDER BY asignado_at DESC LIMIT 1`, servicio)
	var a AssignmentRow
	if err := row.Scan(&a.ID, &a.Servicio, &a.ProxyURL, &a.AsignadoAt, &a.Origen,
		&a.Activo, &a.Confirmado, &a.ConfirmadoAt, &a.RetiradoAt, &a.Nombre); err != nil {
		return nil, err
	}
	return &a, nil
}

// LastProxyRefresh devuelve la última marca de refresh desde la fuente.
func (s *Store) LastProxyRefresh(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT coalesce(value,'') FROM settings WHERE key='proxy_last_refresh'`).Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return v, nil
}

// MarkProxyRefresh guarda la marca de refresh.
func (s *Store) MarkProxyRefresh(ctx context.Context, marca string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ('proxy_last_refresh',$1)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, marca)
	return err
}

// --- Tokens TUI (Fase 11 / E1) ----------------------------------------------
// Los tokens son de corta duración y de un solo uso: s1 emite, guarda SHA-256,
// y el servicio destino valida contra s1 (marcando el token como usado).

// EmitTUIToken genera un token único, guarda su hash y devuelve el token crudo.
// ttl es la validez (corta duración, p.ej. 10 minutos).
func (s *Store) EmitTUIToken(ctx context.Context, servicio, usuario string, ttl time.Duration) (string, error) {
	token, err := auth.RandToken(32)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	expira := time.Now().UTC().Add(ttl)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tui_access_tokens (token_hash, servicio, usuario, expira_at, usado)
		VALUES ($1,$2,$3,$4,false)`, hash, servicio, usuario, expira)
	if err != nil {
		return "", fmt.Errorf("guardando token TUI: %w", err)
	}
	return token, nil
}

// ValidateTUIToken consume un token de un solo uso para el servicio indicado.
// Devuelve el usuario si el token es válido (hash coincide, servicio correcto,
// no expirado y sin usar); en cualquier otro caso devuelve "".
func (s *Store) ValidateTUIToken(ctx context.Context, servicio, token string) (string, error) {
	if servicio == "" || token == "" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	var usuario string
	err := s.db.QueryRowContext(ctx, `
		UPDATE tui_access_tokens
		SET usado=true
		WHERE token_hash=$1 AND servicio=$2
		  AND usado=false AND expira_at > now()
		RETURNING usuario`, hash, servicio).Scan(&usuario)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return usuario, nil
}

// CleanupExpiredTokens borra tokens TUI vencidos (expira_at pasado) y usados
// con más de un día de antigüedad: evita que la tabla crezca sin límite.
func (s *Store) CleanupExpiredTokens(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM tui_access_tokens
		WHERE expira_at < now() - interval '1 day'
		   OR (usado AND expira_at < now())`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- SafePath y validaciones de seguridad (Fase 13 / F1) --------------------

// CleanServiceID normaliza un id_servicio (s1..s5, w6..w10).
func CleanServiceID(id string) string {
	id = strings.TrimSpace(id)
	switch id {
	case "s1", "s2", "s3", "s4", "s5", "w6", "w7", "w8", "w9", "w10":
		return id
	}
	return ""
}
