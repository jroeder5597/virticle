package webaccess

import (
	"net"
	"strings"
)

// Snippet is ready-to-copy configuration for one reverse proxy.
type Snippet struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	File  string `json:"file"` // where it goes
	Notes string `json:"notes"`
	Text  string `json:"text"`
}

// SampleHost is used when no public URL is set yet.
const SampleHost = "virticle.example.org"

type snipVars struct {
	Host    string // public host name, no port
	Backend string // host:port the proxy connects to
	Proxy   string // address the controller will see the proxy at
}

func varsFor(s Settings) snipVars {
	v := snipVars{Host: SampleHost, Backend: "127.0.0.1:8080", Proxy: "127.0.0.1"}
	if u, err := ParsePublicURL(s.PublicURL); err == nil {
		v.Host = u.Hostname()
	}
	if h, p, err := net.SplitHostPort(s.Listen); err == nil && p != "" {
		if h == "" || net.ParseIP(h) != nil && net.ParseIP(h).IsUnspecified() {
			h = "127.0.0.1"
		}
		v.Backend = net.JoinHostPort(h, p)
	}
	if len(s.TrustedProxies) > 0 {
		v.Proxy = s.TrustedProxies[0]
	}
	return v
}

// Snippets generates the configuration of the five supported proxies for the
// current settings. The output is deterministic (golden files test it).
func Snippets(s Settings) []Snippet {
	v := varsFor(s)
	r := strings.NewReplacer("@HOST@", v.Host, "@BACKEND@", v.Backend, "@PROXY@", v.Proxy)
	mk := func(id, name, file, notes, text string) Snippet {
		return Snippet{ID: id, Name: name, File: file, Notes: notes, Text: r.Replace(strings.TrimLeft(text, "\n"))}
	}
	return []Snippet{
		mk("nginx", "nginx", "/etc/nginx/conf.d/virticle.conf",
			"Reload with nginx -s reload. Replace the certificate paths with your own.", nginxText),
		mk("apache", "Apache httpd", "/etc/apache2/sites-available/virticle.conf",
			"Enable the modules first: a2enmod ssl proxy proxy_http proxy_wstunnel rewrite headers. Then a2ensite virticle and reload.", apacheText),
		mk("haproxy", "HAProxy", "/etc/haproxy/haproxy.cfg",
			"Add to the existing configuration. The pem file holds the certificate chain followed by the key.", haproxyText),
		mk("caddy", "Caddy", "/etc/caddy/Caddyfile",
			"Caddy obtains and renews the certificate by itself when the name is public.", caddyText),
		mk("traefik", "Traefik", "traefik.yml and a file-provider dynamic configuration",
			"The first block is static configuration, the second is dynamic. Set certResolver if Traefik should use ACME.", traefikText),
	}
}

const nginxText = `
# Controller: trusted proxy address @PROXY@, public URL https://@HOST@
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 80;
    server_name @HOST@;
    return 308 https://$host$request_uri;
}

server {
    listen 443 ssl;
    http2 on;
    server_name @HOST@;

    ssl_certificate     /etc/ssl/@HOST@/fullchain.pem;
    ssl_certificate_key /etc/ssl/@HOST@/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;

    # Volume and ISO uploads are large: no size limit, no buffering to disk.
    client_max_body_size 0;
    proxy_request_buffering off;
    proxy_buffering off;

    location / {
        proxy_pass http://@BACKEND@;
        proxy_http_version 1.1;

        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host  $host;

        # WebSockets: /api/events, VNC and serial consoles under /api/nodes/
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection $connection_upgrade;

        # Consoles and uploads stay open for a long time.
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
`

const apacheText = `
# Controller: trusted proxy address @PROXY@, public URL https://@HOST@
<VirtualHost *:80>
    ServerName @HOST@
    Redirect permanent / https://@HOST@/
</VirtualHost>

<VirtualHost *:443>
    ServerName @HOST@

    SSLEngine on
    SSLCertificateFile    /etc/ssl/@HOST@/fullchain.pem
    SSLCertificateKeyFile /etc/ssl/@HOST@/privkey.pem
    SSLProtocol           -all +TLSv1.2 +TLSv1.3

    ProxyPreserveHost On
    # X-Forwarded-For is added by mod_proxy itself.
    RequestHeader set X-Forwarded-Proto "https"
    RequestHeader set X-Forwarded-Host  "@HOST@"

    # Uploads are large (no limit is the default); consoles stay open.
    LimitRequestBody 0
    ProxyTimeout 3600

    # WebSockets: /api/events, VNC and serial consoles under /api/nodes/
    RewriteEngine On
    RewriteCond %{HTTP:Upgrade} =websocket [NC]
    RewriteRule ^/(.*)$ ws://@BACKEND@/$1 [P,L]

    ProxyPass        / http://@BACKEND@/ flushpackets=on
    ProxyPassReverse / http://@BACKEND@/
</VirtualHost>
`

const haproxyText = `
# Controller: trusted proxy address @PROXY@, public URL https://@HOST@
defaults
    mode http
    timeout connect 5s
    timeout client  1h
    timeout server  1h
    # WebSockets (/api/events, VNC and serial consoles) after the upgrade
    timeout tunnel  1h

frontend virticle_http
    bind :80
    http-request redirect scheme https code 308 unless { ssl_fc }

frontend virticle_https
    bind :443 ssl crt /etc/haproxy/certs/@HOST@.pem alpn h2,http/1.1 ssl-min-ver TLSv1.2
    http-request set-header X-Forwarded-Proto https
    http-request set-header X-Forwarded-Host  %[req.hdr(Host)]
    http-request set-header X-Forwarded-For   %[src]
    default_backend virticle

backend virticle
    # Request bodies are streamed, not buffered, so large uploads pass.
    server virticle @BACKEND@ check
`

const caddyText = `
# Controller: trusted proxy address @PROXY@, public URL https://@HOST@
@HOST@ {
    reverse_proxy @BACKEND@ {
        # X-Forwarded-For/-Proto/-Host and WebSocket upgrades are handled by default.
        # Stream requests and responses; do not buffer uploads or console data.
        flush_interval -1
        transport http {
            read_timeout  1h
            write_timeout 1h
        }
    }
    request_body {
        max_size 0
    }
}
`

const traefikText = `
# Controller: trusted proxy address @PROXY@, public URL https://@HOST@
# --- static configuration (traefik.yml) ---
entryPoints:
  web:
    address: ":80"
    http:
      redirections:
        entryPoint:
          to: websecure
          scheme: https
          permanent: true
  websecure:
    address: ":443"
    transport:
      respondingTimeouts:
        readTimeout: 0s    # uploads and consoles stay open
        writeTimeout: 0s
        idleTimeout: 1h
providers:
  file:
    filename: /etc/traefik/virticle.yml

# --- dynamic configuration (/etc/traefik/virticle.yml) ---
http:
  routers:
    virticle:
      rule: "Host(` + "`@HOST@`" + `)"
      entryPoints: [websecure]
      service: virticle
      tls: {}              # add certResolver: <name> to use ACME
  services:
    virticle:
      loadBalancer:
        passHostHeader: true
        responseForwarding:
          flushInterval: 1ms
        servers:
          - url: "http://@BACKEND@"
`
