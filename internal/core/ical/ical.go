// Package ical lee los calendarios iCal (RFC 5545) que exportan Airbnb,
// Booking.com y los demas canales (fase04.md §3, job ical_sync).
//
// Solo interesan los VEVENT con UID, DTSTART y DTEND (o DURATION en dias); el
// resto del formato se ignora. Se escribio aqui en vez de usar una dependencia
// porque el subconjunto es pequeno y estable.
//
// La descarga no puede alcanzar la red interna: el servidor lee URLs que
// escribe la empresa (SSRF). El dialer revisa la IP ya resuelta (evita DNS
// rebinding) y las redirecciones pasan por el mismo filtro.
package ical

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Event es una estancia leida del calendario.
type Event struct {
	UID       string
	Start     string // AAAA-MM-DD
	End       string // AAAA-MM-DD, exclusiva
	Summary   string
	Cancelled bool
}

// Error es un error de lectura con un mensaje para la persona (se guarda en
// el calendario y se muestra en W-H6).
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// Errores de lectura.
var (
	ErrURL      = &Error{"Pega la dirección https:// que te da el canal para exportar el calendario."}
	ErrConnect  = &Error{"No se pudo conectar con el canal."}
	ErrPrivate  = &Error{"La dirección apunta a una red privada."}
	ErrTooLarge = &Error{"El calendario es demasiado grande."}
	ErrFormat   = &Error{"La dirección no devuelve un calendario iCal."}
	ErrTimeout  = &Error{"El canal tardó demasiado en responder."}
)

const maxBytes = 5 << 20

// Fetcher descarga calendarios.
type Fetcher struct {
	client       *http.Client
	allowPrivate bool
}

// NewFetcher crea el cliente. allowPrivate (solo local y pruebas) deja leer
// localhost y redes privadas, y http sin TLS.
func NewFetcher(allowPrivate bool) *Fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip != nil && !allowPrivate && privateIP(ip) {
			return ErrPrivate
		}
		return nil
	}}
	transport := &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second, MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second,
	}
	client := &http.Client{
		Transport: transport, Timeout: 25 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return ErrConnect
			}
			return checkURL(req.URL, allowPrivate)
		},
	}
	return &Fetcher{client: client, allowPrivate: allowPrivate}
}

// privateIP indica direcciones que no deben leerse desde el servidor.
func privateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 // 100.64.0.0/10
}

func checkURL(u *url.URL, allowPrivate bool) error {
	if u.Host == "" || u.User != nil || (u.Scheme != "https" && (!allowPrivate || u.Scheme != "http")) {
		return ErrURL
	}
	return nil
}

// ValidURL normaliza (webcal:// pasa a https://) y valida la URL.
func (f *Fetcher) ValidURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(raw), "webcal://") {
		raw = "https://" + raw[len("webcal://"):]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrURL
	}
	if err := checkURL(u, f.allowPrivate); err != nil {
		return "", err
	}
	return u.String(), nil
}

// Fetch descarga y lee el calendario. loc es la zona de la empresa (para
// eventos con hora en UTC).
func (f *Fetcher) Fetch(ctx context.Context, raw string, loc *time.Location) ([]Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, ErrURL
	}
	req.Header.Set("User-Agent", "ARGOS-iCal/1.0")
	req.Header.Set("Accept", "text/calendar, text/plain;q=0.8, */*;q=0.5")
	res, err := f.client.Do(req)
	if err != nil {
		return nil, fetchError(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, &Error{fmt.Sprintf("El canal respondió %d.", res.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, fetchError(err)
	}
	if len(body) > maxBytes {
		return nil, ErrTooLarge
	}
	return Parse(string(body), loc)
}

func fetchError(err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, ErrPrivate):
		return ErrPrivate
	case errors.Is(err, ErrURL):
		return ErrURL
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return ErrTimeout
	default:
		return ErrConnect
	}
}

// Parse lee los VEVENT de un calendario.
func Parse(body string, loc *time.Location) ([]Event, error) {
	lines := unfold(body)
	if len(lines) == 0 || !strings.EqualFold(strings.TrimSpace(lines[0]), "BEGIN:VCALENDAR") {
		return nil, ErrFormat
	}
	out := []Event{}
	var cur *rawEvent
	for _, line := range lines {
		name, params, value := splitLine(line)
		switch {
		case name == "BEGIN" && strings.EqualFold(value, "VEVENT"):
			cur = &rawEvent{}
		case name == "END" && strings.EqualFold(value, "VEVENT") && cur != nil:
			if ev, ok := cur.event(loc); ok {
				out = append(out, ev)
			}
			cur = nil
		case cur != nil:
			cur.set(name, params, value)
		}
	}
	return out, nil
}

type rawEvent struct {
	uid, summary, status, duration string
	start, end                     string
	startParams, endParams         string
}

func (r *rawEvent) set(name, params, value string) {
	switch name {
	case "UID":
		r.uid = strings.TrimSpace(value)
	case "SUMMARY":
		r.summary = unescape(value)
	case "STATUS":
		r.status = strings.ToUpper(strings.TrimSpace(value))
	case "DTSTART":
		r.start, r.startParams = value, params
	case "DTEND":
		r.end, r.endParams = value, params
	case "DURATION":
		r.duration = strings.ToUpper(strings.TrimSpace(value))
	}
}

func (r *rawEvent) event(loc *time.Location) (Event, bool) {
	start, ok := date(r.start, r.startParams, loc)
	if r.uid == "" || !ok {
		return Event{}, false
	}
	end, ok := date(r.end, r.endParams, loc)
	if !ok {
		end = addDays(start, durationDays(r.duration))
	}
	if end <= start {
		end = addDays(start, 1)
	}
	return Event{
		UID: cut(r.uid, 255), Start: start, End: end, Summary: cut(r.summary, 255), Cancelled: r.status == "CANCELLED",
	}, true
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// date lee DTSTART/DTEND como fecha local: VALUE=DATE (20261205), hora
// flotante o con TZID (se toma su fecha) y hora UTC (se pasa a la zona de la
// empresa).
func date(value, params string, loc *time.Location) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 8 {
		return "", false
	}
	if strings.HasSuffix(value, "Z") && len(value) == 16 {
		t, err := time.Parse("20060102T150405Z", value)
		if err != nil {
			return "", false
		}
		return t.In(loc).Format(time.DateOnly), true
	}
	if tz := paramValue(params, "TZID"); tz != "" && len(value) >= 15 {
		if l, err := time.LoadLocation(tz); err == nil {
			if t, err := time.ParseInLocation("20060102T150405", value[:15], l); err == nil {
				return t.Format(time.DateOnly), true
			}
		}
	}
	t, err := time.Parse("20060102", value[:8])
	if err != nil {
		return "", false
	}
	return t.Format(time.DateOnly), true
}

func addDays(d string, n int) string {
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, n).Format(time.DateOnly)
}

// durationDays lee P3D o P1W (sin hora); por defecto un dia.
func durationDays(d string) int {
	var n int
	switch {
	case strings.HasPrefix(d, "P") && strings.HasSuffix(d, "D"):
		if _, err := fmt.Sscanf(d, "P%dD", &n); err == nil && n > 0 {
			return n
		}
	case strings.HasPrefix(d, "P") && strings.HasSuffix(d, "W"):
		if _, err := fmt.Sscanf(d, "P%dW", &n); err == nil && n > 0 {
			return n * 7
		}
	}
	return 1
}

// unfold une las lineas continuadas (empiezan con espacio o tab).
func unfold(body string) []string {
	out := []string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), maxBytes)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(out) > 0 {
			out[len(out)-1] += line[1:]
			continue
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// splitLine separa NOMBRE;PARAMS:VALOR.
func splitLine(line string) (name, params, value string) {
	head, value, ok := strings.Cut(line, ":")
	if !ok {
		return strings.ToUpper(line), "", ""
	}
	if n, p, ok := strings.Cut(head, ";"); ok {
		return strings.ToUpper(n), p, value
	}
	return strings.ToUpper(head), "", value
}

func paramValue(params, key string) string {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(k, key) {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func unescape(v string) string {
	r := strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\\`, `\`)
	return strings.TrimSpace(r.Replace(v))
}
