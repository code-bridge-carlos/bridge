// Package proxies: obtención y verificación real de proxies (Fase 6).
//
// Fuente: proxmint/free-proxy-list (CC BY 4.0) — proxies/all.txt (protocol://ip:port).
// Verificación: GET https://api.ipify.org A TRAVÉS del proxy (transporte CONNECT
// nativo de Go para http/https; socks4/5 se guardan pero no se verifican en esta fase).
package proxies

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ListURL es la fuente de la lista re-validada cada 30 min.
const ListURL = "https://raw.githubusercontent.com/proxmint/free-proxy-list/main/proxies/all.txt"

// IPApi es el endpoint que devuelve la IP pública de la conexión.
const IPApi = "https://api.ipify.org"

// Entry es un proxy de la lista.
type Entry struct {
	URL  string // protocol://ip:port
	Tipo string // http | https | socks4 | socks5
}

// FetchList descarga y parsea la lista desde GitHub.
func FetchList(ctx context.Context, maxEntries int) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ListURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "s1-panel/1.0 (B5) +https://github.com/proxmint/free-proxy-list")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("descargando lista: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lista respondió %s", resp.Status)
	}
	return ParseList(resp.Body, maxEntries)
}

// ParseList convierte el texto de all.txt en entradas (ignorando basura).
func ParseList(r io.Reader, max int) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urlStr := line
		tipo := "http"
		if !strings.Contains(urlStr, "://") {
			// all.txt usa protocol://ip:port; si no, inferir
			urlStr = "http://" + urlStr
		} else {
			parts := strings.SplitN(urlStr, "://", 2)
			tipo = strings.ToLower(parts[0])
		}
		out = append(out, Entry{URL: urlStr, Tipo: tipo})
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, sc.Err()
}

// CheckResult es el resultado de una verificación real.
type CheckResult struct {
	IP      string // IP vista por el destino remoto a través del proxy
	Latency int64  // milisegundos de ida y vuelta
	Err     error  // si no es alcanzable
}

// CheckIP verifica el proxy haciendo una petición REAL a IPApi a través de él.
// timeout es el tiempo máximo total.
func CheckIP(ctx context.Context, proxyURL string, timeout time.Duration) CheckResult {
	// Normalizar: socks4/5 no verificables en B5 (transporte no soportado)
	purl, err := url.Parse(proxyURL)
	if err != nil {
		return CheckResult{Err: fmt.Errorf("url de proxy inválida: %v", err)}
	}
	switch strings.ToLower(purl.Scheme) {
	case "http", "https": // ok (CONNECT + reenvío nativo)
	case "socks4", "socks5":
		return CheckResult{Err: fmt.Errorf("socks no soportado en B5 (Fase 13)")}
	default:
		return CheckResult{Err: fmt.Errorf("esquema %q no soportado", purl.Scheme)}
	}

	cli := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:       http.ProxyURL(purl),
			DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
		},
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, IPApi, nil)
	if err != nil {
		return CheckResult{Err: err}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return CheckResult{Err: fmt.Errorf("no alcanzable vía proxy: %v", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CheckResult{Err: fmt.Errorf("el destino respondió %s", resp.Status)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return CheckResult{Err: err}
	}
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return CheckResult{Err: fmt.Errorf("respuesta no-IP: %q", ip)}
	}
	return CheckResult{IP: ip, Latency: time.Since(start).Milliseconds()}
}

// DirectIP obtiene la IP pública SIN proxy (línea base para oculta).
func DirectIP(ctx context.Context, timeout time.Duration) (string, error) {
	cli := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, IPApi, nil)
	if err != nil {
		return "", err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("respuesta no-IP: %q", ip)
	}
	return ip, nil
}

// ProxyURL normaliza http://ip:port.
func ProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if !strings.Contains(raw, "://") {
		return "http://" + raw
	}
	return raw
}
