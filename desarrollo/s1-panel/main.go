// s1-panel: panel web del sistema multiagente. Liger, stateless, en Go.
//
// Arranque del servidor: lee configuración de variables de entorno,
// conecta a PostgreSQL (Supabase), monta rutas y lanza keep-alive.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"s1-panel/internal/auth"
	"s1-panel/internal/dispatcher"
	"s1-panel/internal/keepalive"
	"s1-panel/internal/store"
	"s1-panel/internal/web"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func main() {
	port := flag.String("port", envOr("PORT", "8080"), "puerto HTTP")
	flag.Parse()

	// --- Configuración ------------------------------------------------
	// En producción (APP_ENV=production) los secretos son OBLIGATORIOS: sin
	// ellos el proceso aborta. Los fallbacks de desarrollo solo se usan al
	// arrancar con APP_ENV=development y son explícitos en los logs.
	prod := envOr("APP_ENV", "development") == "production"
	user := envOr("PANEL_USER", "admin")
	pass := os.Getenv("PANEL_PASSWORD")
	if pass == "" {
		if prod {
			log.Fatalf("s1: PANEL_PASSWORD es obligatoria en producción (APP_ENV=production)")
		}
		pass = "admin123" // SOLO para desarrollo local
		log.Printf("AVISO: PANEL_PASSWORD no definida, usando 'admin123' (SOLO dev)")
	}
	secret := os.Getenv("SESSION_SECRET")
	if len(secret) < 16 {
		if prod {
			log.Fatalf("s1: SESSION_SECRET es obligatoria en producción (mínimo 16 caracteres)")
		}
		secret = "dev-secret-no-usable-en-produccion-123456"
		log.Printf("AVISO: SESSION_SECRET no definida (o corta), usando valor de desarrollo")
	}
	control := os.Getenv("CONTROL_TOKEN")
	if control == "" {
		if prod {
			log.Fatalf("s1: CONTROL_TOKEN es obligatorio en producción (workers y TUIs)")
		}
		log.Printf("AVISO: CONTROL_TOKEN no definido: endpoints de workers devolverán 503 (SOLO dev)")
	}
	dbURL := envOr("DATABASE_URL", "postgres://postgres:test@localhost:5433/testdb?sslmode=disable")

	// --- Dependencias --------------------------------------------------
	st, err := store.Open(dbURL)
	if err != nil {
		log.Fatalf("s1: no pude conectar a la base de datos: %v", err)
	}
	defer st.Close()

	am, err := auth.NewManager(secret, 12*time.Hour)
	if err != nil {
		log.Fatalf("s1: %v", err)
	}

	// --- Dispatcher (cola de subtareas, Fase 5 + preempción por idle A4) ---
	ctxDisp, cancelDisp := context.WithCancel(context.Background())
	defer cancelDisp()
	disp := dispatcher.New(st)
	disp.SetPreemptTimings(
		time.Duration(atoiOr(envOr("PREEMPT_IDLE_SECONDS", "300"), 300))*time.Second,
		time.Duration(atoiOr(envOr("PREEMPT_STALE_SECONDS", "60"), 60))*time.Second,
		time.Duration(atoiOr(envOr("PREEMPT_HANDOFF_IDLE_SECONDS", "300"), 300))*time.Second,
	)
	disp.Start(ctxDisp)

	srv, err := web.New(st, am, user, pass, envOr("COOKIE_SECURE", "false") == "true",
		envOr("S2_URL", ""), envOr("S2_NOTIFY_URL", ""), control,
		disp.Tick)
	if err != nil {
		log.Fatalf("s1: %v", err)
	}
	srv.SetProxyMax(atoiOr(envOr("PROXY_MAX_IMPORT", "100"), 100))
	srv.SetProxyCheckBatch(atoiOr(envOr("PROXY_CHECK_BATCH", "10"), 10))
	srv.SetBaseURL(envOr("SERVICES_BASE_URL", ""))

	// --- Notificador a s2: reintenta envíos de fin de subtarea pendientes ---
	ctxNotif, cancelNotif := context.WithCancel(context.Background())
	defer cancelNotif()
	srv.StartS2Notifier(ctxNotif)

	// --- Limpieza de tokens TUI vencidos (cada hora) ----------------------
	ctxClean, cancelClean := context.WithCancel(context.Background())
	defer cancelClean()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctxClean.Done():
				log.Println("limpieza tokens: detenido")
				return
			case <-ticker.C:
				if n, err := st.CleanupExpiredTokens(ctxClean); err != nil {
					log.Printf("limpieza tokens: %v", err)
				} else if n > 0 {
					log.Printf("limpieza tokens: %d vencidos/usados eliminados", n)
				}
			}
		}
	}()

	// --- Cron de proxies: refresh cada hora al minuto 00 (Fase 6) -------
	ctxProxy, cancelProxy := context.WithCancel(context.Background())
	defer cancelProxy()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctxProxy.Done():
				log.Println("cron proxies: detenido")
				return
			case <-ticker.C:
				if time.Now().Minute() != 0 {
					continue // solo al minuto 00
				}
				// evitar doble refresh en el mismo minuto
				prefix := time.Now().Format("2006-01-02 15:04")
				if m, _ := st.LastProxyRefresh(ctxProxy); m != "" && strings.HasPrefix(m, prefix) {
					continue
				}
				log.Println("cron: hora en punto → refresh de proxies")
				srv.RefreshProxies(ctxProxy)
			}
		}
	}()

	// --- Keep-alive ----------------------------------------------------
	ctxKA, cancelKA := context.WithCancel(context.Background())
	defer cancelKA()
	// En desarrollo, los servicios aún no tienen URL pública; el runner
	// marcará "down" hasta que la DB tenga URLs (se poblarán en Fase 14).
	ka := keepalive.New(st, keepalive.FormatURL(envOr("SERVICES_BASE_URL", "http://localhost")))
	ka.Start(ctxKA)

	// --- Servidor ------------------------------------------------------
	mux := http.NewServeMux()
	srv.Routes(mux)

	srvHTTP := &http.Server{
		Addr:              ":" + *port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("s1 panel escuchando en http://localhost:%s", *port)
		if err := srvHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("s1: servidor: %v", err)
		}
	}()

	// --- Shutdown limpio ----------------------------------------------
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("s1: apagando...")
	cancelKA()
	cancelNotif()
	cancelClean()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srvHTTP.Shutdown(ctx)
	log.Println("s1: apagado")
}
