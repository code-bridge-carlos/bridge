#!/usr/bin/env python3
"""Mini proxy HTTP local para verificación determinista de B5.

Implementa un proxy HTTP con CONNECT (túneles HTTPS) + reenvío HTTP plano,
suficiente para comprobar el flujo de s1 (checker GetIP vía proxy) sin
depender de la fiabilidad de proxys públicos.

Uso: python3 proxy-local.py [puerto=8899]
"""
import socket
import sys
import threading
import urllib.parse

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8899
BUFSZ = 65536


def relay(src, dst):
    try:
        while True:
            data = src.recv(BUFSZ)
            if not data:
                break
            dst.sendall(data)
    except Exception:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except Exception:
            pass


def split_hostport(hostport):
    """'host:port' → (host, port). Soporta IPv6 [::1]:puerto."""
    hostport = hostport.strip()
    if hostport.startswith("["):
        host, _, port = hostport[1:].partition("]:")
        return host, int(port or 80)
    host, _, port = hostport.rpartition(":")
    if not host:
        return hostport, 80
    return host, int(port or 80)


def handle_http(client):
    # lee la primera línea (método + URL) y cabeceras hasta \r\n\r\n
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = client.recv(BUFSZ)
        if not chunk:
            client.close()
            return
        data += chunk
        if len(data) > 1 << 20:
            client.close()
            return
    first = data.split(b"\r\n")[0].decode("latin1")
    parts = first.split(" ")
    if len(parts) < 3:
        client.close()
        return
    method, target = parts[0], parts[1]

    if method == "CONNECT":
        # túnel HTTPS: responder 200 y reenviar a ciegas
        hostport = target.split("/")[0]
        try:
            upstream = socket.create_connection(split_hostport(hostport), timeout=10)
        except Exception:
            client.close()
            return
        client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        a = threading.Thread(target=relay, args=(client, upstream), daemon=True)
        b = threading.Thread(target=relay, args=(upstream, client), daemon=True)
        a.start(); b.start()
        a.join(); b.join()
        client.close()
        return

    # HTTP plano: quitar cabeceras proxy y reenviar tal cual
    # (Go http.Server acepta absolute-form en la request line)
    hostport = urllib.parse.urlsplit(target).netloc or "localhost"
    try:
        upstream = socket.create_connection(split_hostport(hostport), timeout=10)
    except Exception:
        client.close()
        return
    lines = data.split(b"\r\n")
    clean = [ln for ln in lines
             if not (ln.startswith(b"Proxy-Connection")
                     or ln.startswith(b"Proxy-Authorization"))]
    upstream.sendall(b"\r\n".join(clean))
    a = threading.Thread(target=relay, args=(client, upstream), daemon=True)
    b = threading.Thread(target=relay, args=(upstream, client), daemon=True)
    a.start(); b.start()
    a.join(); b.join()
    client.close()


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("0.0.0.0", PORT))
    srv.listen(16)
    print(f"[proxy-local] escuchando en :{PORT}", flush=True)
    while True:
        conn, _ = srv.accept()
        threading.Thread(target=handle_http, args=(conn,), daemon=True).start()


if __name__ == "__main__":
    main()