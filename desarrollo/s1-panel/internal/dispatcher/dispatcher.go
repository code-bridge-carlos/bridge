// Package dispatcher: asigna subtareas pendientes a workers libres y aplica
// la preempción por idle de la spec de s1.
// Corre tras encolar, tras terminar una subtarea y en un ticker periódico.
// Toda la lógica vive en la base de datos (s1 stateless).
package dispatcher

import (
	"context"
	"log"
	"time"

	"s1-panel/internal/store"
)

// Dispatcher asigna pendientes → libres y vigila preempción por idle.
type Dispatcher struct {
	st        *store.Store
	idleAfter time.Duration // ocupada >= idleAfter sin handoff → handoff (spec: 300s)
	staleAfter time.Duration // sin heartbeat en staleAfter → colgada (spec: 60s)
	handoffIdle time.Duration // worker libre desde >= handoffIdle puede recibir handoff
}

// New crea un Dispatcher con los tiempos de preempción de la spec.
func New(st *store.Store) *Dispatcher {
	return &Dispatcher{
		st:          st,
		idleAfter:   300 * time.Second,
		staleAfter:  60 * time.Second,
		handoffIdle: 300 * time.Second,
	}
}

// SetPreemptTimings configura los tiempos de preempción (tests/desarrollo).
func (d *Dispatcher) SetPreemptTimings(idleAfter, staleAfter, handoffIdle time.Duration) {
	if idleAfter > 0 {
		d.idleAfter = idleAfter
	}
	if staleAfter > 0 {
		d.staleAfter = staleAfter
	}
	if handoffIdle > 0 {
		d.handoffIdle = handoffIdle
	}
}

// preempt revisa y aplica la preempción por idle:
//  1. subtareas colgadas (>=staleAfter sin heartbeat) → reasignar desde cero
//     a un worker libre (60s de la spec: "no responde → reasignar + restart").
//  2. subtareas ocupadas >=idleAfter cuando hay un worker libre >=handoffIdle
//     → handoff (300s de la spec: la ocupada devuelve progreso, la libre toma
//     el trabajo desde cero).
func (d *Dispatcher) preempt(ctx context.Context) (int, error) {
	preempted := 0

	// 1. colgadas (sin señal de vida): el worker está muerto; reasignar desde
	//    cero al mejor worker libre que exista.
	stale, err := d.st.StaleSubtareas(ctx, d.staleAfter)
	if err != nil {
		return 0, err
	}
	for _, subID := range stale {
		libres, err := d.st.FreeWorkers(ctx)
		if err != nil {
			return 0, err
		}
		if len(libres) == 0 {
			return preempted, nil // sin nadie que tome el trabajo: esperar
		}
		if _, err := d.st.PreemptSubtarea(ctx, subID, "colgada_sin_heartbeat", libres[0]); err != nil {
			log.Printf("dispatcher: preempción colgada %s: %v", subID[:8], err)
			continue
		}
		log.Printf("dispatcher: colgada %s reasignada a %s", subID[:8], libres[0])
		preempted++
	}

	// 2. handoff por idle: una subtarea lleva >=idleAfter con el mismo worker
	//    y existe un worker libre desde >=handoffIdle → el trabajo pasa.
	longRun, err := d.st.LongRunningSubtareas(ctx, d.idleAfter)
	if err != nil {
		return preempted, err
	}
	libresIdle, err := d.st.FreeWorkersIdle(ctx, d.handoffIdle)
	if err != nil {
		return preempted, err
	}
	for _, subID := range longRun {
		if len(libresIdle) == 0 {
			break
		}
		destino := libresIdle[0].ID
		if _, err := d.st.PreemptSubtarea(ctx, subID, "handoff_por_idle", destino); err != nil {
			log.Printf("dispatcher: handoff %s: %v", subID[:8], err)
			libresIdle = libresIdle[1:]
			continue
		}
		log.Printf("dispatcher: handoff %s → %s (idle)", subID[:8], destino)
		preempted++
		libresIdle = libresIdle[1:] // ese worker ya tiene carga
	}

	return preempted, nil
}

// Dispatch intenta asignar las subtareas pendientes a workers libres.
// Devuelve cuántas asignó.
func (d *Dispatcher) Dispatch(ctx context.Context) (int, error) {
	pend, err := d.st.PendingSubtaskIDs(ctx, 5)
	if err != nil {
		return 0, err
	}
	if len(pend) == 0 {
		return 0, nil
	}
	libres, err := d.st.FreeWorkers(ctx)
	if err != nil {
		return 0, err
	}
	asignadas := 0
	for i := 0; i < len(pend) && i < len(libres); i++ {
		if err := d.st.AssignSubtask(ctx, pend[i], libres[i]); err != nil {
			log.Printf("dispatcher: no asigné %s a %s: %v", pend[i][:8], libres[i], err)
			continue
		}
		log.Printf("dispatcher: %s → %s", pend[i][:8], libres[i])
		asignadas++
	}
	return asignadas, nil
}

// Tick ejecuta preempción y asignación (también tras encolar/terminar).
func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	n, err := d.preempt(ctx)
	if err != nil {
		return n, err
	}
	m, err := d.Dispatch(ctx)
	return n + m, err
}

// Start lanza el ticker periódico (cada 5s revisa cola y preempción).
func (d *Dispatcher) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Println("dispatcher: detenido")
				return
			case <-ticker.C:
				if n, err := d.Tick(ctx); err != nil {
					log.Printf("dispatcher: error: %v", err)
				} else if n > 0 {
					log.Printf("dispatcher: %d acciones por ticker", n)
				}
			}
		}
	}()
	log.Println("dispatcher: ticker cada 5s")
}