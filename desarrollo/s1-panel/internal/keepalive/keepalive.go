// Package keepalive: loop que pingea /health de los 10 servicios cada 10
// minutos cuando el toggle global está activo. Registra up/down + latencia
// en services_status (nunca en RAM).
package keepalive

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"s1-panel/internal/store"
)

// Interval es el periodo entre pings (10 min en producción; configurable).
var Interval = 10 * time.Minute

// Servicio aliases para no acoplar nombres de paquete.
type ServiceStatus = store.ServiceStatus

// Runner ejecuta el loop de keep-alive en segundo plano.
type Runner struct {
	st     *store.Store
	client *http.Client
	url    func(id string) string // devuelve URL base del servicio (sin /health)
}

// New crea un Runner. urlFn mapea id_servicio → URL base.
func New(st *store.Store, urlFn func(id string) string) *Runner {
	return &Runner{
		st:     st,
		client: &http.Client{Timeout: 5 * time.Second},
		url:    urlFn,
	}
}

// Start lanza el loop en una goroutine hasta que ctx se cancele.
func (r *Runner) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		// Ping inmediato al arrancar (persistir RAM/estado en services_status).
		r.pingAll(ctx)
		for {
			select {
			case <-ctx.Done():
				log.Println("keep-alive: detenido")
				return
			case <-ticker.C:
				r.pingAll(ctx)
			}
		}
	}()
	log.Printf("keep-alive: loop activo cada %v (estado global: %v)", Interval, r.st.KeepAliveState(ctx))
}

// pingAll verifica los 10 servicios y actualiza services_status.
func (r *Runner) pingAll(ctx context.Context) {
	on := r.st.KeepAliveState(ctx)
	if !on {
		return
	}
	svcs, err := r.st.ListServices(ctx)
	if err != nil {
		log.Printf("keep-alive: error listando servicios: %v", err)
		return
	}
	for _, sv := range svcs {
		if sv.ID == "s1" {
			continue // s1 no se pingea a sí mismo
		}
		r.pingOne(ctx, sv.ID, baseURLOf(sv))
	}
}

// baseURLOf deriva la URL base de un servicio. Prioriza url_base (usado por el
// keep-alive para el health-check); si está vacío, lo deriva de url_tui
// (http://localhost:9002/tui → http://localhost:9002) por compatibilidad.
func baseURLOf(sv store.ServiceStatus) string {
	if strings.TrimSpace(sv.URLBase) != "" {
		return strings.TrimSuffix(sv.URLBase, "/")
	}
	u := strings.TrimSuffix(sv.URLTUI, "/")
	if i := strings.LastIndex(u, "/tui"); i >= 0 {
		return u[:i]
	}
	return u
}

// pingOne mide latencia y estado de un servicio.
func (r *Runner) pingOne(ctx context.Context, id, baseURL string) {
	if baseURL == "" {
		r.st.UpdateServiceStatus(ctx, id, "down", 0, 0)
		return
	}
	start := time.Now()
	resp, err := r.client.Get(baseURL + "/health")
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		log.Printf("keep-alive: %s DOWN (%v)", id, err)
		r.st.UpdateServiceStatus(ctx, id, "down", latency, 0)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		ramMB := 0
		var hdr struct{ RamMB int `json:"ram_mb"` }
		if err := json.NewDecoder(resp.Body).Decode(&hdr); err == nil && hdr.RamMB > 0 {
			ramMB = hdr.RamMB
		}
		log.Printf("keep-alive: %s UP (%dms, %dMB)", id, latency, ramMB)
		r.st.UpdateServiceStatus(ctx, id, "up", latency, ramMB)
	} else {
		log.Printf("keep-alive: %s estado HTTP %d", id, resp.StatusCode)
		r.st.UpdateServiceStatus(ctx, id, "down", latency, 0)
	}
}

// FormatURL construye la URL base de un servicio para keep-alive
// (usada cuando no hay URL registrada en DB todavía).
func FormatURL(publicHost string) func(string) string {
	return func(id string) string {
		return fmt.Sprintf("%s/%s", publicHost, id)
	}
}
